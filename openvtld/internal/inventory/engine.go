// The inventory engine reconciles four independent observers — mhVTL
// config, the changers (mtx), the media directories, and the daemon
// journal — into one Snapshot, publishing diffs to the event bus and
// persisting history to the store. Since v0.6 the snapshot is
// list-shaped: every collector is per-library except the journal tail
// (vtltape queue ids are globally unique) and the pool stats loop
// (pools are system-wide, libraries sit on top of them).
package inventory

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/openvtl/openvtld/internal/config"
	"github.com/openvtl/openvtld/internal/events"
	"github.com/openvtl/openvtld/internal/store"
)

type Engine struct {
	cfg *config.Config
	bus *events.Bus
	db  *store.Store
	log *slog.Logger

	mu   sync.RWMutex
	snap Snapshot

	// runCtx is the daemon-lifetime context from Start — collector
	// goroutines launched later (Reload) must not inherit a request
	// context.
	runCtx context.Context

	// journal activity accumulators (queueID -> delta since last flush)
	actMu  sync.Mutex
	writes map[int]int64
	reads  map[int]int64
}

func New(cfg *config.Config, bus *events.Bus, db *store.Store, log *slog.Logger) *Engine {
	return &Engine{
		cfg: cfg, bus: bus, db: db, log: log,
		writes: map[int]int64{}, reads: map[int]int64{},
	}
}

// Snapshot returns a deep-enough copy of current state. Every slice is
// non-nil so the API marshals [] rather than null — a pending library
// has never been polled and its nil slots crashed the UI once.
func (e *Engine) Snapshot() Snapshot {
	e.mu.RLock()
	defer e.mu.RUnlock()
	s := e.snap
	s.Libraries = make([]LibrarySnapshot, len(e.snap.Libraries))
	for i, l := range e.snap.Libraries {
		l.Drives = append([]Drive{}, l.Drives...)
		l.Slots = append([]Slot{}, l.Slots...)
		l.Carts = append([]Cart{}, l.Carts...)
		s.Libraries[i] = l
	}
	s.Pools = append([]PoolStats{}, e.snap.Pools...)
	return s
}

// SetFCState is called by the orchestrator after target verification.
// VerifiedAt is stamped here — the one choke point — because no caller
// sets it (found in the field as verified=true alongside a zero
// verified_at in status.json).
func (e *Engine) SetFCState(fc FCState) {
	if fc.Verified && fc.VerifiedAt.IsZero() {
		fc.VerifiedAt = time.Now().UTC()
	}
	e.mu.Lock()
	e.snap.FC = fc
	e.snap.UpdatedAt = time.Now().UTC()
	e.mu.Unlock()
	e.bus.Publish("fc_state", fc.TargetWWN, map[string]any{"verified": fc.Verified, "detail": fc.Detail})
}

// ChangerFor returns a library's changer sg node.
func (e *Engine) ChangerFor(libID int) (string, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	for _, l := range e.snap.Libraries {
		if l.Library.ID == libID && l.Library.ChangerSG != "" {
			return l.Library.ChangerSG, true
		}
	}
	return "", false
}

// MediaDirFor returns the media root holding a cart (its library's
// home directory).
func (e *Engine) MediaDirFor(label string) (string, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	for _, l := range e.snap.Libraries {
		for _, c := range l.Carts {
			if c.Label == label {
				return l.Library.HomeDir, true
			}
		}
	}
	return "", false
}

