package api

// System control + API access keys (v0.7).
//
// Restart is control-plane only: mhVTL, LIO, and host I/O never notice
// openvtld going away. Graceful mode drains jobs first (refusing new
// job-creating calls meanwhile); immediate restarts now — interrupted
// jobs are failed-retryable by the runner's startup pass, exports
// resume from their chunk ledger.
//
// API keys are bearer identities for scripts: same role vocabulary as
// users, hash-only storage, master toggle default OFF. Resolution
// lives in the auth middleware; management lives here.

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/openvtl/openvtld/internal/auth"
	"github.com/openvtl/openvtld/internal/catalog"
	"github.com/openvtl/openvtld/internal/store"
	"github.com/openvtl/openvtld/internal/sysexec"
)

const settingAPIKeysEnabled = "apikeys.enabled"

// drainBlocked refuses job-creating calls while a graceful restart
// waits for the queue to empty.
func (s *Server) drainBlocked(w http.ResponseWriter) bool {
	if s.draining.Load() {
		writeJSON(w, 409, map[string]string{
			"error": "restart pending — waiting for active jobs to drain; retry after the restart"})
		return true
	}
	return false
}

func (s *Server) systemInfo(w http.ResponseWriter, r *http.Request) {
	active := 0
	if jobs, err := s.db.UnfinishedJobs(r.Context()); err == nil {
		active = len(jobs)
	}
	host, _ := os.Hostname()
	sysName, sysUUID, _ := s.db.SystemIdentity(r.Context(), catalog.SanitizeName(host))
	writeJSON(w, 200, map[string]any{
		"version":         s.version,
		"started_at":      s.started.UTC(),
		"uptime_sec":      int64(time.Since(s.started).Seconds()),
		"active_jobs":     active,
		"plain_listener":  s.cfg.Listen, // "" = disabled (site.conf drop-in owns the flag)
		"draining":        s.draining.Load(),
		"apikeys_enabled": s.db.Setting(r.Context(), settingAPIKeysEnabled, "") == "1",
		// S3 namespacing: the instance's friendly name (the <system> key
		// segment) and its stable UUID backstop.
		"system_name": sysName,
		"system_uuid": sysUUID,
	})
}

