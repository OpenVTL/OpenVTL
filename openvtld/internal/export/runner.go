// Package export runs the v0.4 job state machines: export a quiesced
// cartridge to S3 (chunked+zstd, manifest last), import it back
// byte-identical, and evict local data to a labelled stub. One job runs
// at a time — the pool is a shared data plane and WAN upload dominates
// anyway. Every state transition is persisted (job_event) and published
// on the bus for SSE.
package export

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/openvtl/openvtld/internal/catalog"
	"github.com/openvtl/openvtld/internal/events"
	"github.com/openvtl/openvtld/internal/inventory"
	"github.com/openvtl/openvtld/internal/store"
)

type Runner struct {
	db  *store.Store
	bus *events.Bus
	inv *inventory.Engine
	log *slog.Logger

	mediaDir   string
	stagingDir string
	chunkBytes int64
	hostname   string
	version    string
	librarySN  string

	queue   chan int64
	mu      sync.Mutex
	cancels map[int64]context.CancelFunc
	current int64 // job id being processed, 0 if idle

	// IE-watcher suppression for carts transiting the MAP on non-vault
	// business (minting) — see watchers.go.
	suppressMu sync.Mutex
	suppressIE map[string]time.Time
}

type Options struct {
	MediaDir   string
	StagingDir string
	ChunkBytes int64
	Version    string
	LibrarySN  string
}

func NewRunner(db *store.Store, bus *events.Bus, inv *inventory.Engine, log *slog.Logger, o Options) *Runner {
	host, _ := os.Hostname()
	return &Runner{
		db: db, bus: bus, inv: inv, log: log,
		mediaDir: o.MediaDir, stagingDir: o.StagingDir, chunkBytes: o.ChunkBytes,
		hostname: host, version: o.Version, librarySN: o.LibrarySN,
		queue: make(chan int64, 64), cancels: map[int64]context.CancelFunc{},
	}
}

// Start prepares staging, resumes unfinished jobs, and launches the
// worker. Interrupted jobs pick up from their chunk ledger.
func (r *Runner) Start(ctx context.Context) error {
	if err := os.MkdirAll(r.stagingDir, 0o755); err != nil {
		return fmt.Errorf("staging dir: %w", err)
	}
	// Leftover staging files from a crash are garbage — chunks are
	// re-produced from the ledger, never from stage files.
	if ents, err := os.ReadDir(r.stagingDir); err == nil {
		for _, e := range ents {
			if strings.HasPrefix(e.Name(), "chunk-") || strings.HasPrefix(e.Name(), "import-") {
				os.RemoveAll(filepath.Join(r.stagingDir, e.Name()))
			}
		}
	}
	unfinished, err := r.db.UnfinishedJobs(ctx)
	if err != nil {
		return fmt.Errorf("resume scan: %w", err)
	}
	for _, j := range unfinished {
		r.log.Info("resuming job", "id", j.ID, "kind", j.Kind, "cart", j.CartLabel, "state", j.State)
		r.queue <- j.ID
	}
	go r.work(ctx)
	return nil
}

// Enqueue schedules a job for the worker.
func (r *Runner) Enqueue(id int64) error {
	select {
	case r.queue <- id:
		return nil
	default:
		return fmt.Errorf("job queue full")
	}
}

// Cancel aborts a running job or marks a queued one cancelled.
func (r *Runner) Cancel(ctx context.Context, id int64) error {
	r.mu.Lock()
	cancel, running := r.cancels[id]
	r.mu.Unlock()
	if running {
		cancel()
		return nil
	}
	j, err := r.db.GetJob(ctx, id)
	if err != nil {
		return err
	}
	if j.State == "done" || j.State == "failed" || j.State == "cancelled" {
		return fmt.Errorf("job %d already %s", id, j.State)
	}
	return r.transition(ctx, j, "cancelled", "cancelled before start")
}

func (r *Runner) work(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case id := <-r.queue:
			r.run(ctx, id)
		}
	}
}

func (r *Runner) run(ctx context.Context, id int64) {
	j, err := r.db.GetJob(ctx, id)
	if err != nil {
		r.log.Error("job load failed", "id", id, "err", err)
		return
	}
	if j.State == "done" || j.State == "failed" || j.State == "cancelled" {
		return // cancelled while queued, or duplicate enqueue
	}
	jctx, cancel := context.WithCancel(ctx)
	r.mu.Lock()
	r.cancels[id] = cancel
	r.current = id
	r.mu.Unlock()
	defer func() {
		cancel()
		r.mu.Lock()
		delete(r.cancels, id)
		r.current = 0
		r.mu.Unlock()
	}()

	switch j.Kind {
	case "export":
		err = r.runExport(jctx, j)
	case "import":
		err = r.runImport(jctx, j)
	case "evict":
		err = r.runEvict(jctx, j)
	default:
		err = fmt.Errorf("unknown job kind %q", j.Kind)
	}
	if err != nil {
		state := "failed"
		detail := err.Error()
		if jctx.Err() != nil {
			state = "cancelled"
			detail = "cancelled by operator"
		}
		// Reload for the current state so the transition edge is honest.
		if cur, gerr := r.db.GetJob(ctx, id); gerr == nil {
			j = cur
		}
		r.db.SetJobError(ctx, id, detail)
		if terr := r.transition(ctx, j, state, detail); terr != nil {
			r.log.Error("failure transition", "id", id, "err", terr)
		}
		r.db.LogEvent(ctx, time.Now(), "job_"+state, j.CartLabel,
			fmt.Sprintf("%s job %d: %s", j.Kind, id, detail))
		r.log.Warn("job ended", "id", id, "kind", j.Kind, "state", state, "err", detail)
	}
}

