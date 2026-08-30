package export

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Settings keys (settings table, editable from the UI):
//
//	export.ie_watcher     "on" to auto-export carts appearing in IE
//	                      elements (BRMS vault moves). Default off —
//	                      unverifiable without a licensed LPAR, so the
//	                      manual Export-now action ships in front of it.
//	export.default_remote s3_remote id used by watchers.
//	evict.threshold_pct   zpool physical fullness (%) that triggers policy
//	                      eviction of exported carts. Default "" = never.
//	minting.enabled       reserved: spare-cart minting, per-site, DEFAULT
//	                      OFF (settled 2026-07-02). Mechanics need a
//	                      library reconfig story; lands with BRMS
//	                      validation.
const (
	SettingIEWatcher     = "export.ie_watcher"
	SettingDefaultRemote = "export.default_remote"
	SettingEvictPct      = "evict.threshold_pct"
	SettingMinting       = "minting.enabled"
)

// StartWatchers launches the IE vault watcher and the pool-pressure
// evictor. Both are settings-gated and default off; both only ever
// enqueue jobs — the runner's state machines own all safety checks.
func (r *Runner) StartWatchers(ctx context.Context) {
	go r.watchIE(ctx)
	go r.watchPool(ctx)
}

// SuppressIE exempts a label from the IE watcher while it transits the
// MAP for reasons that are not a vault move (cart minting). TTL-capped
// so a crashed mint can't silence the watcher forever.
func (r *Runner) SuppressIE(label string) {
	r.suppressMu.Lock()
	defer r.suppressMu.Unlock()
	if r.suppressIE == nil {
		r.suppressIE = map[string]time.Time{}
	}
	r.suppressIE[label] = time.Now().Add(10 * time.Minute)
}

func (r *Runner) ReleaseIE(label string) {
	r.suppressMu.Lock()
	defer r.suppressMu.Unlock()
	delete(r.suppressIE, label)
}

func (r *Runner) ieSuppressed(label string) bool {
	r.suppressMu.Lock()
	defer r.suppressMu.Unlock()
	until, ok := r.suppressIE[label]
	if ok && time.Now().After(until) {
		delete(r.suppressIE, label)
		return false
	}
	return ok
}

func (r *Runner) defaultRemote(ctx context.Context) (int64, bool) {
	v := r.db.Setting(ctx, SettingDefaultRemote, "")
	id, err := strconv.ParseInt(v, 10, 64)
	return id, err == nil && id > 0
}

// hasOpenJob reports whether a cart has any job in flight.
func (r *Runner) hasOpenJob(ctx context.Context, label string) bool {
	jobs, err := r.db.UnfinishedJobs(ctx)
	if err != nil {
		return true // fail safe: don't double-enqueue on DB trouble
	}
	for _, j := range jobs {
		if j.CartLabel == label {
			return true
		}
	}
	return false
}

// watchIE fires an export when a cart shows up in an import/export
// element — that's what a BRMS "move to vault" looks like from inside
// the library. Runs behind the export.ie_watcher toggle.
func (r *Runner) watchIE(ctx context.Context) {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if r.db.Setting(ctx, SettingIEWatcher, "off") != "on" {
			continue
		}
		remoteID, ok := r.defaultRemote(ctx)
		if !ok {
			continue
		}
		for _, lib := range r.inv.Snapshot().Libraries {
			for _, s := range lib.Slots {
				if s.Kind != "ie" || s.Label == "" || r.hasOpenJob(ctx, s.Label) || r.ieSuppressed(s.Label) {
					continue
				}
				j, err := r.db.CreateJob(ctx, "export", s.Label, &remoteID, "", "detected", "ie-watcher")
				if err != nil {
					r.log.Error("ie-watcher job create", "cart", s.Label, "err", err)
					continue
				}
				r.log.Info("ie-watcher: cart in IE element, exporting",
					"library", lib.Library.ID, "cart", s.Label, "job", j.ID)
				r.db.LogEvent(ctx, time.Now(), "vault_detected", s.Label,
					fmt.Sprintf("cart in library %d IE element %d — export job %d created", lib.Library.ID, s.Num, j.ID))
				r.publish(ctx, j.ID)
				r.Enqueue(j.ID)
			}
		}
	}
}

// watchPool evicts already-exported carts (oldest export first) when
// zpool physical usage crosses the configured threshold — the pressure
// valve. One evict in flight at a time.
func (r *Runner) watchPool(ctx context.Context) {
	t := time.NewTicker(60 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		pctStr := r.db.Setting(ctx, SettingEvictPct, "")
		if pctStr == "" {
			continue
		}
		threshold, err := strconv.ParseFloat(pctStr, 64)
		if err != nil || threshold <= 0 {
			continue
		}
		snap := r.inv.Snapshot()
		for _, pool := range snap.Pools {
			// Pool pressure = the shared zpool's REAL fullness. Dataset-
			// level numbers don't see global dedup: the old used/statfs
			// formula read ~64% on an 18%-full pool at 9× dedup and would
			// have fired eviction absurdly early (v0.9 fix).
			if pool.ZpoolSizeBytes == 0 {
				continue
			}
			usedPct := float64(pool.ZpoolAllocBytes) * 100 / float64(pool.ZpoolSizeBytes)
			if usedPct < threshold {
				continue
			}
			remoteID, ok := r.defaultRemote(ctx)
			if !ok {
				continue
			}
			metas, err := r.db.CartMetas(ctx)
			if err != nil {
				continue
			}
			// Oldest export generation first; skip loaded/evicted/busy
			// carts. Only carts living on THIS pool relieve its
			// pressure — a cart's pool is its library's home pool
			// (library home dir sits on the pool mountpoint).
			var pick, pickGen string
			for _, lib := range snap.Libraries {
				if !strings.HasPrefix(lib.Library.HomeDir, pool.Mountpoint) {
					continue
				}
				for _, c := range lib.Carts {
					m, ok := metas[c.Label]
					if !ok || m.LocalState != "resident" || m.LastExportGen == "" {
						continue
					}
					if strings.HasPrefix(c.Location, "drive:") || r.hasOpenJob(ctx, c.Label) {
						continue
					}
					if pick == "" || m.LastExportGen < pickGen {
						pick, pickGen = c.Label, m.LastExportGen
					}
				}
			}
			if pick == "" {
				r.log.Warn("pool over evict threshold but no evictable cart",
					"pool", pool.Name, "used_pct", fmt.Sprintf("%.1f", usedPct))
				continue
			}
			j, err := r.db.CreateJob(ctx, "evict", pick, &remoteID, pickGen, "queued", "policy")
			if err != nil {
				r.log.Error("policy evict job create", "cart", pick, "err", err)
				continue
			}
			r.db.LogEvent(ctx, time.Now(), "policy_evict", pick,
				fmt.Sprintf("pool %s at %.1f%% >= %.0f%% threshold — evict job %d (generation %s)", pool.Name, usedPct, threshold, j.ID, pickGen))
			r.publish(ctx, j.ID)
			r.Enqueue(j.ID)
		}
	}
}