func (s *Server) systemRestart(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Mode    string `json:"mode"` // graceful (default) | immediate
		Confirm string `json:"confirm"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Confirm != "restart" {
		badRequest(w, `confirmation required: {"confirm":"restart"} — restarts the control plane only (host I/O unaffected)`)
		return
	}
	if in.Mode == "" {
		in.Mode = "graceful"
	}
	if in.Mode != "graceful" && in.Mode != "immediate" {
		badRequest(w, "mode must be graceful or immediate")
		return
	}
	jobs, err := s.db.UnfinishedJobs(r.Context())
	if err != nil {
		serverError(w, err)
		return
	}

	if in.Mode == "graceful" && len(jobs) > 0 {
		if s.draining.Swap(true) {
			badRequest(w, "a graceful restart is already draining")
			return
		}
		s.audit(r, "system.restart", "graceful", map[string]any{"draining_jobs": len(jobs)})
		s.log.Info("graceful restart scheduled — draining jobs", "active", len(jobs))
		go s.drainAndRestart()
		writeJSON(w, 202, map[string]any{
			"ok": true, "draining": true, "active_jobs": len(jobs),
			"detail": "restart scheduled — waiting for active jobs to finish (30 min limit)"})
		return
	}

	s.audit(r, "system.restart", in.Mode, map[string]any{"active_jobs": len(jobs)})
	s.log.Info("restart requested", "mode", in.Mode, "active_jobs", len(jobs))
	go func() {
		time.Sleep(500 * time.Millisecond) // let the response flush
		s.execRestart()
	}()
	detail := "restarting now"
	if len(jobs) > 0 {
		detail = "restarting now — interrupted jobs become retryable (exports resume from their chunk ledger)"
	}
	writeJSON(w, 200, map[string]any{"ok": true, "detail": detail})
}

func (s *Server) drainAndRestart() {
	ctx := context.Background()
	deadline := time.Now().Add(30 * time.Minute)
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for range t.C {
		jobs, err := s.db.UnfinishedJobs(ctx)
		if err == nil && len(jobs) == 0 {
			s.execRestart()
			return
		}
		if time.Now().After(deadline) {
			s.draining.Store(false)
			s.log.Warn("graceful restart abandoned — jobs still active after 30 minutes")
			s.bus.Publish("system", "restart", map[string]any{
				"detail": "graceful restart abandoned: jobs still active after 30 minutes"})
			return
		}
	}
}

// execRestart asks systemd for the restart. --no-block enqueues the
// job in PID 1 and returns — the systemctl client dying with our
// cgroup cannot cancel it.
func (s *Server) execRestart() {
	s.log.Info("restarting openvtld via systemctl")
	if _, err := sysexec.Run(context.Background(), 10*time.Second, "systemctl", "--no-block", "restart", "openvtld"); err != nil {
		s.draining.Store(false)
		s.log.Error("self-restart failed", "err", err)
	}
}

// dataplaneRestart — tier 2 (v0.7 System panel): the full Apply-style
// mhVTL restart + fabric rebuild with NO pending libraries — the
// wedged-daemon recovery sequence as a button. Loud by design: every
// host session drops; the operator varies off/on afterwards. Reuses
// orchestrate.ApplyLibraries verbatim (full mhVTL restart sequence, IPC
// queue flush, discovery settle, epoch rebuild — every step a field lesson).
func (s *Server) dataplaneRestart(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Confirm string `json:"confirm"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Confirm != "restart" {
		badRequest(w, `confirmation required: {"confirm":"restart"} — restarts mhVTL and rebuilds the target fabrics (drops every host session)`)
		return
	}
	if s.drainBlocked(w) {
		return
	}
	if jobs, err := s.db.UnfinishedJobs(r.Context()); err != nil {
		serverError(w, err)
		return
	} else if len(jobs) > 0 {
		badRequest(w, "preflight: %d active/queued job(s) — wait or cancel first", len(jobs))
		return
	}
	s.audit(r, "system.dataplane_restart", "", nil)
	// Survive client disconnects — see deleteLibrary. A browser giving
	// up mid-restart must never kill the sequence half-way.
	ctx := context.WithoutCancel(r.Context())
	label := "Restarting data plane"
	res, err := s.fc.ApplyLibraries(ctx, s.inv, s.maintStep(label))
	if err != nil {
		s.maintDone(label, false, err.Error())
		s.log.Error("data-plane restart failed", "err", err, "steps", res.Steps)
		writeJSON(w, 500, map[string]any{"error": err.Error(), "steps": res.Steps})
		return
	}
	s.maintDone(label, true, "data plane restarted")
	// Same side effect as Activate: any library the restart brought
	// live flips active in the DB (a declared-but-pending library IS
	// served once mhVTL re-reads device.conf).
	for _, l := range s.inv.Snapshot().Libraries {
		if l.Library.Live {
			if err := s.db.SetLibraryState(ctx, l.Library.ID, store.LibraryActive); err != nil && err != store.ErrNotFound {
				s.log.Warn("library state flip", "library", l.Library.ID, "err", err)
			}
		}
	}
	writeJSON(w, 200, res)
}

