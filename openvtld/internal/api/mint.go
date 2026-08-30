package api

// POST /api/cartridges — zero-restart cart creation (v0.5 media ops,
// library-scoped since v0.6). Synchronous: the whole mktape → MAP →
// park sequence is seconds, and making it a job kind would force a
// rebuild of the job table's CHECK constraint for no operational gain.
// The IE watcher is suppressed for the label while it transits the MAP
// — the transit is exactly what a vault move looks like, and the
// watcher is LIVE in the field.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/openvtl/openvtld/internal/inventory"
	"github.com/openvtl/openvtld/internal/store"
)

func (s *Server) mintCart(w http.ResponseWriter, r *http.Request) {
	if s.drainBlocked(w) {
		return
	}
	var in struct {
		Library int    `json:"library"` // 0 = the sole live library
		Label   string `json:"label"`   // empty = autogenerate; count>1 requires empty
		Count   int    `json:"count"`   // 0/1 = single; batch labels are auto-sequenced
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		badRequest(w, "bad json: %v", err)
		return
	}
	if in.Count == 0 {
		in.Count = 1
	}
	if in.Count < 1 {
		badRequest(w, "count must be at least 1")
		return
	}
	in.Label = strings.ToUpper(strings.TrimSpace(in.Label))
	if in.Count > 1 && in.Label != "" {
		badRequest(w, "count > 1 auto-sequences labels — leave label empty")
		return
	}

	// Serialize cart creation: two concurrent requests otherwise
	// autogenerate the same label from equal snapshots (the v0.7 rapid-
	// mint collision; the engine rail caught it, now it can't happen).
	s.mintMu.Lock()
	defer s.mintMu.Unlock()

	snap := s.inv.Snapshot()
	if in.Library == 0 {
		live := 0
		for _, l := range snap.Libraries {
			if l.Library.Live {
				in.Library, live = l.Library.ID, live+1
			}
		}
		if live != 1 {
			badRequest(w, "library is required (%d live libraries)", live)
			return
		}
	}
	lib, ok := snap.LibraryByID(in.Library)
	if !ok || !lib.Library.Live {
		badRequest(w, "library %d is not live", in.Library)
		return
	}

	// A batch can't exceed the library's free storage slots — each cart
	// parks in one. Bound it up front so the request is a clean refusal
	// rather than a partial batch that fails once slots run out.
	if avail := lib.FreeStorageSlots(); in.Count > avail {
		if avail == 0 {
			badRequest(w, "no free slots in library %d — every storage slot is occupied", in.Library)
			return
		}
		badRequest(w, "count must be 1-%d — the library has %d free slot(s)", avail, avail)
		return
	}

	// Cart size is not a user input — it follows the library's drive (its
	// LTO/media generation), using the standard native capacity.
	sizeMB := s.inv.MintCapacityMB(r.Context(), in.Library)
	sizeGB := sizeMB / 1000

	// Known labels (live inventory + catalog) + the library's label prefix
	// and drive suffix — the inputs to auto-sequenced label allocation.
	// Batch allocations append to known so the sequence advances within one
	// request without re-snapshotting.
	known, prefix, suffix, err := s.mintKnownLabels(r.Context(), snap, in.Library)
	if err != nil {
		serverError(w, err)
		return
	}

	type created struct {
		Label string `json:"label"`
		Slot  int    `json:"slot"`
	}
	out := []created{}
	for i := 0; i < in.Count; i++ {
		label := in.Label // explicit only for the single-cart form
		if label == "" {
			label = inventory.NextLabel(known, prefix, suffix)
			if label == "" {
				badRequest(w, "could not derive a free label — supply one explicitly")
				return
			}
		}
		if !inventory.LabelRe.MatchString(label) {
			badRequest(w, "label must be six A-Z/0-9 characters plus media suffix, e.g. OVT011L5")
			return
		}
		dup := false
		for _, k := range known {
			if k == label {
				dup = true
			}
		}
		if dup {
			badRequest(w, "label %s already exists", label)
			return
		}

		s.runner.SuppressIE(label)
		slot, err := s.inv.MintCart(r.Context(), in.Library, label, sizeMB)
		s.runner.ReleaseIE(label)
		if err != nil {
			// Partial batches are reported, not rolled back — created
			// carts are real and usable.
			if len(out) > 0 {
				s.audit(r, "cart.create", label, map[string]any{
					"library": in.Library, "size_gb": sizeGB, "created": len(out), "error": err.Error()})
				writeJSON(w, 500, map[string]any{
					"library": in.Library, "size_gb": sizeGB, "created": out,
					"error": fmt.Sprintf("cart %d/%d (%s): %v", len(out)+1, in.Count, label, err)})
				return
			}
			serverError(w, err)
			return
		}
		known = append(known, label)
		out = append(out, created{Label: label, Slot: slot})
		s.audit(r, "cart.create", label, map[string]any{"library": in.Library, "slot": slot, "size_gb": sizeGB})
	}
	writeJSON(w, 201, map[string]any{"library": in.Library, "size_gb": sizeGB, "created": out})
}

