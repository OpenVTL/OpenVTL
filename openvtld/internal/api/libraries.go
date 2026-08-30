package api

// v0.6 library lifecycle: catalog-driven creation (writes device.conf
// blocks, pending_restart until the window) and the operator-window
// Apply that restarts mhVTL + rebuilds the FC target. Libraries pair
// 1:1 with pools — the settled model: a library's home dir IS its
// pool's mountpoint, and a cart's pool is its library's pool.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"

	"github.com/openvtl/openvtld/internal/catalog"
	"github.com/openvtl/openvtld/internal/inventory"
	"github.com/openvtl/openvtld/internal/s3"
	"github.com/openvtl/openvtld/internal/store"
)

var prefixRe = regexp.MustCompile(`^[A-Z0-9]{3}$`)

// maintStep returns an ApplyLibraries step callback that streams each
// completed step to the SSE bus under a human-readable operation
// label, so the UI can show a live checklist during the multi-minute
// maintenance window. maintDone closes it out.
func (s *Server) maintStep(label string) func(string) {
	s.bus.Publish("maint_step", label, map[string]any{"label": label, "step": "started"})
	return func(step string) {
		s.bus.Publish("maint_step", label, map[string]any{"label": label, "step": step})
	}
}

func (s *Server) maintDone(label string, ok bool, detail string) {
	s.bus.Publish("maint_done", label, map[string]any{"label": label, "ok": ok, "detail": detail})
}

// maintDoneReboot closes a maintenance window that ends in a full
// appliance reboot. The SSE stream is about to drop, so the "rebooting"
// flag tells the UI to hold the window open in a "reconnecting" state and
// resolve it once the daemon comes back (see the web connection watcher),
// instead of the window vanishing and leaving the operator on a dead page.
func (s *Server) maintDoneReboot(label, detail string) {
	s.bus.Publish("maint_done", label, map[string]any{"label": label, "ok": true, "detail": detail, "rebooting": true})
}

func (s *Server) listLibraries(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.ListLibraries(r.Context())
	if err != nil {
		serverError(w, err)
		return
	}
	if rows == nil {
		rows = []store.Library{}
	}
	writeJSON(w, 200, rows)
}

