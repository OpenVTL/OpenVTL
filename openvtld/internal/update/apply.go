package update

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/openvtl/openvtld/internal/release"
)

// DefaultMhvtlPinFile is where install.sh records the installed mhVTL pin.
const DefaultMhvtlPinFile = "/usr/src/openvtl-mhvtl/.openvtl-pin"

// Preflight refusals — the CLI maps these to specific guidance + exit codes.
var (
	ErrSameVersion = errors.New("bundle is the same version as the running binary — nothing to update")
	ErrDowngrade   = errors.New("bundle is older than the running binary — use `openvtld rollback`, not update")
	ErrTierBC      = errors.New("bundle changes mhVTL (Tier B) — run install.sh in a maintenance window, not the automated updater")
	ErrMidJob      = errors.New("a job is in flight — retry when idle or pass --force")
)

// Options configure an Apply/Rollback run.
type Options struct {
	Paths          Paths
	CurrentVersion string // running binary's version (the "from")
	CurrentBuild   string // running binary's buildDate (RFC3339), "" if unknown
	MhvtlPinFile   string // default DefaultMhvtlPinFile
	Force          bool
	Deadline       time.Duration                 // confirm/health deadline; default 90s
	AttemptLimit   int                           // watchdog crash-restart budget; default 3
	Log            func(format string, a ...any) // progress sink
}

func (o *Options) def() {
	if o.MhvtlPinFile == "" {
		o.MhvtlPinFile = DefaultMhvtlPinFile
	}
	if o.Deadline == 0 {
		o.Deadline = 90 * time.Second
	}
	if o.AttemptLimit == 0 {
		o.AttemptLimit = 3
	}
	if o.Log == nil {
		o.Log = func(string, ...any) {}
	}
}

