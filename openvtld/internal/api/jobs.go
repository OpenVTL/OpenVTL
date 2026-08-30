package api

// v0.4: the first mutating routes. Every mutation lands in audit_log
// with the caller's address; secrets go in, never come back out.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/openvtl/openvtld/internal/catalog"
	"github.com/openvtl/openvtld/internal/export"
	"github.com/openvtl/openvtld/internal/orchestrate"
	"github.com/openvtl/openvtld/internal/s3"
	"github.com/openvtl/openvtld/internal/store"
)

func (s *Server) audit(r *http.Request, action, subject string, params any) {
	p := ""
	if params != nil {
		if b, err := json.Marshal(params); err == nil {
			p = string(b)
		}
	}
	// Actor = session user (v0.5). Falls back to the remote address for
	// anything auditable that can still happen anonymously.
	actor := r.RemoteAddr
	if u := sessionUser(r); u != nil {
		actor = u.Username
	}
	if err := s.db.Audit(r.Context(), actor, r.RemoteAddr, action, subject, p); err != nil {
		s.log.Warn("audit write failed", "action", action, "err", err)
	}
}

func badRequest(w http.ResponseWriter, format string, a ...any) {
	writeJSON(w, 400, map[string]string{"error": fmt.Sprintf(format, a...)})
}

func serverError(w http.ResponseWriter, err error) {
	writeJSON(w, 500, map[string]string{"error": err.Error()})
}

// --- remotes ---

// remoteIn is the create/update payload. The secret is write-only;
// empty secret on update keeps the stored one.
type remoteIn struct {
	Name      string `json:"name"`
	Endpoint  string `json:"endpoint"`
	Region    string `json:"region"`
	Bucket    string `json:"bucket"`
	Prefix    string `json:"prefix"`
	AccessKey string `json:"access_key"`
	SecretKey string `json:"secret_key"`
	UseSSL    *bool  `json:"use_ssl"`
	PathStyle bool   `json:"path_style"`
}

func (in *remoteIn) toRemote() (*store.Remote, error) {
	if in.Name == "" || in.Bucket == "" || in.AccessKey == "" {
		return nil, fmt.Errorf("name, bucket and access_key are required")
	}
	r := &store.Remote{
		Name: in.Name, Endpoint: in.Endpoint, Region: in.Region, Bucket: in.Bucket,
		Prefix: in.Prefix, AccessKey: in.AccessKey, SecretKey: in.SecretKey,
		UseSSL: true, PathStyle: in.PathStyle,
	}
	if r.Endpoint == "" {
		r.Endpoint = "s3.amazonaws.com"
	}
	if in.UseSSL != nil {
		r.UseSSL = *in.UseSSL
	}
	return r, nil
}

// remoteOut adds has_secret so the UI can show state without the value.
type remoteOut struct {
	store.Remote
	HasSecret bool `json:"has_secret"`
}

func redact(r store.Remote) remoteOut {
	has := r.SecretKey != ""
	r.SecretKey = ""
	return remoteOut{Remote: r, HasSecret: has}
}

func (s *Server) listRemotes(w http.ResponseWriter, r *http.Request) {
	rs, err := s.db.ListRemotes(r.Context())
	if err != nil {
		serverError(w, err)
		return
	}
	out := make([]remoteOut, 0, len(rs))
	for _, rem := range rs {
		out = append(out, redact(rem))
	}
	writeJSON(w, 200, out)
}

func (s *Server) createRemote(w http.ResponseWriter, r *http.Request) {
	var in remoteIn
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		badRequest(w, "bad json: %v", err)
		return
	}
	rem, err := in.toRemote()
	if err != nil {
		badRequest(w, "%v", err)
		return
	}
	if rem.SecretKey == "" {
		badRequest(w, "secret_key is required on create")
		return
	}
	id, err := s.db.CreateRemote(r.Context(), rem)
	if err != nil {
		serverError(w, err)
		return
	}
	s.audit(r, "remote.create", rem.Name, map[string]any{"id": id, "bucket": rem.Bucket, "endpoint": rem.Endpoint})
	created, err := s.db.GetRemote(r.Context(), id)
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, 201, redact(*created))
}

