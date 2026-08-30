// openvtld — the OpenVTL control plane.
//
// Observes mhVTL + LIO + the ZFS storage pool, owns boot orchestration for the
// FC target, and serves the REST/SSE API + embedded web UI. The data
// plane (kernel + mhVTL) runs without it; openvtld failing must never
// stop a backup.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"strings"

	"github.com/openvtl/openvtld/internal/api"
	"github.com/openvtl/openvtld/internal/config"
	"github.com/openvtl/openvtld/internal/events"
	"github.com/openvtl/openvtld/internal/export"
	"github.com/openvtl/openvtld/internal/inventory"
	"github.com/openvtl/openvtld/internal/orchestrate"
	"github.com/openvtl/openvtld/internal/storage"
	"github.com/openvtl/openvtld/internal/store"
	"github.com/openvtl/openvtld/internal/tlsutil"
	"github.com/openvtl/openvtld/internal/update"
)

var (
	version   = "dev"     // set via -ldflags at build (short commit hash)
	buildDate = "unknown" // set via -ldflags at build (RFC3339) — update-ordering key
)

func main() {
	// v0.8 self-update subcommands run and exit before the daemon path. They
	// take their own flags, so intercept before config.Load()'s flag.Parse().
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "update", "rollback", "verify-bundle", "update-status":
			os.Exit(update.CLI(os.Args[1], os.Args[2:], version, buildDate))
		}
	}

	cfg := config.Load()
	if cfg.ShowVersion {
		fmt.Println(version)
		return
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)
	log.Info("openvtld starting", "version", version, "listen", cfg.Listen)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := os.MkdirAll(filepath.Dir(cfg.DBPath), 0o755); err != nil {
		log.Error("db dir", "err", err)
		os.Exit(1)
	}
	db, err := store.Open(cfg.DBPath)
	if err != nil {
		log.Error("store open", "err", err)
		os.Exit(1)
	}
	defer db.Close()

	// ACL intent bootstraps once from the -acls flag; from then on the
	// initiator_acl table is authority (UI-managed, v0.5).
	if err := db.SeedACLs(ctx, strings.Split(cfg.ACLs, ",")); err != nil {
		log.Error("acl seed", "err", err)
		os.Exit(1)
	}

	bus := events.NewBus()
	inv := inventory.New(cfg, bus, db, log)
	if err := inv.Start(ctx); err != nil {
		log.Error("inventory start", "err", err)
		os.Exit(1)
	}

	// Support-bundle FC counter baseline: snapshot the fc_host statistics
	// now so fc.txt can show since-start deltas (read-only sysfs).
	api.CaptureFCBaseline()

	// Boot orchestration (v0.7 multi-port): every target-capable FC
	// port serves (minus operator-disabled ones). A box with none is a
	// legal shape — orchestration idles and everything else runs.
	// Failure is reported, not fatal — the UI must come up regardless
	// so the operator can see what's wrong.
	fc := orchestrate.New(cfg, db, log)
	if !fc.AnyFabric(ctx) {
		detail := "no target fabrics configured — attach an FC HBA to present the libraries"
		inv.SetFCState(inventory.FCState{NoHBA: !fc.HBAPresent(), Detail: detail})
		log.Info(detail, "hba_present", fc.HBAPresent())
	} else if !cfg.SkipFC {
		go func() {
			libs, err := inventory.ParseMhvtlConf(cfg.MhvtlConf)
			if err != nil {
				log.Error("orchestrator: mhvtl config", "err", err)
				return
			}
			// Boot races mhvtl.target: the engine started against
			// unregistered SCSI devices (degraded) and an immediate Ensure
			// once excluded every library and left an EMPTY target. Settle
			// discovery, pick up the now-live libraries, then verify.
			if missing := orchestrate.WaitDiscovery(ctx, libs, 90*time.Second); len(missing) > 0 {
				log.Warn("boot: declared devices never registered — ensure will report", "missing", missing)
			} else if err := inv.Reload(ctx); err != nil {
				log.Warn("boot: engine reload after discovery settle", "err", err)
			}
			r := fc.Ensure(ctx, libs)
			inv.SetFCState(inventory.FCState{Verified: r.OK, Detail: r.Detail})
			// Early-boot faults are frequently transient: FC link/ISP
			// settling, slow firmware loads, targetcli flakes (the historic
			// exit-255 was the rtslib-fb-targetctl boot restore racing this
			// rebuild — disabled by install/deploy since v0.8.x). Self-heal
			// with a bounded backoff loop (~15 min) instead of the old
			// single retry so a reboot converges without operator help;
			// bounded so it can never fight a later maintenance window.
			// Ensure is serialized + additive-after-first-rebuild, so a
			// retry never drops sessions on ports that already came up.
			for _, delay := range []time.Duration{5 * time.Second, 15 * time.Second,
				30 * time.Second, time.Minute, 2 * time.Minute, 5 * time.Minute, 5 * time.Minute} {
				if r.OK {
					break
				}
				log.Warn("boot: fabric ensure not verified — will retry", "retry_in", delay.String(), "detail", r.Detail)
				select {
				case <-ctx.Done():
					return
				case <-time.After(delay):
				}
				r = fc.Ensure(ctx, libs)
				inv.SetFCState(inventory.FCState{Verified: r.OK, Detail: r.Detail})
			}
			// This goroutine is the only holder of the result — without
			// this line a boot-time ensure failure is invisible in the
			// journal (found the hard way 2026-07-05: silent 8-hour
			// unverified state after a mid-apply reboot).
			if r.OK {
				log.Info("boot: target fabrics ensured", "detail", r.Detail)
			} else {
				log.Error("boot: target fabrics NOT verified after retries — investigate, then re-ensure via the System panel or restart openvtld", "detail", r.Detail)
			}
		}()
	}

	// v0.4 job runner: resumes unfinished export/import/evict jobs,
	// then watches for IE vault moves and pool pressure (both
	// settings-gated, default off).
	librarySN := ""
	if libs := inv.Snapshot().Libraries; len(libs) > 0 {
		librarySN = libs[0].Library.Serial
	}
	runner := export.NewRunner(db, bus, inv, log, export.Options{
		MediaDir:   cfg.MediaDir,
		StagingDir: cfg.StagingDir,
		ChunkBytes: cfg.ChunkBytes,
		Version:    version,
		LibrarySN:  librarySN, // fallback only — manifests stamp the cart's own library
	})
	if err := runner.Start(ctx); err != nil {
		log.Error("job runner start", "err", err)
		os.Exit(1)
	}
	runner.StartWatchers(ctx)

	// v0.6 storage manager: pool builder + cache-device designation.
	stor := storage.New(db, bus, log)
	if err := stor.Start(ctx); err != nil {
		log.Error("storage manager start", "err", err)
		os.Exit(1)
	}

	certFile, keyFile, err := tlsutil.EnsureCert(cfg.TLSDir)
	if err != nil {
		log.Error("tls cert", "err", err)
		os.Exit(1)
	}

	// v0.8: if we were just swapped in by an update, self-confirm once healthy
	// so the auto-rollback watchdog stands down. No-op on a normal boot.
	go func() {
		up := update.DefaultPaths()
		up.Plain, up.TLS = healthAddr(cfg.Listen), healthAddr(cfg.ListenTLS)
		update.ConfirmWhenHealthy(ctx, up, version, func(f string, a ...any) { log.Info(fmt.Sprintf(f, a...)) })
	}()

	srv := api.New(cfg, inv, bus, db, runner, fc, stor, api.UIFS(), log, version, buildDate)
	if err := srv.ListenAndServe(ctx, cfg.ListenTLS, certFile, keyFile, cfg.Listen); err != nil {
		log.Error("http server", "err", err)
		os.Exit(1)
	}
	log.Info("openvtld stopped")
}

// healthAddr turns a listen spec (":8080", "0.0.0.0:8080") into a loopback
// host:port the local update health-probe can reach. Empty in, empty out.
func healthAddr(listen string) string {
	if listen == "" {
		return ""
	}
	_, port, err := net.SplitHostPort(listen)
	if err != nil {
		return ""
	}
	return net.JoinHostPort("127.0.0.1", port)
}