// Apply runs the full Tier-A update flow for a bundle tarball.
func Apply(ctx context.Context, tarball string, opt Options) error {
	opt.def()
	p := opt.Paths

	// 1. Verify --------------------------------------------------------------
	opt.Log("verifying bundle signature")
	tmp, err := os.MkdirTemp("", "ovtl-update-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	bundleRoot, err := unpackBundle(tarball, tmp)
	if err != nil {
		return fmt.Errorf("unpack: %w", err)
	}
	if err := release.VerifyBundleDir(bundleRoot); err != nil {
		return fmt.Errorf("verify: %w", err)
	}
	bv, err := release.ParseVersionDir(bundleRoot)
	if err != nil {
		return fmt.Errorf("read bundle VERSION: %w", err)
	}
	opt.Log("signature OK — bundle is authentic (%s, built %s)", bv.Hash, bv.BuildTime)

	// 2. Preflight -----------------------------------------------------------
	if err := preflight(ctx, p, bv, opt); err != nil {
		return err
	}

	newBin := filepath.Join(bundleRoot, "bin", "openvtld")
	if _, err := os.Stat(newBin); err != nil {
		return fmt.Errorf("bundle has no bin/openvtld: %w", err)
	}

	// 3. Backup --------------------------------------------------------------
	if err := os.MkdirAll(p.BackupsDir, 0o755); err != nil {
		return err
	}
	opt.Log("backing up current binary -> %s", p.PrevBinary)
	if err := copyFile(p.Binary, p.PrevBinary, 0o755); err != nil {
		return fmt.Errorf("backup binary: %w", err)
	}
	ts := time.Now().UTC().Format("20060102T150405Z")
	snap := ""
	if _, err := os.Stat(p.DB); err == nil {
		snap = filepath.Join(p.BackupsDir, fmt.Sprintf("openvtld-%s-%s.db", nz(opt.CurrentVersion), ts))
		opt.Log("snapshotting database -> %s", snap)
		if err := snapshotDB(ctx, p.DB, snap); err != nil {
			return fmt.Errorf("db snapshot: %w", err)
		}
	} else {
		opt.Log("no database at %s yet — skipping snapshot", p.DB)
	}

	// 4. Stage + swap --------------------------------------------------------
	staged := p.Binary + ".new"
	if err := copyFile(newBin, staged, 0o755); err != nil {
		return fmt.Errorf("stage new binary: %w", err)
	}
	// Sanity: the staged binary must run and self-report the bundle's version
	// before it goes live (catches a truncated/incompatible binary the earlier
	// signature check somehow missed).
	if out, err := run(ctx, 15*time.Second, staged, "-version"); err != nil || strings.TrimSpace(out) != bv.Hash {
		os.Remove(staged)
		return fmt.Errorf("staged binary failed self-check (-version=%q, want %q, err=%v)", strings.TrimSpace(out), bv.Hash, err)
	}

	marker := Marker{
		FromVersion:  nz(opt.CurrentVersion),
		ToVersion:    bv.Hash,
		FromBuild:    opt.CurrentBuild,
		ToBuild:      bv.BuildTime,
		Binary:       p.Binary,
		PrevBinary:   p.PrevBinary,
		DBPath:       p.DB,
		DBSnapshot:   snap,
		AttemptLimit: opt.AttemptLimit,
		StagedAt:     time.Now().UTC().Format(time.RFC3339),
		Deadline:     time.Now().Add(opt.Deadline).UTC().Format(time.RFC3339),
	}
	if err := writeMarker(p, marker); err != nil {
		os.Remove(staged)
		return fmt.Errorf("write update marker: %w", err)
	}
	// last-good records the pair to revert to on a future `openvtld rollback`.
	_ = writeLastGood(p, marker)
	opt.Log("swapping binary (atomic)")
	if err := os.Rename(staged, p.Binary); err != nil {
		clearMarker(p)
		return fmt.Errorf("swap binary: %w", err)
	}

	// 5. Restart -------------------------------------------------------------
	opt.Log("restarting %s (control-plane only; tape I/O unaffected)", p.Service)
	if out, err := run(ctx, 60*time.Second, "systemctl", "restart", p.Service); err != nil {
		opt.Log("systemctl restart returned an error (%v) — probing health anyway: %s", err, strings.TrimSpace(out))
	}

	// 6. Confirm or auto-rollback -------------------------------------------
	opt.Log("waiting up to %s for %s to report healthy", opt.Deadline, bv.Hash)
	if _, ok := waitHealthy(ctx, p, bv.Hash, opt.Deadline); ok {
		clearMarker(p)
		opt.Log("CONFIRMED — now running %s", bv.Hash)
		return nil
	}

	// The new binary did not become healthy. Either the systemd watchdog has
	// already reverted (old version is answering), or nothing is — in which case
	// the CLI rolls back itself so it never leaves the box broken.
	if h, ok := probe(ctx, p); ok && h.OK && h.Version == marker.FromVersion {
		clearMarker(p)
		return fmt.Errorf("new binary failed its health check — auto-rolled-back to %s", marker.FromVersion)
	}
	opt.Log("new binary unhealthy and no rollback observed — rolling back now")
	if err := rollbackNow(ctx, p, marker, opt); err != nil {
		return fmt.Errorf("update failed AND rollback failed: %v — MANUAL RECOVERY: restore %s and %s, then restart %s",
			err, marker.PrevBinary, marker.DBSnapshot, p.Service)
	}
	return fmt.Errorf("update failed its health check — rolled back to %s", marker.FromVersion)
}

func preflight(ctx context.Context, p Paths, bv release.Version, opt Options) error {
	if bv.Hash == "" {
		return errors.New("bundle VERSION has no version line")
	}
	if bv.Hash == opt.CurrentVersion && !opt.Force {
		return ErrSameVersion
	}
	if newer, ok := buildNewer(bv.BuildTime, opt.CurrentBuild); ok && !newer && bv.Hash != opt.CurrentVersion {
		// Older build time = downgrade. Not overridable by --force: backward
		// motion is `rollback`, which restores the binary together with its matching DB snapshot.
		return ErrDowngrade
	} else if !ok {
		opt.Log("note: cannot order builds (no embedded buildDate) — skipping downgrade check")
	}
	// Tier B/C: a changed mhVTL pin needs install.sh in a maintenance window.
	// Not overridable — the updater does not do host-disrupting changes.
	if changed, known := mhvtlPinChanged(bv.MhvtlPin, opt.MhvtlPinFile); changed {
		return fmt.Errorf("%w (bundle %s, installed pin at %s)", ErrTierBC, bv.MhvtlPin, opt.MhvtlPinFile)
	} else if !known {
		opt.Log("note: installed mhVTL pin unreadable (%s) — assuming Tier A", opt.MhvtlPinFile)
	}
	// Mid-job: best-effort read-only count of unfinished jobs.
	if n, err := activeJobs(ctx, p.DB); err != nil {
		opt.Log("note: could not check for in-flight jobs (%v) — proceeding", err)
	} else if n > 0 && !opt.Force {
		return fmt.Errorf("%w (%d in flight)", ErrMidJob, n)
	}
	return nil
}

// activeJobs counts unfinished jobs via a read-only connection (WAL lets this
// run alongside the live daemon).
func activeJobs(ctx context.Context, dbPath string) (int, error) {
	if _, err := os.Stat(dbPath); err != nil {
		return 0, nil // no DB, no jobs
	}
	db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro&_pragma=busy_timeout(3000)")
	if err != nil {
		return 0, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var n int
	err = db.QueryRowContext(cctx, `SELECT COUNT(*) FROM job WHERE finished_at IS NULL`).Scan(&n)
	return n, err
}

// rollbackNow restores the previous binary + DB snapshot from a marker and
// restarts, waiting for health.
func rollbackNow(ctx context.Context, p Paths, m Marker, opt Options) error {
	opt.Log("restoring previous binary %s", m.PrevBinary)
	if err := copyFile(m.PrevBinary, p.Binary, 0o755); err != nil {
		return fmt.Errorf("restore binary: %w", err)
	}
	if m.DBSnapshot != "" {
		opt.Log("restoring database snapshot %s", m.DBSnapshot)
		if err := restoreDB(m.DBSnapshot, p.DB); err != nil {
			return fmt.Errorf("restore db: %w", err)
		}
	}
	clearMarker(p)
	opt.Log("restarting %s on the restored binary", p.Service)
	if out, err := run(ctx, 60*time.Second, "systemctl", "restart", p.Service); err != nil {
		opt.Log("systemctl restart after rollback returned %v: %s", err, strings.TrimSpace(out))
	}
	if _, ok := waitHealthy(ctx, p, "", opt.Deadline); !ok {
		return errors.New("box did not become healthy after rollback")
	}
	return nil
}

// Rollback reverts to the last-known-good binary+DB pair (from an in-flight
// marker, else last-good.json). Used by `openvtld rollback`.
func Rollback(ctx context.Context, opt Options) error {
	opt.def()
	p := opt.Paths
	m, ok := readMarker(p)
	if !ok {
		if m, ok = readLastGood(p); !ok {
			return errors.New("nothing to roll back to (no pending update and no last-known-good record)")
		}
	}
	if m.PrevBinary == "" {
		return errors.New("rollback record has no previous binary")
	}
	if _, err := os.Stat(m.PrevBinary); err != nil {
		return fmt.Errorf("previous binary %s is missing: %w", m.PrevBinary, err)
	}
	opt.Log("rolling back to %s (from %s)", m.FromVersion, m.ToVersion)
	return rollbackNow(ctx, p, m, opt)
}

// ConfirmWhenHealthy is the daemon-side self-confirm: once the newly-swapped
// binary is up and healthy, it clears the pending-update marker so the watchdog
// stops counting and a later restart is a normal boot. Runs as a goroutine from
// main; a no-op when there's no pending update or this isn't the target version.
func ConfirmWhenHealthy(ctx context.Context, p Paths, version string, logf func(string, ...any)) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	m, ok := readMarker(p)
	if !ok || m.ToVersion != version {
		return
	}
	if _, ok := waitHealthy(ctx, p, version, 90*time.Second); ok {
		clearMarker(p)
		logf("update confirmed by self-probe — running %s", version)
	}
}

func nz(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}
