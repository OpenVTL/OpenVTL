package orchestrate

// ApplyLibraries is the daemon-orchestrated half of the maintenance
// window: activate pending_restart libraries by restarting mhVTL with
// the full mhVTL restart sequence and rebuilding the FC target. THE OPERATOR OWNS
// THE OTHER HALF — vary off/on and the IBM i device descriptions; this
// endpoint must only run inside an agreed window, which the typed
// confirmation enforces socially and the preflight enforces
// technically (idle drives, no active jobs, data plane quiet).
//
// Sequence (every step is a binding lesson, do not reorder):
//  1. preflight — refuse if any drive is loaded/active or jobs run
//  2. rewrite every live library_contents from live element status
//     (else carts teleport to stale home slots on restart)
//  3. stop mhvtl.target, pkill -9 orphans, rm /var/lock/mhvtl/*
//     (orphans holding locks wedge the restart into D-state)
//  4. start mhvtl.target, wait for every vtllibrary@/vtltape@ instance
//  5. engine reload (sg nodes renumbered)
//  6. FC Ensure — Verify fails on the renumbered nodes, Rebuild runs
//     (clearconfig drops sessions: acceptable ONLY here)

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/openvtl/openvtld/internal/inventory"
	"github.com/openvtl/openvtld/internal/sysexec"
)

type ApplyResult struct {
	Steps []string `json:"steps"`
	FC    Result   `json:"fc"`
}

type applyEngine interface {
	Snapshot() inventory.Snapshot
	Reload(ctx context.Context) error
	RewriteLibraryContents(ctx context.Context, libID int) error
	SetFCState(fc inventory.FCState)
}

