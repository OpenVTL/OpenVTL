package update

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/openvtl/openvtld/internal/release"
)

// CLI dispatches the update-related subcommands (`openvtld update|rollback|
// verify-bundle|update-status`). curVersion/curBuild are the running binary's
// build identity. Returns a process exit code.
func CLI(cmd string, args []string, curVersion, curBuild string) int {
	switch cmd {
	case "update":
		return cliUpdate(args, curVersion, curBuild)
	case "rollback":
		return cliRollback(args, curVersion, curBuild)
	case "verify-bundle":
		return cliVerify(args)
	case "update-status":
		return cliStatus(args)
	default:
		fmt.Fprintf(os.Stderr, "unknown update subcommand %q\n", cmd)
		return 2
	}
}

// pathsFlags registers the shared path/service overrides (defaults = installed
// layout; tests and non-standard boxes override them).
func pathsFlags(fs *flag.FlagSet) func() Paths {
	d := DefaultPaths()
	bin := fs.String("bin", d.Binary, "installed openvtld binary path")
	prev := fs.String("prev-bin", d.PrevBinary, "previous-binary backup path")
	state := fs.String("state-dir", d.StateDir, "openvtld state directory (marker + backups)")
	db := fs.String("db", d.DB, "SQLite database path")
	svc := fs.String("service", d.Service, "systemd service name")
	plain := fs.String("plain", d.Plain, "plaintext health address host:port (\"\" if disabled)")
	tls := fs.String("tls", d.TLS, "TLS health address host:port")
	return func() Paths {
		return Paths{
			Binary:     *bin,
			PrevBinary: *prev,
			StateDir:   *state,
			DB:         *db,
			BackupsDir: filepath.Join(*state, "backups"),
			Service:    *svc,
			Plain:      *plain,
			TLS:        *tls,
		}
	}
}

func stdoutLog(format string, a ...any) { fmt.Printf("  "+format+"\n", a...) }

// cliContext cancels on Ctrl-C so a long wait can be interrupted, but the
// updater's own rollback path still runs to completion via context.WithoutCancel
// inside Apply's critical restore (rollbackNow uses the passed ctx; interrupting
// mid-rollback is the operator's call).
func cliContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

func cliUpdate(args []string, curVersion, curBuild string) int {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	getPaths := pathsFlags(fs)
	force := fs.Bool("force", false, "proceed past mid-job / same-version refusals")
	deadline := fs.Duration("deadline", 90*time.Second, "health-confirm deadline")
	limit := fs.Int("attempt-limit", 3, "watchdog crash-restart budget before auto-rollback")
	pinFile := fs.String("mhvtl-pin", DefaultMhvtlPinFile, "installed mhVTL pin file (Tier-B detection)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: openvtld update [flags] <bundle.tar.gz>")
		return 2
	}
	bundle := fs.Arg(0)
	if _, err := os.Stat(bundle); err != nil {
		fmt.Fprintf(os.Stderr, "[x] %v\n", err)
		return 1
	}
	if os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, "[x] openvtld update must run as root")
		return 1
	}
	ctx, cancel := cliContext()
	defer cancel()

	fmt.Printf("OpenVTL update — %s -> (bundle)\n", nz(curVersion))
	err := Apply(ctx, bundle, Options{
		Paths:          getPaths(),
		CurrentVersion: curVersion,
		CurrentBuild:   curBuild,
		MhvtlPinFile:   *pinFile,
		Force:          *force,
		Deadline:       *deadline,
		AttemptLimit:   *limit,
		Log:            stdoutLog,
	})
	if err == nil {
		fmt.Println("[+] update complete.")
		return 0
	}
	switch {
	case errors.Is(err, ErrSameVersion):
		fmt.Printf("[=] %v\n", err)
		return 3
	case errors.Is(err, ErrDowngrade):
		fmt.Printf("[x] %v\n", err)
		return 3
	case errors.Is(err, ErrTierBC):
		fmt.Printf("[x] %v\n", err)
		fmt.Println("    This bundle updates the data plane. Apply it with a maintenance window:")
		fmt.Println("        sudo <bundle>/repo/packaging/install.sh")
		return 3
	case errors.Is(err, ErrMidJob):
		fmt.Printf("[x] %v\n", err)
		return 3
	default:
		fmt.Fprintf(os.Stderr, "[x] %v\n", err)
		return 1
	}
}

func cliRollback(args []string, curVersion, curBuild string) int {
	fs := flag.NewFlagSet("rollback", flag.ContinueOnError)
	getPaths := pathsFlags(fs)
	deadline := fs.Duration("deadline", 90*time.Second, "health deadline after rollback")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, "[x] openvtld rollback must run as root")
		return 1
	}
	ctx, cancel := cliContext()
	defer cancel()
	fmt.Println("OpenVTL rollback — reverting to last-known-good")
	if err := Rollback(ctx, Options{
		Paths:          getPaths(),
		CurrentVersion: curVersion,
		CurrentBuild:   curBuild,
		Deadline:       *deadline,
		Log:            stdoutLog,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "[x] %v\n", err)
		return 1
	}
	fmt.Println("[+] rollback complete.")
	return 0
}

// cliVerify verifies a bundle's signature + checksums without applying it.
// Accepts a .tar.gz or an already-unpacked bundle directory.
func cliVerify(args []string) int {
	fs := flag.NewFlagSet("verify-bundle", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: openvtld verify-bundle <bundle.tar.gz|dir>")
		return 2
	}
	target := fs.Arg(0)
	info, err := os.Stat(target)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[x] %v\n", err)
		return 1
	}
	dir := target
	if !info.IsDir() {
		tmp, err := os.MkdirTemp("", "ovtl-verify-")
		if err != nil {
			fmt.Fprintf(os.Stderr, "[x] %v\n", err)
			return 1
		}
		defer os.RemoveAll(tmp)
		if dir, err = unpackBundle(target, tmp); err != nil {
			fmt.Fprintf(os.Stderr, "[x] unpack: %v\n", err)
			return 1
		}
	}
	if err := release.VerifyBundleDir(dir); err != nil {
		fmt.Fprintf(os.Stderr, "[x] %v\n", err)
		return 1
	}
	v, _ := release.ParseVersionDir(dir)
	fmt.Printf("[+] bundle verified: %s (built %s, %s)\n", v.Hash, v.BuildTime, v.MhvtlPin)
	return 0
}

func cliStatus(args []string) int {
	fs := flag.NewFlagSet("update-status", flag.ContinueOnError)
	getPaths := pathsFlags(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	p := getPaths()
	if m, ok := readMarker(p); ok {
		fmt.Println("pending update:")
		printMarker(m)
		return 0
	}
	if m, ok := readLastGood(p); ok {
		fmt.Println("no pending update. last-known-good (rollback target):")
		printMarker(m)
		return 0
	}
	fmt.Println("no pending update, no rollback record.")
	return 0
}

func printMarker(m Marker) {
	b, _ := json.MarshalIndent(m, "  ", "  ")
	fmt.Printf("  %s\n", b)
}