func (s *Server) createLibrary(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name         string `json:"name"` // display name; defaults to the serial
		Product      string `json:"product"`
		DriveProduct string `json:"drive_product"`
		NumDrives    int    `json:"num_drives"`
		NumSlots     int    `json:"num_slots"` // 0 = engine default (100)
		NumMAP       int    `json:"num_map"`   // 0 = engine default (4)
		LabelPrefix  string `json:"label_prefix"`
		PoolID       int64  `json:"pool_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		badRequest(w, "bad json: %v", err)
		return
	}
	if !prefixRe.MatchString(in.LabelPrefix) {
		badRequest(w, "label_prefix must be exactly three A-Z/0-9 characters (barcode prefix)")
		return
	}
	if in.NumSlots != 0 && (in.NumSlots < 1 || in.NumSlots > 400) {
		badRequest(w, "num_slots must be 1-400")
		return
	}
	// I/E element addresses start at 768 and slots at 1024 (Patch 1), so
	// the MAP count has a hard ceiling; 32 is the sane operational bound.
	if in.NumMAP != 0 && (in.NumMAP < 1 || in.NumMAP > 32) {
		badRequest(w, "num_map must be 1-32")
		return
	}
	pool, err := s.db.GetPool(r.Context(), in.PoolID)
	if err != nil {
		badRequest(w, "pool %d not found", in.PoolID)
		return
	}
	if pool.State != store.PoolActive {
		badRequest(w, "pool %s is %s — only active pools can home a library", pool.Name, pool.State)
		return
	}
	// 1:1 pairing (settled): one library per pool.
	if libs, err := s.db.ListLibraries(r.Context()); err == nil {
		for _, l := range libs {
			if l.HomePool == pool.ID {
				badRequest(w, "pool %s is already home to library %s", pool.Name, l.Name)
				return
			}
		}
	}

	libID, serial, driveSerials, err := s.inv.CreateLibrary(r.Context(), inventory.CreateLibrarySpec{
		Product: in.Product, DriveProduct: in.DriveProduct,
		NumDrives: in.NumDrives, HomeDir: pool.Mountpoint,
		NumSlots: in.NumSlots, NumMAP: in.NumMAP,
	})
	if err != nil {
		badRequest(w, "%v", err)
		return
	}
	name := in.Name
	if name == "" {
		name = serial
	}
	model, variant, _ := inventory.LibraryVariantByProduct(in.Product)
	if err := s.db.CreateLibrary(r.Context(), store.Library{
		ID: libID, Name: name, Vendor: model.Vendor, Product: in.Product,
		Variant: variant.Display, Serial: serial,
		DriveModel: in.DriveProduct, NumDrives: in.NumDrives,
		LabelPrefix: in.LabelPrefix, MediaDir: pool.Mountpoint,
		HomePool: pool.ID, State: store.LibraryPendingRestart,
	}); err != nil {
		serverError(w, err)
		return
	}
	s.audit(r, "library.create", serial, map[string]any{
		"library": libID, "name": name, "product": in.Product, "drives": in.NumDrives,
		"drive_model": in.DriveProduct, "pool": pool.Name, "prefix": in.LabelPrefix,
		"slots": in.NumSlots, "map": in.NumMAP,
	})
	writeJSON(w, 201, map[string]any{
		"library": libID, "serial": serial, "drive_serials": driveSerials,
		"state": store.LibraryPendingRestart,
		"note":  "declared in device.conf — activation requires the Apply maintenance window (mhVTL restart + FC rebuild + operator vary off/on)",
	})
}

// deleteLibrary removes a library, its drives AND every cartridge on
// it in one operation (operator decision: cascade — per-cart deletion
// first was too cumbersome; drives are never modified independently).
// Double acknowledgement: the library name plus the literal
// "I understand". Copies in S3 are never touched. A live library
// needs the full data-plane restart to stop serving — host sessions
// drop, maintenance window.
func (s *Server) deleteLibrary(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("lib"))
	if err != nil {
		badRequest(w, "bad library id")
		return
	}
	var in struct {
		Confirm     string `json:"confirm"`
		Acknowledge string `json:"acknowledge"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		badRequest(w, "bad json: %v", err)
		return
	}
	row, err := s.db.GetLibrary(r.Context(), id)
	if err != nil {
		writeJSON(w, 404, map[string]string{"error": "unknown library"})
		return
	}
	if in.Confirm != row.Name && in.Confirm != row.Serial {
		badRequest(w, `confirmation required: {"confirm":"%s"} must match the library name`, row.Name)
		return
	}
	if in.Acknowledge != "I understand" {
		badRequest(w, `acknowledgement required: {"acknowledge":"I understand"} — deletion erases every cartridge on the library`)
		return
	}
	if s.drainBlocked(w) {
		return
	}
	// The whole operation must survive the client disconnecting — a
	// browser giving up mid-apply otherwise cancels the context and
	// kills the restart sequence half-way (2026-07-05 incident: mtx/
	// sg_vpd children died with "signal: killed" and DB cleanup never
	// ran). Values (session identity) are kept; cancellation is not.
	ctx := context.WithoutCancel(r.Context())
	if jobs, err := s.db.UnfinishedJobs(ctx); err != nil {
		serverError(w, err)
		return
	} else if len(jobs) > 0 {
		badRequest(w, "preflight: %d active/queued job(s) — wait or cancel first", len(jobs))
		return
	}
	snapLib, inSnap := s.inv.Snapshot().LibraryByID(id)
	live := inSnap && snapLib.Library.Live
	// Cart labels from snapshot AND DB — a retry after a partial
	// failure may find the snapshot has already forgotten the library.
	labelSet := map[string]bool{}
	for _, c := range snapLib.Carts {
		labelSet[c.Label] = true
	}
	if dbLabels, err := s.db.CartLabelsByLibrary(ctx, id); err == nil {
		for _, l := range dbLabels {
			labelSet[l] = true
		}
	}
	var labels []string
	for l := range labelSet {
		labels = append(labels, l)
	}

	if err := s.inv.RemoveLibrary(ctx, id); err != nil && !errors.Is(err, inventory.ErrNotDeclared) {
		badRequest(w, "%v", err)
		return
	}
	// ErrNotDeclared = finishing an interrupted deletion (the orphan
	// case): the config is already clean and nothing is served.
	s.audit(r, "library.delete", row.Name, map[string]any{
		"library": id, "serial": row.Serial, "live": live, "cartridges": len(labels)})

	// The maintenance overlay opens BEFORE the slow work: purging tens
	// of cartridges on a dedup pool grinds for minutes (per-block DDT
	// updates — observed ~8 min for 40 carts on a dedup pool)
	// and the operator otherwise stares at a silent UI wondering if
	// the delete wedged. Only the live path gets the overlay — a
	// pending library was never served and has nothing slow to purge.
	label := "Deleting library " + row.Name
	var onStep func(string)
	if live {
		onStep = s.maintStep(label)
		onStep("removed from configuration")
		if len(labels) > 0 {
			onStep("erasing " + strconv.Itoa(len(labels)) + " cartridge(s) — freeing deduplicated media can take minutes")
		}
	}
	// Cleanup runs for every case, before any reboot/reload so the
	// persisted state is consistent: erase the cart media + rows, the
	// config remnants and the DB row. S3 exports and the catalog stay —
	// the bucket is the record.
	var progress func(string, int, int)
	if onStep != nil {
		progress = func(l string, done, total int) {
			// Every cart for small sets; every 5th (plus the last) for
			// big ones — the overlay stays informative, not scrolling.
			if total <= 12 || done%5 == 0 || done == total {
				onStep("erased " + l + " (" + strconv.Itoa(done) + "/" + strconv.Itoa(total) + ")")
			}
		}
	}
	removed, skipped := s.inv.PurgeMedia(row.MediaDir, labels, progress)
	for _, l := range labels {
		if err := s.db.DeleteCartridge(ctx, l); err != nil {
			s.log.Warn("cartridge row delete", "label", l, "err", err)
		}
	}
	s.inv.RemoveLibraryContents(id)
	if err := s.db.DeleteLibrary(ctx, id); err != nil && err != store.ErrNotFound {
		serverError(w, err)
		return
	}

	if live {
		// A live library's mhVTL device can only be released cleanly by
		// a reboot: the generic sysfs delete corrupts mhVTL's kernel LU
		// list and oopses the box (2026-07-05), and an in-place mhVTL
		// restart leaves the removed library's daemonless device dead,
		// wedging discovery. Everything above is already persisted, so
		// the fresh boot serves only the survivors and never re-creates
		// the removed device. Host sessions drop at the reboot — the
		// maintenance window the UI double-confirms.
		onStep("erased " + strconv.Itoa(removed) + " cartridge(s)")
		onStep("rebooting to release the tape devices — the appliance restarts now")
		s.maintDoneReboot(label, "Rebooting to finish — the appliance will be back in a minute or two.")
		s.log.Warn("library delete: rebooting to release removed devices", "library", id, "name", row.Name)
		s.rebootAppliance()
		writeJSON(w, 200, map[string]any{
			"ok": true, "library": id, "name": row.Name,
			"cartridges_deleted": removed, "skipped": skipped, "rebooting": true,
		})
		return
	}

	// Pending (never served) or orphan retry: nothing was registered in
	// the kernel, so no reboot — just re-scan so the snapshot forgets it.
	if err := s.inv.Reload(ctx); err != nil {
		s.log.Warn("engine reload after library-delete cleanup", "err", err)
	}
	writeJSON(w, 200, map[string]any{
		"ok": true, "library": id, "name": row.Name,
		"cartridges_deleted": removed, "skipped": skipped, "rebooting": false,
	})
}