func (s *Server) updateRemote(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		badRequest(w, "bad id")
		return
	}
	var in remoteIn
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		badRequest(w, "bad json: %v", err)
		return
	}
	rem, err := in.toRemote()
	if err != nil {
		badRequest(w, "%v", err)
		return
	}
	rem.ID = id
	if err := s.db.UpdateRemote(r.Context(), rem); err != nil {
		serverError(w, err)
		return
	}
	s.audit(r, "remote.update", rem.Name, map[string]any{"id": id})
	updated, err := s.db.GetRemote(r.Context(), id)
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, 200, redact(*updated))
}

func (s *Server) deleteRemote(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		badRequest(w, "bad id")
		return
	}
	if err := s.db.DeleteRemote(r.Context(), id); err != nil {
		serverError(w, err)
		return
	}
	if rs, err := s.db.ListRemotes(r.Context()); err == nil && len(rs) == 0 {
		if err := s.db.SetSetting(r.Context(), settingNoOffsiteNag, ""); err != nil {
			s.log.Warn("reset no-offsite nag", "err", err)
		}
	}
	s.audit(r, "remote.delete", r.PathValue("id"), nil)
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) testRemote(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		badRequest(w, "bad id")
		return
	}
	rem, err := s.db.GetRemote(r.Context(), id)
	if err != nil {
		badRequest(w, "unknown remote %d", id)
		return
	}
	s.audit(r, "remote.test", rem.Name, nil)
	detail := ""
	ok := false
	if cl, cerr := s3.New(rem); cerr != nil {
		detail = cerr.Error()
	} else if d, terr := cl.Test(r.Context()); terr != nil {
		detail = terr.Error()
	} else {
		ok, detail = true, d
	}
	if err := s.db.RecordRemoteTest(r.Context(), id, ok, detail); err != nil {
		s.log.Warn("remote test record", "err", err)
	}
	writeJSON(w, 200, map[string]any{"ok": ok, "detail": detail})
}

// --- jobs ---

func (s *Server) listJobs(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if l, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && l > 0 && l <= 1000 {
		limit = l
	}
	jobs, err := s.db.ListJobs(r.Context(), limit)
	if err != nil {
		serverError(w, err)
		return
	}
	if jobs == nil {
		jobs = []store.Job{}
	}
	writeJSON(w, 200, jobs)
}

// searchJobs runs a deep query over the whole job table — a free-text match
// (q) across id/kind/state/label/trigger/generation/system/error, newest
// first. The Jobs page uses it to reach past the recent window it loads.
func (s *Server) searchJobs(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	limit := 500
	if l, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && l > 0 {
		limit = l
	}
	jobs, err := s.db.SearchJobs(r.Context(), q, limit)
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, 200, jobs)
}

func (s *Server) getJob(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		badRequest(w, "bad id")
		return
	}
	j, err := s.db.GetJob(r.Context(), id)
	if err != nil {
		writeJSON(w, 404, map[string]string{"error": "unknown job"})
		return
	}
	evs, err := s.db.JobEvents(r.Context(), id)
	if err != nil {
		serverError(w, err)
		return
	}
	chunks, _ := s.db.ChunksForJob(r.Context(), id)
	writeJSON(w, 200, map[string]any{"job": j, "events": evs, "chunks": chunks})
}