// transition persists a state change, refreshes the caller's copy, and
// publishes the full job to SSE subscribers.
func (r *Runner) transition(ctx context.Context, j *store.Job, to, detail string) error {
	if err := r.db.TransitionJob(ctx, j.ID, j.State, to, detail); err != nil {
		return fmt.Errorf("transition %s->%s: %w", j.State, to, err)
	}
	j.State = to
	r.publish(ctx, j.ID)
	return nil
}

// publish pushes the current job row onto the bus (kind job_update).
func (r *Runner) publish(ctx context.Context, id int64) {
	j, err := r.db.GetJob(ctx, id)
	if err != nil {
		return
	}
	b, _ := json.Marshal(j)
	var m map[string]any
	json.Unmarshal(b, &m)
	r.bus.Publish("job_update", fmt.Sprintf("job:%d", id), m)
}

func (r *Runner) progress(ctx context.Context, id int64, bytesDone int64, chunksDone int) {
	if err := r.db.SetJobProgress(ctx, id, bytesDone, chunksDone); err != nil {
		r.log.Warn("progress write", "id", id, "err", err)
	}
	r.publish(ctx, id)
}

// cartLocation reads the live location of a cart from the inventory.
func (r *Runner) cartLocation(label string) (string, bool) {
	c, _, ok := r.inv.Snapshot().FindCart(label)
	return c.Location, ok
}

// mediaDirFor resolves the media root holding a cart — its library's
// home directory since v0.6. The flag-configured dir is the fallback
// for carts the inventory doesn't know yet.
func (r *Runner) mediaDirFor(label string) string {
	if dir, ok := r.inv.MediaDirFor(label); ok && dir != "" {
		return dir
	}
	return r.mediaDir
}

// libSerialFor stamps manifests with the owning library's serial — also
// the <library> segment of the S3 key (immutable, unlike the friendly
// library name).
func (r *Runner) libSerialFor(label string) string {
	if _, lib, ok := r.inv.Snapshot().FindCart(label); ok && lib.Library.Serial != "" {
		return lib.Library.Serial
	}
	return r.librarySN
}

// systemIdentity resolves this instance's friendly name (the <system> S3
// key segment) and stable UUID, defaulting the name to the sanitized
// hostname and generating the UUID on first use.
func (r *Runner) systemIdentity(ctx context.Context) (name, uuid string) {
	name, uuid, err := r.db.SystemIdentity(ctx, catalog.SanitizeName(r.hostname))
	if err != nil {
		r.log.Warn("system identity", "err", err)
		return catalog.SanitizeName(r.hostname), ""
	}
	return name, uuid
}

// quiesceCheck enforces the one data-plane rule: never touch a cart
// that is (or might be) in a drive. Also requires the media files to
// have been stable for a few seconds.
func (r *Runner) quiesceCheck(label string) error {
	loc, ok := r.cartLocation(label)
	if !ok {
		return fmt.Errorf("cart %s not known to inventory", label)
	}
	if strings.HasPrefix(loc, "drive:") {
		return fmt.Errorf("cart %s is loaded in %s — unload from the host first", label, loc)
	}
	if loc == "missing" {
		return fmt.Errorf("cart %s has media but no library slot", label)
	}
	dir := filepath.Join(r.mediaDirFor(label), label)
	files, err := listCartFiles(dir)
	if err != nil {
		return err
	}
	for _, fi := range files {
		if time.Since(fi.ModTime()) < 10*time.Second {
			return fmt.Errorf("cart %s media still changing (%s modified %s ago)",
				label, fi.Name(), time.Since(fi.ModTime()).Round(time.Second))
		}
	}
	return nil
}

// unvault returns a cart sitting in an IE element to the first empty
// storage slot of its own library; no-op when the cart is already in
// storage.
func (r *Runner) unvault(ctx context.Context, label string) (string, error) {
	_, lib, ok := r.inv.Snapshot().FindCart(label)
	if !ok {
		return "", fmt.Errorf("cart %s not known to inventory", label)
	}
	var from, to int
	for _, s := range lib.Slots {
		if s.Kind == "ie" && s.Label == label {
			from = s.Num
		}
		if s.Kind == "storage" && s.Label == "" && to == 0 {
			to = s.Num
		}
	}
	if from == 0 {
		return "cart already in storage; no unvault needed", nil
	}
	if to == 0 {
		return "", fmt.Errorf("no empty storage slot to unvault %s into", label)
	}
	if err := inventory.MoveCart(ctx, lib.Library.ChangerSG, from, to); err != nil {
		return "", err
	}
	return fmt.Sprintf("moved %s: ie element %d -> storage slot %d", label, from, to), nil
}