// ApplyLibraries runs the maintenance-window restart+rebuild. onStep,
// when non-nil, is called with each completed step's text as it
// happens — the API layer publishes these to the SSE bus so the UI
// shows live progress instead of a button that "just sits" through a
// multi-minute synchronous call (2026-07-05 operator feedback).
func (f *FC) ApplyLibraries(ctx context.Context, eng applyEngine, onStep func(string)) (*ApplyResult, error) {
	res := &ApplyResult{}
	step := func(format string, a ...any) {
		s := fmt.Sprintf(format, a...)
		res.Steps = append(res.Steps, s)
		f.log.Info("apply-libraries: " + s)
		if onStep != nil {
			onStep(s)
		}
	}
	run := func(name string, args ...string) error {
		_, err := sysexec.Run(ctx, 60*time.Second, name, args...)
		if err != nil {
			return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
		}
		return nil
	}

	// 1. preflight
	snap := eng.Snapshot()
	for _, l := range snap.Libraries {
		for _, d := range l.Drives {
			if d.Loaded != "" || d.Activity != "idle" {
				return res, fmt.Errorf("preflight: library %d drive %d holds %q (%s) — unload from the host first",
					l.Library.ID, d.Index, d.Loaded, d.Activity)
			}
		}
	}
	step("preflight: all drives empty and idle")

	// 2. persist runtime cart positions
	for _, l := range snap.Libraries {
		if !l.Library.Live {
			continue
		}
		if err := eng.RewriteLibraryContents(ctx, l.Library.ID); err != nil {
			return res, fmt.Errorf("library_contents.%d rewrite: %w", l.Library.ID, err)
		}
		step("library_contents.%d regenerated from live element status", l.Library.ID)
	}

	// 3./4. the restart sequence
	if err := run("systemctl", "stop", "mhvtl.target"); err != nil {
		return res, err
	}
	// Orphaned daemons hold /var/lock/mhvtl/* and wedge the restart.
	_, _ = sysexec.Run(ctx, 15*time.Second, "pkill", "-9", "-x", "vtltape")
	_, _ = sysexec.Run(ctx, 15*time.Second, "pkill", "-9", "-x", "vtllibrary")
	if err := run("sh", "-c", "rm -f /var/lock/mhvtl/*"); err != nil {
		return res, err
	}
	// Stale `vtlcmd N exit` messages survive in mhVTL's shared SysV queue
	// (fixed key 0x4d61726b, "Mark") whenever a daemon dies before consuming
	// them — ExecStop + KillMode=none races guarantee some do. The next
	// vtltape with the same minor reads one at startup and shuts itself down
	// seconds later (the v0.6 "pair-crash"; the SEGV inside that shutdown is
	// packaging/mhvtl Patch 10). Remove the queue while everything is dead;
	// the first daemon up recreates it empty. Tolerate absence.
	_, _ = sysexec.Run(ctx, 15*time.Second, "ipcrm", "-Q", "0x4d61726b")
	if err := run("systemctl", "daemon-reload"); err != nil { // new per-instance drop-ins
		return res, err
	}
	step("mhvtl.target stopped, orphans killed, locks cleared, stale IPC queue removed")
	// This path (Activate / data-plane restart) only ever restarts
	// daemons that are STILL declared, so every device comes back and
	// no stale nodes linger. Deleting a LIVE library — which leaves a
	// daemonless dead device that wedges discovery — takes the reboot
	// path instead (see deleteLibrary); it never reaches here.
	if err := run("systemctl", "start", "mhvtl.target"); err != nil {
		return res, err
	}

	libs, err := inventory.ParseMhvtlConf(f.cfg.MhvtlConf)
	if err != nil {
		return res, err
	}
	var units []string
	for _, l := range libs {
		units = append(units, fmt.Sprintf("vtllibrary@%d.service", l.ID))
		for _, d := range l.Drives {
			units = append(units, fmt.Sprintf("vtltape@%d.service", d.QueueID))
		}
	}
	if len(units) == 0 {
		// Deleting the last library leaves zero daemons to wait for —
		// mhvtl.target is up with nothing under it, and the fabric step
		// below rebuilds down to empty targets.
		step("no libraries declared — no daemons to wait for")
	} else {
		deadline := time.Now().Add(45 * time.Second)
		for {
			out, _ := sysexec.Run(ctx, 15*time.Second, "systemctl", append([]string{"is-active"}, units...)...)
			if !strings.Contains(out, "inactive") && !strings.Contains(out, "failed") && out != "" {
				break
			}
			if time.Now().After(deadline) {
				return res, fmt.Errorf("mhvtl daemons not all active after restart (%s) — a wedged instance needs a reboot; states: %s",
					strings.Join(units, " "), strings.ReplaceAll(strings.TrimSpace(out), "\n", ","))
			}
			select {
			case <-ctx.Done():
				return res, ctx.Err()
			case <-time.After(2 * time.Second):
			}
		}
		step("mhvtl.target restarted — %d daemon instances active", len(units))

		// 4b. settle: systemd "active" precedes SCSI device registration by
		// a beat — the first window-1 Apply built against a half-registered
		// library. Wait until every declared serial answers discovery.
		nSerials := 0
		for _, l := range libs {
			nSerials += 1 + len(l.Drives)
		}
		if missing := WaitDiscovery(ctx, libs, 45*time.Second); len(missing) > 0 {
			return res, fmt.Errorf("devices still unregistered %v after restart — check vtltape/vtllibrary journals", missing)
		}
		step("all %d declared devices answering sg discovery", nSerials)
	}

	// 5. re-discover (sg nodes renumbered)
	if err := eng.Reload(ctx); err != nil {
		return res, err
	}
	step("engine reloaded: sg nodes re-discovered")

	// 6. target fabrics. A box with no serving FC port (generic
	// installs, pre-commissioning) is a legal shape — the libraries are
	// fully served to the local kernel; skipping here must not fail the
	// Apply.
	if !f.AnyFabric(ctx) {
		res.FC = Result{OK: true, Detail: "skipped — no target fabrics configured (no serving FC ports)"}
		eng.SetFCState(inventory.FCState{NoHBA: !f.HBAPresent(), Detail: res.FC.Detail})
		step("target fabrics: %s", res.FC.Detail)
		step("READY FOR OPERATOR: no host-facing target on this box — attach an FC HBA to present the libraries")
		return res, nil
	}
	r := f.Ensure(ctx, libs)
	eng.SetFCState(inventory.FCState{Verified: r.OK, Detail: r.Detail})
	res.FC = r
	step("target fabrics: %s", r.Detail)
	if !r.OK {
		return res, fmt.Errorf("target fabrics not verified after apply: %s", r.Detail)
	}
	step("READY FOR OPERATOR: vary off/on the host device descriptions; create the new MLB description for the added library")
	return res, nil
}