func (s *Server) retryJob(w http.ResponseWriter, r *http.Request) {
	if s.drainBlocked(w) {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		badRequest(w, "bad id")
		return
	}
	j, err := s.db.GetJob(r.Context(), id)
	if err != nil {
		writeJSON(w, 404, map[string]string{"error": "unknown job"})
		return
	}
	if j.State != "failed" && j.State != "cancelled" {
		badRequest(w, "job %d is %s — only failed/cancelled jobs retry", id, j.State)
		return
	}
	// Storage jobs are not resumable in place — LVM work can't be blindly
	// replayed (the runner doesn't handle these kinds at all). Recovery is
	// to remove the pool and create it again from the Storage view.
	if j.Kind == "pool_create" || j.Kind == "pool_remove" {
		badRequest(w, "storage jobs can't be retried — remove the pool in the Storage view, then create it again")
		return
	}
	initial := "queued"
	if j.Kind == "export" {
		initial = "detected"
	}
	s.db.SetJobError(r.Context(), id, "")
	if err := s.db.TransitionJob(r.Context(), id, j.State, initial, "retried by operator"); err != nil {
		serverError(w, err)
		return
	}
	s.audit(r, "job.retry", j.CartLabel, map[string]any{"id": id, "kind": j.Kind})
	if err := s.runner.Enqueue(id); err != nil {
		serverError(w, err)
		return
	}
	j, _ = s.db.GetJob(r.Context(), id)
	writeJSON(w, 200, j)
}

func (s *Server) cancelJob(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		badRequest(w, "bad id")
		return
	}
	s.audit(r, "job.cancel", r.PathValue("id"), nil)
	if err := s.runner.Cancel(r.Context(), id); err != nil {
		badRequest(w, "%v", err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// --- cart actions ---

type cartActionIn struct {
	RemoteID   int64  `json:"remote_id"`
	Generation string `json:"generation,omitempty"`
	// Import-only: pulling a FOREIGN cart (another instance's export in a
	// shared bucket) requires its system name (to disambiguate the
	// manifest) and the local library to slot it into. Omitted for a
	// same-system re-import of an evicted cart.
	SystemName    string `json:"system_name,omitempty"`
	TargetLibrary int64  `json:"target_library,omitempty"`
}

func (s *Server) knownCart(label string) bool {
	_, _, ok := s.inv.Snapshot().FindCart(label)
	return ok
}

func (s *Server) startCartJob(w http.ResponseWriter, r *http.Request, kind string) {
	if s.drainBlocked(w) {
		return
	}
	label := r.PathValue("label")
	var in cartActionIn
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		badRequest(w, "bad json: %v", err)
		return
	}
	if in.RemoteID == 0 {
		badRequest(w, "remote_id is required")
		return
	}
	if _, err := s.db.GetRemote(r.Context(), in.RemoteID); err != nil {
		badRequest(w, "unknown remote %d", in.RemoteID)
		return
	}
	if in.Generation != "" && !catalog.ValidGeneration(in.Generation) {
		badRequest(w, "malformed generation %q", in.Generation)
		return
	}

	// targetLib > 0 marks a foreign / explicit-destination import: the cart
	// is slotted into that local library (Phase A cross-instance import).
	var targetLib int64
	if kind == "import" {
		if in.Generation == "" {
			badRequest(w, "generation is required for import")
			return
		}
		// Foreign when the caller names a destination/system or the label
		// simply isn't a local cart. A same-system re-import of a known
		// (evicted) cart takes neither and uses the in-place restore path.
		if in.TargetLibrary != 0 || in.SystemName != "" || !s.knownCart(label) {
			targetLib = in.TargetLibrary
			if targetLib == 0 {
				live := []int{}
				for _, l := range s.inv.Snapshot().Libraries {
					if l.Library.Live {
						live = append(live, l.Library.ID)
					}
				}
				if len(live) != 1 {
					badRequest(w, "target_library is required (%d live libraries)", len(live))
					return
				}
				targetLib = int64(live[0])
			}
			lib, ok := s.inv.Snapshot().LibraryByID(int(targetLib))
			if !ok || !lib.Library.Live {
				badRequest(w, "target library %d is not live", targetLib)
				return
			}
			if in.SystemName == "" {
				badRequest(w, "system_name is required to import a cartridge into a library")
				return
			}
			if _, err := s.db.GetCatalogEntry(r.Context(), in.RemoteID, in.SystemName, label, in.Generation); err != nil {
				badRequest(w, "no catalog entry for %s/%s generation %s — rebuild from the bucket first", in.SystemName, label, in.Generation)
				return
			}
			// Label collision: a cart with this barcode already exists
			// locally. If it's an evicted stub, this is really a same-system
			// re-import — restore it in place. Otherwise refuse: two carts
			// can't share a barcode.
			if s.knownCart(label) {
				metas, _ := s.db.CartMetas(r.Context())
				if metas[label].LocalState == "evicted" {
					targetLib, in.SystemName = 0, ""
				} else {
					writeJSON(w, 409, map[string]string{"error": fmt.Sprintf("%s already exists on this appliance — delete it or import under a different label", label)})
					return
				}
			}
		} else if !s.knownCart(label) {
			badRequest(w, "unknown cartridge %s", label)
			return
		}
	} else if !s.knownCart(label) {
		// export / evict can't target a label the library doesn't know.
		badRequest(w, "unknown cartridge %s", label)
		return
	}

	initial := "queued"
	if kind == "export" {
		initial = "detected"
	}
	j, err := s.db.CreateJob(r.Context(), kind, label, &in.RemoteID, in.Generation, initial, "manual")
	if err != nil {
		serverError(w, err)
		return
	}
	if targetLib != 0 {
		if err := s.db.SetJobImportTarget(r.Context(), j.ID, in.SystemName, targetLib); err != nil {
			serverError(w, err)
			return
		}
		j.SystemName, j.TargetLibrary = in.SystemName, &targetLib
	}
	s.audit(r, "job.create."+kind, label, map[string]any{"job": j.ID, "remote": in.RemoteID, "generation": in.Generation, "system": in.SystemName, "target_library": targetLib})
	if err := s.runner.Enqueue(j.ID); err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, 201, j)
}

