package api

// Settings → Updates. The browser uploads a
// signed bundle; the daemon PRECHECKS it (signature + preflight) so bad
// bundles fail in the upload response, then hands the actual apply to a
// DETACHED transient systemd unit running the proven CLI path
// (`openvtld update`). The detachment is the whole trick: the apply
// restarts openvtld.service, and any child of the daemon dies with the
// service cgroup — systemd-run escapes it, so the updater survives its
// parent's restart and runs the same health probe + CLI-side rollback
// as an operator at a shell. The UI rides out the restart by polling
// /healthz (public) until the target version answers.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/openvtl/openvtld/internal/update"
)

// updatesDir stages uploaded bundles; the detached unit deletes its
// bundle when done (best-effort — boot could sweep leftovers, but a
// 22M straggler in the state dir is harmless).
const updatesDir = "/var/lib/openvtld/updates"

func (s *Server) updateOptions(force bool) update.Options {
	return update.Options{
		Paths:          update.DefaultPaths(),
		CurrentVersion: s.version,
		CurrentBuild:   s.buildDate,
		Force:          force,
	}
}

func (s *Server) updateStatus(w http.ResponseWriter, _ *http.Request) {
	pending, lastGood := update.Status(update.DefaultPaths())
	writeJSON(w, 200, map[string]any{
		"version":   s.version,
		"build":     s.buildDate,
		"pending":   pending,
		"last_good": lastGood,
	})
}

// uploadUpdate: multipart POST, field "bundle" (+ optional force=1).
// 202 means "handed to the detached updater — watch the version".
func (s *Server) uploadUpdate(w http.ResponseWriter, r *http.Request) {
	// Bundles are ~22M; half a GB of headroom, not a general file sink.
	r.Body = http.MaxBytesReader(w, r.Body, 512<<20)
	f, hdr, err := r.FormFile("bundle")
	if err != nil {
		badRequest(w, "multipart field 'bundle' required: %v", err)
		return
	}
	defer f.Close()
	force := r.FormValue("force") == "1"

	if err := os.MkdirAll(updatesDir, 0o755); err != nil {
		serverError(w, err)
		return
	}
	dst := filepath.Join(updatesDir, fmt.Sprintf("upload-%d.tar.gz", time.Now().UnixNano()))
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		serverError(w, err)
		return
	}
	if _, err := io.Copy(out, f); err != nil {
		out.Close()
		os.Remove(dst)
		badRequest(w, "upload aborted: %v", err)
		return
	}
	out.Close()

	opt := s.updateOptions(force)
	bv, err := update.Precheck(r.Context(), dst, opt)
	if err != nil {
		os.Remove(dst)
		if errors.Is(err, update.ErrTierBC) {
			badRequest(w, "%v — this bundle updates the data plane; apply it with install.sh in a maintenance window", err)
			return
		}
		badRequest(w, "%v", err)
		return
	}

	// Detach: a transient unit outside our cgroup survives the
	// control-plane restart the apply performs.
	unit := fmt.Sprintf("openvtl-update-%d", time.Now().Unix())
	forceArg := ""
	if force {
		forceArg = "-force "
	}
	script := fmt.Sprintf("%s update %s%s; rm -f %s", opt.Paths.Binary, forceArg, dst, dst)
	if out, err := exec.Command("systemd-run", "--unit="+unit, "--collect",
		"/bin/sh", "-c", script).CombinedOutput(); err != nil {
		os.Remove(dst)
		serverError(w, fmt.Errorf("could not launch the detached updater: %v (%s)", err, string(out)))
		return
	}
	s.audit(r, "system.update", bv.Hash, map[string]any{
		"from": s.version, "to": bv.Hash, "built": bv.BuildTime,
		"file": hdr.Filename, "force": force, "unit": unit,
	})
	s.log.Info("update handed to detached unit", "unit", unit, "from", s.version, "to", bv.Hash)
	writeJSON(w, 202, map[string]any{
		"from": s.version, "to": bv.Hash, "built": bv.BuildTime, "unit": unit,
		"detail": "applying — the control plane restarts; tape I/O is unaffected. Progress: journalctl -u " + unit,
	})
}

// rollbackUpdate reverts to last-known-good via the same detached path.
func (s *Server) rollbackUpdate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Confirm string `json:"confirm"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Confirm != "rollback" {
		badRequest(w, `confirmation required: {"confirm":"rollback"} — reverts to the last-known-good binary + DB snapshot`)
		return
	}
	pending, lastGood := update.Status(update.DefaultPaths())
	if pending == nil && lastGood == nil {
		badRequest(w, "nothing to roll back to (no pending update and no last-known-good record)")
		return
	}
	target := lastGood
	if pending != nil {
		target = pending
	}
	unit := fmt.Sprintf("openvtl-rollback-%d", time.Now().Unix())
	binary := update.DefaultPaths().Binary
	if out, err := exec.Command("systemd-run", "--unit="+unit, "--collect",
		binary, "rollback").CombinedOutput(); err != nil {
		serverError(w, fmt.Errorf("could not launch the detached rollback: %v (%s)", err, string(out)))
		return
	}
	s.audit(r, "system.rollback", target.FromVersion, map[string]any{
		"from": s.version, "to": target.FromVersion, "unit": unit,
	})
	writeJSON(w, 202, map[string]any{
		"from": s.version, "to": target.FromVersion, "unit": unit,
		"detail": "rolling back — the control plane restarts. Progress: journalctl -u " + unit,
	})
}