// Start performs initial discovery and launches all collectors.
func (e *Engine) Start(ctx context.Context) error {
	e.runCtx = ctx
	libs, err := ParseMhvtlConf(e.cfg.MhvtlConf)
	if err != nil {
		return fmt.Errorf("mhvtl config: %w", err)
	}
	devs, err := DiscoverSG(ctx)
	if err != nil {
		return fmt.Errorf("sg discovery: %w", err)
	}

	e.mu.Lock()
	live := 0
	for _, lib := range libs {
		home := lib.HomeDir
		if home == "" {
			home = e.cfg.MediaDir
		}
		ls := LibrarySnapshot{Library: Library{
			ID: lib.ID, Product: lib.Product, Serial: lib.Serial,
			HomeDir: home, NumDrives: len(lib.Drives),
		}}
		// Changer + drives are matched by unit serial (sg nodes renumber
		// on every boot and HBA reprobe — never trust ordering).
		for _, dev := range devs {
			if dev.Type == "mediumx" && SerialMatches(dev.Serial, lib.Serial) {
				ls.Library.ChangerSG, ls.Library.Live = dev.SG, true
			}
		}
		for i, d := range lib.Drives {
			dr := Drive{Index: i, Library: lib.ID, QueueID: d.QueueID,
				Serial: d.Serial, Product: d.Product, Activity: "idle"}
			for _, dev := range devs {
				if dev.Type == "tape" && SerialMatches(dev.Serial, d.Serial) {
					dr.SG, dr.ST = dev.SG, dev.Aux
				}
			}
			ls.Drives = append(ls.Drives, dr)
		}
		if ls.Library.Live {
			live++
		} else {
			e.log.Warn("library declared but not served (pending mhvtl restart?)",
				"library", lib.ID, "serial", lib.Serial)
		}
		e.snap.Libraries = append(e.snap.Libraries, ls)
	}
	e.mu.Unlock()

	// Zero live libraries is degraded, not fatal: the UI must come up
	// so the operator can provision storage and declare libraries
	// (fresh install / storage re-foundation). Reload()/Apply bring
	// collectors online when libraries appear.
	if live == 0 {
		e.log.Warn("no live library — running degraded (wizard available, nothing served)")
	}
	for i, l := range e.Snapshot().Libraries {
		e.log.Info("inventory initialized", "library", l.Library.ID,
			"product", l.Library.Product, "serial", l.Library.Serial,
			"drives", len(l.Drives), "changer", l.Library.ChangerSG, "live", l.Library.Live)
		if l.Library.Live {
			go e.loopChanger(ctx, i)
		}
	}

	go e.loopMedia(ctx)
	go e.loopStats(ctx)
	go e.loopJournal(ctx)
	go e.loopActivityFlush(ctx)
	return nil
}

// Reload re-parses device.conf and re-runs sg discovery, updating
// libraries in place (sg nodes renumber on any mhvtl restart) and
// appending newly declared ones. A library that just became live gets
// its changer loop started. Existing loop goroutines index into
// snap.Libraries — order is preserved (sorted by id, append-only), so
// their indexes stay valid. Called after library creation and after
// the operator-window mhvtl restart.
func (e *Engine) Reload(ctx context.Context) error {
	libs, err := ParseMhvtlConf(e.cfg.MhvtlConf)
	if err != nil {
		return fmt.Errorf("mhvtl config: %w", err)
	}
	devs, err := DiscoverSG(ctx)
	if err != nil {
		return fmt.Errorf("sg discovery: %w", err)
	}

	var startLoops []int
	e.mu.Lock()
	// Prune libraries no longer declared in device.conf BEFORE the
	// add/update pass re-indexes — Reload otherwise only ever adds or
	// updates, so deleting a library (which removes its device.conf
	// block) would leave a phantom entry in the snapshot: it showed up
	// as a ghost "pending" library in the UI with device.conf and the
	// DB both empty (2026-07-05). Pruning first keeps startLoops indices
	// valid against the final slice.
	declared := map[int]bool{}
	for _, lib := range libs {
		declared[lib.ID] = true
	}
	kept := make([]LibrarySnapshot, 0, len(e.snap.Libraries))
	for _, ls := range e.snap.Libraries {
		if declared[ls.Library.ID] {
			kept = append(kept, ls)
		}
	}
	e.snap.Libraries = kept
	for _, lib := range libs {
		home := lib.HomeDir
		if home == "" {
			home = e.cfg.MediaDir
		}
		// find existing entry
		li := -1
		for i := range e.snap.Libraries {
			if e.snap.Libraries[i].Library.ID == lib.ID {
				li = i
			}
		}
		if li == -1 {
			e.snap.Libraries = append(e.snap.Libraries, LibrarySnapshot{Library: Library{
				ID: lib.ID, Product: lib.Product, Serial: lib.Serial,
				HomeDir: home, NumDrives: len(lib.Drives),
			}})
			li = len(e.snap.Libraries) - 1
		}
		ls := &e.snap.Libraries[li]
		wasLive := ls.Library.Live
		ls.Library.Product, ls.Library.Serial, ls.Library.HomeDir = lib.Product, lib.Serial, home
		ls.Library.NumDrives = len(lib.Drives)
		ls.Library.ChangerSG, ls.Library.Live = "", false
		for _, dev := range devs {
			if dev.Type == "mediumx" && SerialMatches(dev.Serial, lib.Serial) {
				ls.Library.ChangerSG, ls.Library.Live = dev.SG, true
			}
		}
		for i, d := range lib.Drives {
			var dr *Drive
			if i < len(ls.Drives) {
				dr = &ls.Drives[i]
			} else {
				ls.Drives = append(ls.Drives, Drive{Index: i, Library: lib.ID, Activity: "idle"})
				dr = &ls.Drives[len(ls.Drives)-1]
			}
			dr.QueueID, dr.Serial, dr.Product = d.QueueID, d.Serial, d.Product
			dr.SG, dr.ST = "", ""
			for _, dev := range devs {
				if dev.Type == "tape" && SerialMatches(dev.Serial, d.Serial) {
					dr.SG, dr.ST = dev.SG, dev.Aux
				}
			}
		}
		if !wasLive && ls.Library.Live {
			startLoops = append(startLoops, li)
		}
	}
	e.snap.UpdatedAt = time.Now().UTC()
	e.mu.Unlock()

	// Collectors only when the engine is running (Start sets runCtx);
	// a bare CreateLibrary in tests reloads state without loops.
	if e.runCtx != nil {
		for _, li := range startLoops {
			e.log.Info("library now live — starting changer poll", "library", e.libID(li))
			go e.loopChanger(e.runCtx, li)
		}
	}
	return nil
}