func (s *Server) exportCart(w http.ResponseWriter, r *http.Request) { s.startCartJob(w, r, "export") }
func (s *Server) evictCart(w http.ResponseWriter, r *http.Request)  { s.startCartJob(w, r, "evict") }
func (s *Server) importCart(w http.ResponseWriter, r *http.Request) { s.startCartJob(w, r, "import") }

// --- catalog ---

func (s *Server) listCatalog(w http.ResponseWriter, r *http.Request) {
	remoteID, err := strconv.ParseInt(r.URL.Query().Get("remote_id"), 10, 64)
	if err != nil {
		badRequest(w, "remote_id query param required")
		return
	}
	entries, err := s.db.ListCatalog(r.Context(), remoteID)
	if err != nil {
		serverError(w, err)
		return
	}
	if entries == nil {
		entries = []store.CatalogEntry{}
	}
	writeJSON(w, 200, entries)
}

func (s *Server) rebuildCatalog(w http.ResponseWriter, r *http.Request) {
	var in struct {
		RemoteID int64 `json:"remote_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.RemoteID == 0 {
		badRequest(w, "remote_id is required")
		return
	}
	rem, err := s.db.GetRemote(r.Context(), in.RemoteID)
	if err != nil {
		badRequest(w, "unknown remote %d", in.RemoteID)
		return
	}
	s.audit(r, "catalog.rebuild", rem.Name, nil)
	cl, err := s3.New(rem)
	if err != nil {
		serverError(w, err)
		return
	}
	res, err := catalog.Rebuild(r.Context(), s.db, cl, in.RemoteID, s.log)
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, 200, res)
}

// --- raw bucket browser (Offsite card "Raw") ---

// listBucketObjects returns every object under the remote's prefix, keyed
// relative to it — the frontend builds the folder tree.
func (s *Server) listBucketObjects(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		badRequest(w, "bad id")
		return
	}
	rem, err := s.db.GetRemote(r.Context(), id)
	if err != nil {
		badRequest(w, "unknown remote %d", id)
		return
	}
	cl, err := s3.New(rem)
	if err != nil {
		serverError(w, err)
		return
	}
	objs, err := cl.ListAllObjects(r.Context())
	if err != nil {
		serverError(w, err)
		return
	}
	if objs == nil {
		objs = []s3.ObjectInfo{}
	}
	writeJSON(w, 200, map[string]any{"objects": objs})
}

// deleteBucketPrefix removes a whole folder from the bucket (admin-gated
// by method). Folder-only: the prefix must end with "/", which structurally
// blocks deleting an individual chunk/manifest object (their keys never do).
func (s *Server) deleteBucketPrefix(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		badRequest(w, "bad id")
		return
	}
	var in struct {
		Prefix string `json:"prefix"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		badRequest(w, "bad json: %v", err)
		return
	}
	if in.Prefix == "" || !strings.HasSuffix(in.Prefix, "/") {
		badRequest(w, "prefix must be a folder path ending in / — individual objects can't be deleted here")
		return
	}
	rem, err := s.db.GetRemote(r.Context(), id)
	if err != nil {
		badRequest(w, "unknown remote %d", id)
		return
	}
	cl, err := s3.New(rem)
	if err != nil {
		serverError(w, err)
		return
	}
	n, err := cl.RemovePrefix(r.Context(), in.Prefix)
	if err != nil {
		serverError(w, err)
		return
	}
	s.audit(r, "bucket.delete_prefix", rem.Name, map[string]any{"prefix": in.Prefix, "deleted": n})
	writeJSON(w, 200, map[string]any{"deleted": n})
}