// rebootHost — tier 3: the recovery hammer for the states nothing
// less fixes (D-state sg probes, daemons wedged in the kernel
// module). Boot orchestration restores everything: discovery settle,
// engine reload, epoch check, fabric rebuild. Active jobs are
// interrupted and become retryable.
func (s *Server) rebootHost(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Confirm string `json:"confirm"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Confirm != "reboot" {
		badRequest(w, `confirmation required: {"confirm":"reboot"} — reboots the appliance (host sessions drop; boot orchestration restores the targets)`)
		return
	}
	active := 0
	if jobs, err := s.db.UnfinishedJobs(r.Context()); err == nil {
		active = len(jobs)
	}
	s.audit(r, "system.reboot", "", map[string]any{"active_jobs": active})
	s.log.Warn("appliance reboot requested via API", "active_jobs", active)
	// Drive the maintenance overlay so the operator gets a live
	// "reconnecting" window that survives the reboot instead of a toast
	// that disappears the moment the daemon goes down.
	label := "Rebooting appliance"
	s.maintStep(label)("reboot requested — the appliance restarts now")
	s.maintDoneReboot(label, "Rebooting — the appliance will be back in a minute or two.")
	s.rebootAppliance()
	writeJSON(w, 200, map[string]any{"ok": true,
		"detail": "rebooting — boot orchestration restores pools, daemons and targets; interrupted jobs become retryable"})
}

// rebootAppliance triggers a clean, non-blocking reboot after letting
// the HTTP response + any SSE events flush. --no-block hands the job to
// PID 1 so the systemctl client dying with our cgroup can't cancel it.
// Used by the reboot button and as the final step of a live-library
// delete (the only safe way to release the removed mhVTL devices).
func (s *Server) rebootAppliance() {
	go func() {
		time.Sleep(800 * time.Millisecond)
		if _, err := sysexec.Run(context.Background(), 10*time.Second, "systemctl", "--no-block", "reboot"); err != nil {
			s.log.Error("reboot request failed", "err", err)
		}
	}()
}

// --- API access keys ---

func (s *Server) listAPIKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := s.db.ListAPIKeys(r.Context())
	if err != nil {
		serverError(w, err)
		return
	}
	if keys == nil {
		keys = []store.APIKey{}
	}
	writeJSON(w, 200, keys)
}

func (s *Server) createAPIKey(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name string `json:"name"`
		Role string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		badRequest(w, "bad json: %v", err)
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if len(in.Name) < 2 || len(in.Name) > 64 {
		badRequest(w, "name must be 2-64 characters")
		return
	}
	for _, c := range in.Name {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.') {
			badRequest(w, "name may contain letters, digits, . _ -")
			return
		}
	}
	if !auth.ValidRole(in.Role) {
		badRequest(w, "role must be admin or readonly")
		return
	}
	raw, _, err := auth.NewToken()
	if err != nil {
		serverError(w, err)
		return
	}
	token := "ovtl_" + raw
	actor := ""
	if u := sessionUser(r); u != nil {
		actor = u.Username
	}
	id, err := s.db.CreateAPIKey(r.Context(), in.Name, in.Role, auth.HashToken(token), actor)
	if err != nil {
		badRequest(w, "create key: %v", err) // likely duplicate name
		return
	}
	s.audit(r, "apikey.create", in.Name, map[string]any{"id": id, "role": in.Role})
	// The raw token appears exactly here and never again.
	writeJSON(w, 201, map[string]any{"id": id, "name": in.Name, "role": in.Role, "token": token})
}

func (s *Server) deleteAPIKey(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		badRequest(w, "bad id")
		return
	}
	name, err := s.db.DeleteAPIKey(r.Context(), id)
	if err != nil {
		writeJSON(w, 404, map[string]string{"error": "unknown key"})
		return
	}
	s.audit(r, "apikey.delete", name, map[string]any{"id": id})
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// touchAPIKey updates last_used_at, throttled to once a minute.
func (s *Server) touchAPIKey(ctx context.Context, k *store.APIKey) {
	if k.LastUsedAt != nil {
		if t, err := time.Parse(time.RFC3339, *k.LastUsedAt); err == nil && time.Since(t) < time.Minute {
			return
		}
	}
	if err := s.db.TouchAPIKey(ctx, k.ID); err != nil {
		s.log.Warn("api key touch", "err", err)
	}
}