func (e *Engine) loopChanger(ctx context.Context, li int) {
	t := time.NewTicker(e.cfg.PollChanger)
	defer t.Stop()
	for {
		if st, err := PollChanger(ctx, e.changerSG(li)); err == nil {
			e.applyMtx(ctx, li, st)
		} else if ctx.Err() == nil {
			e.log.Warn("changer poll failed", "library", e.libID(li), "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (e *Engine) changerSG(li int) string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.snap.Libraries[li].Library.ChangerSG
}

func (e *Engine) libID(li int) int {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.snap.Libraries[li].Library.ID
}

func (e *Engine) applyMtx(ctx context.Context, li int, st *MtxStatus) {
	type moveEvt struct{ kind, subject, detail string }
	var evts []moveEvt

	e.mu.Lock()
	lib := &e.snap.Libraries[li]
	// Drives
	for _, md := range st.Drives {
		if md.Num >= len(lib.Drives) {
			continue
		}
		d := &lib.Drives[md.Num]
		if d.Loaded != md.Label {
			if md.Label != "" {
				evts = append(evts, moveEvt{"drive_loaded", md.Label, fmt.Sprintf("library %d drive %d loaded %s", lib.Library.ID, md.Num, md.Label)})
			} else if d.Loaded != "" {
				evts = append(evts, moveEvt{"drive_unloaded", d.Loaded, fmt.Sprintf("library %d drive %d unloaded %s", lib.Library.ID, md.Num, d.Loaded)})
			}
			d.Loaded = md.Label
		}
		d.SourceSlot = md.Source
	}
	// Slots: rebuild + diff cart locations
	oldLoc := map[string]string{}
	for _, c := range lib.Carts {
		oldLoc[c.Label] = c.Location
	}
	lib.Slots = lib.Slots[:0]
	nStorage, nIE := 0, 0
	loc := map[string]string{}
	for _, s := range st.Slots {
		kind := "storage"
		if s.IE {
			kind = "ie"
			nIE++
		} else {
			nStorage++
		}
		lib.Slots = append(lib.Slots, Slot{Kind: kind, Num: s.Num, Label: s.Label})
		if s.Label != "" {
			loc[s.Label] = fmt.Sprintf("%s:%d", kind, s.Num)
		}
	}
	for i, d := range lib.Drives {
		if d.Loaded != "" {
			loc[d.Loaded] = fmt.Sprintf("drive:%d", i)
		}
	}
	lib.Library.NumSlots, lib.Library.NumIE = nStorage, nIE
	// Update cart locations in place
	for i := range lib.Carts {
		c := &lib.Carts[i]
		newLoc, ok := loc[c.Label]
		if !ok {
			newLoc = "missing"
		}
		if old := oldLoc[c.Label]; old != newLoc && old != "" {
			evts = append(evts, moveEvt{"cart_moved", c.Label, fmt.Sprintf("%s -> %s", old, newLoc)})
		}
		c.Location = newLoc
	}
	e.snap.UpdatedAt = time.Now().UTC()
	e.mu.Unlock()

	for _, ev := range evts {
		e.bus.Publish(ev.kind, ev.subject, map[string]any{"detail": ev.detail})
		if err := e.db.LogEvent(ctx, time.Now(), ev.kind, ev.subject, ev.detail); err != nil {
			e.log.Warn("event log write failed", "err", err)
		}
	}
}

func (e *Engine) loopMedia(ctx context.Context) {
	t := time.NewTicker(e.cfg.ScanMedia)
	defer t.Stop()
	for {
		for li := range e.Snapshot().Libraries {
			dir := e.mediaDir(li)
			if media, err := ScanMedia(dir); err == nil {
				e.applyMedia(ctx, li, media)
			} else if ctx.Err() == nil {
				e.log.Warn("media scan failed", "dir", dir, "err", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (e *Engine) mediaDir(li int) string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.snap.Libraries[li].Library.HomeDir
}

func (e *Engine) applyMedia(ctx context.Context, li int, media []MediaInfo) {
	e.mu.Lock()
	lib := &e.snap.Libraries[li]
	byLabel := map[string]int{}
	for i, c := range lib.Carts {
		byLabel[c.Label] = i
	}
	for _, m := range media {
		if i, ok := byLabel[m.Label]; ok {
			lib.Carts[i].SizeBytes = m.SizeBytes
			lib.Carts[i].PhysBytes = m.PhysBytes
			lib.Carts[i].Modified = m.Modified
		} else {
			lib.Carts = append(lib.Carts, Cart{
				Label: m.Label, Library: lib.Library.ID,
				SizeBytes: m.SizeBytes, PhysBytes: m.PhysBytes, Modified: m.Modified, Location: "missing",
			})
		}
	}
	e.snap.UpdatedAt = time.Now().UTC()
	carts := append([]Cart(nil), lib.Carts...)
	e.mu.Unlock()

	for _, c := range carts {
		if err := e.db.UpsertCartridge(ctx, c.Label, c.SizeBytes, c.Location, &c.Modified, c.Library); err != nil {
			e.log.Warn("cart upsert failed", "label", c.Label, "err", err)
		}
	}
}

// Pool samples persist every persistEvery so the dashboard capacity
// trend survives restarts (dedupe_stats, v0.5); unchanged samples are
// skipped, history past sampleRetention is pruned daily.
const (
	persistEvery    = 5 * time.Minute
	sampleRetention = 90 * 24 * time.Hour
)

// poolTargets resolves what to collect stats for: the pool table when
// populated (v0.6 UI-provisioned pools), else the legacy flag-configured
// single pool so a pre-rebuild system keeps its dashboard.
type poolTarget struct {
	name, mount, vdoLV, dataLV string
}

func (e *Engine) poolTargets(ctx context.Context) []poolTarget {
	if pools, err := e.db.ListPools(ctx); err == nil && len(pools) > 0 {
		var out []poolTarget
		for _, p := range pools {
			if p.State != store.PoolActive {
				continue
			}
			out = append(out, poolTarget{
				name: p.Name, mount: p.Mountpoint,
				vdoLV: p.VG + "/" + p.DataLV, dataLV: p.DataLV,
			})
		}
		if len(out) > 0 {
			return out
		}
	}
	if e.cfg.VDOLV == "" {
		return nil // no pools yet (fresh install) and no legacy flag — nothing to poll
	}
	dataLV := e.cfg.VDOLV
	if i := strings.IndexByte(dataLV, '/'); i >= 0 {
		dataLV = dataLV[i+1:]
	}
	return []poolTarget{{name: e.cfg.VDOLV, mount: e.cfg.MediaDir, vdoLV: e.cfg.VDOLV, dataLV: dataLV}}
}

func (e *Engine) loopStats(ctx context.Context) {
	t := time.NewTicker(e.cfg.PollStats)
	defer t.Stop()
	var lastPersist, lastPrune time.Time
	for {
		targets := e.poolTargets(ctx)
		var pools []PoolStats
		for _, pt := range targets {
			ps, err := CollectPoolStats(ctx, pt.mount, pt.vdoLV, pt.dataLV)
			if err != nil {
				continue
			}
			ps.Name, ps.Mountpoint = pt.name, pt.mount
			pools = append(pools, ps)
			e.bus.Publish("pool_stats", pt.name, map[string]any{
				"fs_used":    ps.FSUsedBytes,
				"phys_est":   ps.PhysEstBytes,
				"vdo_phys":   ps.VDOPhysBytes,
				"logical":    ps.LogicalBytes,
				"saving_pct": ps.VDOSavingPct,
				"cache_pct":  ps.CacheUsedPct,
			})
		}
		// Refresh the snapshot when we have fresh stats OR when there are
		// genuinely no pools (so a removed pool's ghost is cleared — else
		// the header keeps counting a pool that's gone from the DB and ZFS).
		// Skip only the case where targets existed but every collection
		// failed: a transient zfs hiccup must not wipe the dashboard.
		if pools != nil || len(targets) == 0 {
			e.mu.Lock()
			e.snap.Pools = pools
			e.snap.UpdatedAt = time.Now().UTC()
			e.mu.Unlock()
			if time.Since(lastPersist) >= persistEvery {
				for _, ps := range pools {
					e.persistPoolSample(ctx, ps)
				}
				lastPersist = time.Now()
			}
			if time.Since(lastPrune) >= 24*time.Hour {
				if err := e.db.PrunePoolSamples(ctx, time.Now().Add(-sampleRetention)); err != nil {
					e.log.Warn("pool sample prune", "err", err)
				}
				lastPrune = time.Now()
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// persistPoolSample stores a capacity-trend point. The legacy
// vdo_used_bytes column carries PhysEstBytes since the v0.9 fix — the
// trend's "physical" line must include global dedup savings (dataset
// `used` exceeded the pool size at 9× dedup); samples written before
// the fix hold the old inflated value until they age out (7 days).
func (e *Engine) persistPoolSample(ctx context.Context, ps PoolStats) {
	if last, err := e.db.LastPoolSample(ctx, ps.Name); err == nil &&
		last.FSUsedBytes == ps.FSUsedBytes && last.VDOUsedBytes == ps.PhysEstBytes &&
		last.LogicalBytes == ps.LogicalBytes {
		return // nothing moved — don't grow the series
	}
	err := e.db.AddPoolSample(ctx, store.PoolSample{
		TS:          time.Now().UTC().Format(time.RFC3339),
		Pool:        ps.Name,
		FSUsedBytes: ps.FSUsedBytes, FSTotalBytes: ps.FSTotalBytes,
		VDOUsedBytes: ps.PhysEstBytes, VDOPhysBytes: ps.VDOPhysBytes,
		LogicalBytes: ps.LogicalBytes,
		VDOSavingPct: int64(ps.VDOSavingPct), CacheUsedPct: ps.CacheUsedPct,
	})
	if err != nil {
		e.log.Warn("pool sample persist", "err", err)
	}
}

func (e *Engine) loopJournal(ctx context.Context) {
	ch := make(chan JournalEvent, 1024)
	go func() {
		for ctx.Err() == nil {
			if err := TailJournal(ctx, e.cfg.JournalSince, ch, e.log); err != nil && ctx.Err() == nil {
				e.log.Warn("journal tail restarting", "err", err)
				time.Sleep(2 * time.Second)
			}
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-ch:
			switch ev.Kind {
			case "write":
				e.actMu.Lock()
				e.writes[ev.QueueID]++
				e.actMu.Unlock()
			case "read":
				e.actMu.Lock()
				e.reads[ev.QueueID]++
				e.actMu.Unlock()
			case "load", "unload", "move", "filemarks", "pr":
				e.bus.Publish("mhvtl_"+ev.Kind, fmt.Sprintf("queue:%d", ev.QueueID), map[string]any{"detail": ev.Detail})
				_ = e.db.LogEvent(ctx, ev.TS, "mhvtl_"+ev.Kind, fmt.Sprintf("queue:%d", ev.QueueID), ev.Detail)
			}
		}
	}
}

// loopActivityFlush aggregates the write/read firehose into 2s
// drive_activity events and idle detection. vtltape queue ids are
// globally unique, so one pass covers every library's drives.
func (e *Engine) loopActivityFlush(ctx context.Context) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		e.actMu.Lock()
		w := e.writes
		r := e.reads
		e.writes = map[int]int64{}
		e.reads = map[int]int64{}
		e.actMu.Unlock()

		now := time.Now().UTC()
		e.mu.Lock()
		for li := range e.snap.Libraries {
			for i := range e.snap.Libraries[li].Drives {
				d := &e.snap.Libraries[li].Drives[i]
				dw, dr := w[d.QueueID], r[d.QueueID]
				if dw > 0 || dr > 0 {
					d.BlocksWritten += dw
					d.BlocksRead += dr
					d.LastActive = now
					if dw >= dr {
						d.Activity = "writing"
					} else {
						d.Activity = "reading"
					}
					e.bus.Publish("drive_activity", fmt.Sprintf("drive:%d:%d", d.Library, d.Index), map[string]any{
						"library": d.Library, "writes_delta": dw, "reads_delta": dr,
						"blocks_written": d.BlocksWritten, "blocks_read": d.BlocksRead,
						"loaded": d.Loaded,
					})
				} else if d.Activity != "idle" && now.Sub(d.LastActive) > 5*time.Second {
					d.Activity = "idle"
					e.bus.Publish("drive_activity", fmt.Sprintf("drive:%d:%d", d.Library, d.Index), map[string]any{
						"library": d.Library, "idle": true,
						"blocks_written": d.BlocksWritten, "blocks_read": d.BlocksRead,
					})
				}
			}
		}
		e.snap.UpdatedAt = now
		e.mu.Unlock()
	}
}