// mintKnownLabels gathers the inputs to auto-sequenced cart labelling: every
// label the system knows (live inventory + S3 catalog, so a label with
// generations in a bucket is never reused), the library's label prefix, and
// its drive's media suffix. Shared by the mint loop and the next-label peek.
func (s *Server) mintKnownLabels(ctx context.Context, snap inventory.Snapshot, libID int) (known []string, prefix, suffix string, err error) {
	for _, c := range snap.AllCarts() {
		known = append(known, c.Label)
	}
	if rows, e := s.db.AllCatalogLabels(ctx); e == nil {
		known = append(known, rows...)
	}
	prefix = "OVT"
	if row, e := s.db.GetLibrary(ctx, libID); e == nil && row.LabelPrefix != "" {
		prefix = row.LabelPrefix
	} else if e != nil && e != store.ErrNotFound {
		return nil, "", "", e
	}
	return known, prefix, s.inv.MintSuffix(ctx, libID), nil
}

// nextLabel — GET /api/libraries/{lib}/next-label — the label a mint would
// auto-generate next for a library, so the UI can show and live-refresh a
// batch's starting label. A read-only peek; the mint mutex still allocates
// authoritatively on create.
func (s *Server) nextLabel(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("lib"))
	if err != nil {
		badRequest(w, "bad library id")
		return
	}
	known, prefix, suffix, err := s.mintKnownLabels(r.Context(), s.inv.Snapshot(), id)
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{
		"label":  inventory.NextLabel(known, prefix, suffix),
		"prefix": prefix,
		"suffix": suffix,
	})
}

// deleteCart — DELETE /api/cartridges/{label} (v0.7):
// typed-label confirmation, refused while loaded or while any
// job references the cart. S3 generations are NOT touched — the
// bucket stays the catalog of record; re-minting the label and
// importing remains possible.
func (s *Server) deleteCart(w http.ResponseWriter, r *http.Request) {
	label := strings.ToUpper(strings.TrimSpace(r.PathValue("label")))
	var in struct {
		Confirm string `json:"confirm"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || strings.ToUpper(strings.TrimSpace(in.Confirm)) != label {
		badRequest(w, `confirmation required: {"confirm":"<label>"} must match the cartridge label exactly`)
		return
	}
	cart, lib, ok := s.inv.Snapshot().FindCart(label)
	if !ok {
		writeJSON(w, 404, map[string]string{"error": "unknown cartridge"})
		return
	}
	if strings.HasPrefix(cart.Location, "drive:") {
		badRequest(w, "%s is loaded in a drive — unload it first", label)
		return
	}
	if jobs, err := s.db.UnfinishedJobs(r.Context()); err != nil {
		serverError(w, err)
		return
	} else {
		for _, j := range jobs {
			if j.CartLabel == label {
				badRequest(w, "job #%d (%s) is %s on this cartridge — wait or cancel first", j.ID, j.Kind, j.State)
				return
			}
		}
	}

	s.runner.SuppressIE(label)
	defer s.runner.ReleaseIE(label)
	if err := s.inv.DeleteCart(r.Context(), lib.Library.ID, label); err != nil {
		serverError(w, err)
		return
	}
	if err := s.db.DeleteCartridge(r.Context(), label); err != nil {
		s.log.Warn("cartridge row delete", "label", label, "err", err)
	}
	s.audit(r, "cart.delete", label, map[string]any{
		"library": lib.Library.ID, "size_bytes": cart.SizeBytes})
	writeJSON(w, 200, map[string]any{"ok": true, "label": label})
}