// applyLibraries runs the daemon half of the maintenance window. The
// FC rebuild drops every host session — hence the typed confirmation
// and the no-jobs/idle-drives preflight.
func (s *Server) applyLibraries(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Confirm string `json:"confirm"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Confirm != "apply" {
		badRequest(w, `confirmation required: {"confirm":"apply"} — this restarts mhVTL and rebuilds the FC target (drops host sessions)`)
		return
	}
	if jobs, err := s.db.UnfinishedJobs(r.Context()); err != nil {
		serverError(w, err)
		return
	} else if len(jobs) > 0 {
		badRequest(w, "preflight: %d active/queued job(s) — wait or cancel first", len(jobs))
		return
	}
	s.audit(r, "library.apply", "", nil)
	// Survive client disconnects — see deleteLibrary. A browser giving
	// up mid-restart must never kill the sequence half-way.
	ctx := context.WithoutCancel(r.Context())
	label := "Activating libraries"
	res, err := s.fc.ApplyLibraries(ctx, s.inv, s.maintStep(label))
	if err != nil {
		s.maintDone(label, false, err.Error())
		s.log.Error("apply-libraries failed", "err", err, "steps", res.Steps)
		writeJSON(w, 500, map[string]any{"error": err.Error(), "steps": res.Steps})
		return
	}
	s.maintDone(label, true, "libraries activated")
	// Every library the engine now sees live flips active in the DB.
	for _, l := range s.inv.Snapshot().Libraries {
		if l.Library.Live {
			if err := s.db.SetLibraryState(ctx, l.Library.ID, store.LibraryActive); err != nil && err != store.ErrNotFound {
				s.log.Warn("library state flip", "library", l.Library.ID, "err", err)
			}
		}
	}
	writeJSON(w, 200, res)
}

// recoverLibrary is one-click DR (Phase B): read a library's topology.json
// from S3, recreate it on a chosen pool adopting the original serial, run
// the Apply window, then queue an import of every cart it held. The cart
// labels are preserved; the operator re-points the IBM i RSC/DEVD at the
// recovered library afterward.
func (s *Server) recoverLibrary(w http.ResponseWriter, r *http.Request) {
	if s.drainBlocked(w) {
		return
	}
	var in struct {
		RemoteID      int64  `json:"remote_id"`
		SystemName    string `json:"system_name"`
		LibrarySerial string `json:"library_serial"`
		PoolID        int64  `json:"pool_id"`
		Name          string `json:"name,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		badRequest(w, "bad json: %v", err)
		return
	}
	if in.SystemName == "" || in.LibrarySerial == "" {
		badRequest(w, "system_name and library_serial are required")
		return
	}
	remote, err := s.db.GetRemote(r.Context(), in.RemoteID)
	if err != nil {
		badRequest(w, "unknown remote %d", in.RemoteID)
		return
	}
	pool, err := s.db.GetPool(r.Context(), in.PoolID)
	if err != nil {
		badRequest(w, "pool %d not found", in.PoolID)
		return
	}
	if pool.State != store.PoolActive {
		badRequest(w, "pool %s is %s — only active pools can home a library", pool.Name, pool.State)
		return
	}
	if libs, err := s.db.ListLibraries(r.Context()); err == nil {
		for _, l := range libs {
			if l.HomePool == pool.ID {
				badRequest(w, "pool %s is already home to library %s — recover onto a free pool", pool.Name, l.Name)
				return
			}
			if l.Serial == in.LibrarySerial {
				badRequest(w, "library %s already exists here", in.LibrarySerial)
				return
			}
		}
	}
	// Apply restarts mhVTL, so the same idle preflight as a manual Apply.
	if jobs, err := s.db.UnfinishedJobs(r.Context()); err != nil {
		serverError(w, err)
		return
	} else if len(jobs) > 0 {
		badRequest(w, "preflight: %d active/queued job(s) — wait or cancel first", len(jobs))
		return
	}

	cl, err := s3.New(remote)
	if err != nil {
		serverError(w, err)
		return
	}
	raw, err := cl.GetBytes(r.Context(), cl.Key(catalog.TopologyKeyParts(in.SystemName, in.LibrarySerial)...))
	if err != nil {
		badRequest(w, "no topology for %s/%s in the bucket — recovery needs a library exported by v0.7+ (re-export a cart to write it), or rebuild the catalog", in.SystemName, in.LibrarySerial)
		return
	}
	topo, err := catalog.DecodeTopology(raw)
	if err != nil {
		badRequest(w, "%v", err)
		return
	}

	// Newest generation per cart label under this system+library.
	entries, err := s.db.ListCatalog(r.Context(), in.RemoteID)
	if err != nil {
		serverError(w, err)
		return
	}
	latest := map[string]string{}
	for _, e := range entries {
		if e.SystemName == in.SystemName && e.LibrarySerial == in.LibrarySerial {
			if g, ok := latest[e.CartLabel]; !ok || e.Generation > g {
				latest[e.CartLabel] = e.Generation
			}
		}
	}

	libID, serial, driveSerials, err := s.inv.CreateLibrary(r.Context(), inventory.CreateLibrarySpec{
		Product: topo.Product, DriveProduct: topo.DriveProduct,
		NumDrives: topo.NumDrives, HomeDir: pool.Mountpoint,
		NumSlots: topo.NumSlots, NumMAP: topo.NumMAP, Serial: in.LibrarySerial,
	})
	if err != nil {
		badRequest(w, "%v", err)
		return
	}
	name := in.Name
	if name == "" {
		name = topo.LibraryName
	}
	if name == "" {
		name = serial
	}
	prefix := topo.LabelPrefix
	if !prefixRe.MatchString(prefix) {
		prefix = "OVT"
	}
	model, variant, _ := inventory.LibraryVariantByProduct(topo.Product)
	if err := s.db.CreateLibrary(r.Context(), store.Library{
		ID: libID, Name: name, Vendor: model.Vendor, Product: topo.Product,
		Variant: variant.Display, Serial: serial,
		DriveModel: topo.DriveProduct, NumDrives: topo.NumDrives,
		LabelPrefix: prefix, MediaDir: pool.Mountpoint,
		HomePool: pool.ID, State: store.LibraryPendingRestart,
	}); err != nil {
		serverError(w, err)
		return
	}
	s.audit(r, "library.recover", serial, map[string]any{
		"library": libID, "name": name, "system": in.SystemName, "carts": len(latest), "pool": pool.Name,
	})

	// Apply (maintenance window); survive a client disconnect like the
	// manual Apply / delete paths.
	ctx := context.WithoutCancel(r.Context())
	step := "Recovering library " + name
	res, err := s.fc.ApplyLibraries(ctx, s.inv, s.maintStep(step))
	if err != nil {
		s.maintDone(step, false, err.Error())
		s.log.Error("recover: apply failed", "err", err, "steps", res.Steps)
		// The library row exists (pending_restart). Still create the
		// import jobs so the recovery is resumable: once the operator
		// gets the library live (re-Apply, or reboot), Jobs → Retry
		// finishes the recovery instead of leaving a dead end.
		jobIDs := s.enqueueRecoverImports(ctx, latest, in.RemoteID, in.SystemName, libID)
		writeJSON(w, 500, map[string]any{
			"error": err.Error(), "steps": res.Steps, "library": libID,
			"import_jobs": jobIDs,
		})
		return
	}
	for _, l := range s.inv.Snapshot().Libraries {
		if l.Library.Live {
			if err := s.db.SetLibraryState(ctx, l.Library.ID, store.LibraryActive); err != nil && err != store.ErrNotFound {
				s.log.Warn("library state flip", "library", l.Library.ID, "err", err)
			}
		}
	}
	s.maintDone(step, true, "library activated — importing cartridges")

	// Queue a foreign import of every cart's newest generation into the
	// recovered library. They run in the background; the operator watches Jobs.
	jobIDs := s.enqueueRecoverImports(ctx, latest, in.RemoteID, in.SystemName, libID)

	writeJSON(w, 201, map[string]any{
		"library": libID, "serial": serial, "name": name,
		"drive_serials": driveSerials, "carts": len(latest), "import_jobs": jobIDs,
		"steps": res.Steps,
	})
}

// enqueueRecoverImports creates and enqueues one foreign-import job per
// recovered cart (newest generation each). Called on the recover happy
// path AND after a failed Apply — a failed job with Retry in the Jobs
// view is the resume path; silently dropping the imports would leave the
// recovery unfinishable from the UI.
func (s *Server) enqueueRecoverImports(ctx context.Context, latest map[string]string, remoteID int64, systemName string, libID int) []int64 {
	jobIDs := []int64{}
	for lbl, gen := range latest {
		j, err := s.db.CreateJob(ctx, "import", lbl, &remoteID, gen, "queued", "recover")
		if err != nil {
			s.log.Warn("recover: create import job", "label", lbl, "err", err)
			continue
		}
		if err := s.db.SetJobImportTarget(ctx, j.ID, systemName, int64(libID)); err != nil {
			s.log.Warn("recover: set import target", "job", j.ID, "err", err)
		}
		if err := s.runner.Enqueue(j.ID); err != nil {
			s.log.Warn("recover: enqueue import", "job", j.ID, "err", err)
			continue
		}
		jobIDs = append(jobIDs, j.ID)
	}
	return jobIDs
}
