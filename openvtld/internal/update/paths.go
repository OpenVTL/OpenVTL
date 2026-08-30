// Package update is the Tier-A self-update apply/rollback core: an
// accepted bundle restarts only the control plane, never the tape data
// path. It is shared by the CLI updater
// (`openvtld update` / `openvtld rollback`) and, later, the in-UI Updates panel.
//
// Tier A is the openvtld binary (± a forward-only DB migration): a control-plane
// restart with zero host disruption. The flow is verify → preflight → backup →
// atomic swap + pending marker → restart → confirm-or-auto-rollback. Rollback
// always pairs the previous binary with the DB snapshot taken alongside it,
// because migrations are forward-only.
package update

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Paths are the on-box locations the updater touches. Defaults match the
// installed layout (install.sh / openvtld.service); tests and the sandbox
// override them.
type Paths struct {
	Binary     string // live binary, swapped in place
	PrevBinary string // last-good binary, kept for rollback
	StateDir   string // /var/lib/openvtld
	DB         string // SQLite DB
	BackupsDir string // DB snapshots
	Service    string // systemd unit name
	Plain      string // plaintext listen addr for health probe (host:port), "" if disabled
	TLS        string // TLS listen addr for health probe (host:port)
}

func DefaultPaths() Paths {
	return Paths{
		Binary:     "/usr/local/bin/openvtld",
		PrevBinary: "/usr/local/bin/openvtld.prev",
		StateDir:   "/var/lib/openvtld",
		DB:         "/var/lib/openvtld/openvtld.db",
		BackupsDir: "/var/lib/openvtld/backups",
		Service:    "openvtld",
		Plain:      "127.0.0.1:8080",
		TLS:        "127.0.0.1:8443",
	}
}

func (p Paths) markerJSON() string { return filepath.Join(p.StateDir, "update-pending.json") }
func (p Paths) markerEnv() string  { return filepath.Join(p.StateDir, "update-pending.env") }
func (p Paths) attempts() string   { return filepath.Join(p.StateDir, "update-attempts") }
func (p Paths) lastGood() string   { return filepath.Join(p.StateDir, "last-good.json") }

// Marker records an in-flight (or just-completed) update. It is the rollback
// anchor: the watchdog and `openvtld rollback` read it to restore the exact
// binary+DB pair that was live before the swap.
type Marker struct {
	FromVersion  string `json:"from_version"`
	ToVersion    string `json:"to_version"`
	FromBuild    string `json:"from_build,omitempty"`
	ToBuild      string `json:"to_build,omitempty"`
	Binary       string `json:"binary"`
	PrevBinary   string `json:"prev_binary"`
	DBPath       string `json:"db_path"`
	DBSnapshot   string `json:"db_snapshot"`
	AttemptLimit int    `json:"attempt_limit"`
	StagedAt     string `json:"staged_at"`
	Deadline     string `json:"deadline"`
}

func writeMarker(p Paths, m Marker) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(p.markerJSON(), b, 0o644); err != nil {
		return err
	}
	// A shell-sourceable twin the ExecStartPre watchdog reads without a JSON
	// parser (it must run even when both binaries are suspect).
	env := fmt.Sprintf(
		"OVTL_BINARY=%s\nOVTL_PREV_BINARY=%s\nOVTL_DB=%s\nOVTL_DB_SNAPSHOT=%s\nOVTL_ATTEMPT_LIMIT=%d\n",
		m.Binary, m.PrevBinary, m.DBPath, m.DBSnapshot, m.AttemptLimit)
	if err := os.WriteFile(p.markerEnv(), []byte(env), 0o644); err != nil {
		return err
	}
	return os.WriteFile(p.attempts(), []byte("0\n"), 0o644)
}

func readMarker(p Paths) (Marker, bool) {
	b, err := os.ReadFile(p.markerJSON())
	if err != nil {
		return Marker{}, false
	}
	var m Marker
	if json.Unmarshal(b, &m) != nil {
		return Marker{}, false
	}
	return m, true
}

// clearMarker removes the pending state after a confirmed update or a completed
// rollback. It deliberately keeps PrevBinary, the DB snapshot, and last-good so
// a later `openvtld rollback` can still revert.
func clearMarker(p Paths) {
	os.Remove(p.markerJSON())
	os.Remove(p.markerEnv())
	os.Remove(p.attempts())
}

func writeLastGood(p Paths, m Marker) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p.lastGood(), b, 0o644)
}

func readLastGood(p Paths) (Marker, bool) {
	b, err := os.ReadFile(p.lastGood())
	if err != nil {
		return Marker{}, false
	}
	var m Marker
	if json.Unmarshal(b, &m) != nil {
		return Marker{}, false
	}
	return m, true
}

// copyFile copies src to dst atomically (temp + rename) with mode, fsyncing the
// data before the rename so a crash can't leave a half-written binary.
func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

// snapshotDB writes a consistent standalone copy of the live SQLite DB via
// VACUUM INTO — safe to run while the daemon holds the DB open in WAL mode.
func snapshotDB(ctx context.Context, dbPath, dst string) error {
	os.Remove(dst) // VACUUM INTO refuses an existing target
	db, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(8000)")
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	q := "VACUUM INTO '" + strings.ReplaceAll(dst, "'", "''") + "'"
	_, err = db.ExecContext(ctx, q)
	return err
}

// restoreDB replaces the live DB with a snapshot and removes the WAL/SHM
// sidecars so SQLite can't replay a newer WAL over the restored file.
func restoreDB(snapshot, dbPath string) error {
	if snapshot == "" {
		return nil // no snapshot was taken (e.g. fresh box) — nothing to restore
	}
	if err := copyFile(snapshot, dbPath, 0o644); err != nil {
		return err
	}
	os.Remove(dbPath + "-wal")
	os.Remove(dbPath + "-shm")
	return nil
}

// unpackBundle extracts a .tar.gz release bundle into destDir and returns the
// single top-level directory it contains (openvtl-<ver>/). Path traversal is
// refused.
func unpackBundle(tarball, destDir string) (string, error) {
	f, err := os.Open(tarball)
	if err != nil {
		return "", err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return "", err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	root := ""
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		name := filepath.Clean(filepath.FromSlash(hdr.Name))
		if name == "." || name == ".." || strings.HasPrefix(name, ".."+string(filepath.Separator)) || filepath.IsAbs(name) {
			return "", fmt.Errorf("unsafe path in bundle: %q", hdr.Name)
		}
		top := name
		if i := strings.IndexByte(name, filepath.Separator); i >= 0 {
			top = name[:i]
		}
		if root == "" {
			root = top
		} else if top != root {
			return "", fmt.Errorf("bundle has multiple top-level dirs (%q, %q)", root, top)
		}
		target := filepath.Join(destDir, name)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return "", err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return "", err
			}
			out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, os.FileMode(hdr.Mode)&0o777)
			if err != nil {
				return "", err
			}
			if _, err := io.Copy(out, tr); err != nil { //nolint:gosec — size bounded by trusted, about-to-be-verified bundle
				out.Close()
				return "", err
			}
			out.Close()
		}
	}
	if root == "" {
		return "", fmt.Errorf("empty bundle")
	}
	return filepath.Join(destDir, root), nil
}

// run executes a command with a timeout, returning combined output for error
// context.
func run(ctx context.Context, timeout time.Duration, name string, args ...string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, name, args...)
	cmd.WaitDelay = 5 * time.Second
	out, err := cmd.CombinedOutput()
	return string(out), err
}