// --- settings + audit ---

// settableKeys is the closed set of UI-editable settings.
// settingNoOffsiteNag: "1" hides the dashboard warning shown when no S3
// remote exists (v0.7: S3 is optional, but running without offsite
// export means losing the box loses the backups on it). Cleared
// automatically when the last remote is deleted so the nag returns.
const settingNoOffsiteNag = "nag.no_offsite_dismissed"

var settableKeys = map[string]bool{
	export.SettingIEWatcher:     true,
	export.SettingDefaultRemote: true,
	export.SettingEvictPct:      true,
	export.SettingMinting:       true,
	settingNoOffsiteNag:         true,
	// fc.disabled_ports: comma-sep port WWNs excluded from serving —
	// normally managed by the Targets port toggle.
	orchestrate.SettingDisabledPorts: true,
	// apikeys.enabled: master switch for Bearer-token auth;
	// off means presented keys are ignored wholesale.
	settingAPIKeysEnabled: true,
	// system.name: this instance's friendly name — the <system> segment
	// of the S3 key layout (validated path-safe below).
	store.SettingSystemName: true,
}

func (s *Server) getSettings(w http.ResponseWriter, r *http.Request) {
	out := map[string]string{}
	for k := range settableKeys {
		out[k] = s.db.Setting(r.Context(), k, "")
	}
	writeJSON(w, 200, out)
}

func (s *Server) putSettings(w http.ResponseWriter, r *http.Request) {
	var in map[string]string
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		badRequest(w, "bad json: %v", err)
		return
	}
	for k, v := range in {
		if !settableKeys[k] {
			badRequest(w, "unknown setting %q", k)
			return
		}
		v = strings.TrimSpace(v)
		if k == store.SettingSystemName && v != "" && !catalog.ValidSystemName(v) {
			badRequest(w, "system name must be 1-32 chars: lowercase letters, digits, - or _ (must start with a letter or digit)")
			return
		}
		if err := s.db.SetSetting(r.Context(), k, v); err != nil {
			serverError(w, err)
			return
		}
	}
	s.audit(r, "settings.update", "", in)
	s.getSettings(w, r)
}

func (s *Server) recentAudit(w http.ResponseWriter, r *http.Request) {
	entries, err := s.db.RecentAudit(r.Context(), 200)
	if err != nil {
		serverError(w, err)
		return
	}
	if entries == nil {
		entries = []store.AuditEntry{}
	}
	writeJSON(w, 200, entries)
}
