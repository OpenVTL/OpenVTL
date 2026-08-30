package api

// v0.7 ZFS storage endpoints: device enumeration (with eligibility
// rails), one-time system-pool setup (data disks + one SSD dedup vdev),
// and per-pool dataset create/remove. Mutations are admin-gated by method
// (middleware). Setup erases the selected disks and requires "create"
// typed back; there is no force path past a disk that holds data.

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/openvtl/openvtld/internal/store"
)

func (s *Server) listDevices(w http.ResponseWriter, r *http.Request) {
	devs, err := s.stor.Devices(r.Context())
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{
		"devices": devs,
		"system":  s.stor.SystemStatus(r.Context()),
	})
}

// rescanDevices — hot-added vHDDs without a reboot: SCSI bus re-probe +
// udev settle, then the fresh enumeration.
func (s *Server) rescanDevices(w http.ResponseWriter, r *http.Request) {
	n, err := s.stor.Rescan(r.Context())
	if err != nil {
		serverError(w, err)
		return
	}
	s.audit(r, "storage.rescan", "", map[string]any{"scsi_hosts": n})
	devs, err := s.stor.Devices(r.Context())
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{
		"scsi_hosts": n,
		"devices":    devs,
		"system":     s.stor.SystemStatus(r.Context()),
	})
}

// setupStorage builds the one system zpool: data disk(s) on HDD + one SSD
// dedup vdev (holds the global dedupe table). Replaces the old shared-
// cache-device designation.
func (s *Server) setupStorage(w http.ResponseWriter, r *http.Request) {
	var in struct {
		DataDevs []string `json:"data_devs"`
		DedupDev string   `json:"dedup_dev"`
		Confirm  string   `json:"confirm"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		badRequest(w, "bad json: %v", err)
		return
	}
	if err := s.stor.SetupSystemPool(r.Context(), in.DataDevs, in.DedupDev, in.Confirm); err != nil {
		badRequest(w, "%v", err)
		return
	}
	s.audit(r, "storage.setup", "", map[string]any{"data_devs": in.DataDevs, "dedup_dev": in.DedupDev})
	writeJSON(w, 200, s.stor.SystemStatus(r.Context()))
}

func (s *Server) listPools(w http.ResponseWriter, r *http.Request) {
	pools, err := s.db.ListPools(r.Context())
	if err != nil {
		serverError(w, err)
		return
	}
	if pools == nil {
		pools = []store.Pool{}
	}
	writeJSON(w, 200, pools)
}

// createPool creates a ZFS dataset (dedup=on) in the system zpool. Just a
// name — all pools share the system disks and the one dedup vdev.
func (s *Server) createPool(w http.ResponseWriter, r *http.Request) {
	if s.drainBlocked(w) {
		return
	}
	var in struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		badRequest(w, "bad json: %v", err)
		return
	}
	poolID, err := s.stor.CreatePool(r.Context(), in.Name)
	if err != nil {
		badRequest(w, "%v", err)
		return
	}
	s.audit(r, "pool.create", in.Name, map[string]any{"pool_id": poolID})
	writeJSON(w, 201, map[string]int64{"pool_id": poolID})
}

// removePool destroys a pool's dataset. Refused while a library is paired.
// The system zpool and its disks are untouched. Admin-gated by method;
// pool name typed back.
func (s *Server) removePool(w http.ResponseWriter, r *http.Request) {
	if s.drainBlocked(w) {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		badRequest(w, "bad pool id")
		return
	}
	var in struct {
		Confirm string `json:"confirm"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		badRequest(w, "bad json: %v", err)
		return
	}
	pool, err := s.db.GetPool(r.Context(), id)
	if err != nil {
		writeJSON(w, 404, map[string]string{"error": "unknown pool"})
		return
	}
	if err := s.stor.RemovePool(r.Context(), id, in.Confirm); err != nil {
		badRequest(w, "%v", err)
		return
	}
	// Removing a pool leaves the system storage intact — freeing the disks
	// is a separate "tear down system storage" step (see teardownStorage).
	s.audit(r, "pool.remove", pool.Name, map[string]any{"pool_id": id})
	writeJSON(w, 200, map[string]any{"ok": true, "pool_id": id})
}

// teardownStorage destroys the system zpool and frees its data disk(s) —
// the deliberate "free the disks" step, valid only once every pool is
// removed. The dedupe SSD stays reserved as the permanent metadata device.
func (s *Server) teardownStorage(w http.ResponseWriter, r *http.Request) {
	if s.drainBlocked(w) {
		return
	}
	var in struct {
		Confirm string `json:"confirm"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		badRequest(w, "bad json: %v", err)
		return
	}
	if err := s.stor.TeardownSystem(r.Context(), in.Confirm); err != nil {
		badRequest(w, "%v", err)
		return
	}
	s.audit(r, "storage.teardown", "", nil)
	writeJSON(w, 200, s.stor.SystemStatus(r.Context()))
}

// growStorage expands the system zpool onto a data disk enlarged underneath
// it (the hypervisor grew the vHDD). Online, non-destructive, idempotent.
func (s *Server) growStorage(w http.ResponseWriter, r *http.Request) {
	if s.drainBlocked(w) {
		return
	}
	before, after, err := s.stor.GrowSystem(r.Context())
	if err != nil {
		badRequest(w, "%v", err)
		return
	}
	s.audit(r, "storage.grow", "", map[string]any{"before_bytes": before, "after_bytes": after})
	writeJSON(w, 200, map[string]any{
		"before_bytes": before, "after_bytes": after, "grew": after > before,
		"system": s.stor.SystemStatus(r.Context()),
	})
}
