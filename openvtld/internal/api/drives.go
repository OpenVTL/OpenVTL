package api

// Manual drive load/unload — changer-level moves (mtx through the
// library, the same path a host MOVE MEDIUM takes). The daemon never
// touches drive st/sg nodes; the one hard rule is refusing to yank
// media out from under an active drive. Library-scoped since v0.6.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/openvtl/openvtld/internal/inventory"
)

// driveIdleFor: a drive must have been quiet this long before a manual
// unload is allowed. The host sees the usual not-ready/unit-attention
// on its next access, exactly as with an operator move on real iron.
const driveIdleFor = 15 * time.Second

// libDrive resolves the {lib}/{index} path pair to the live library
// snapshot + drive.
func (s *Server) libDrive(r *http.Request) (inventory.LibrarySnapshot, *inventory.Drive, error) {
	libID, err := strconv.Atoi(r.PathValue("lib"))
	if err != nil {
		return inventory.LibrarySnapshot{}, nil, fmt.Errorf("bad library id")
	}
	idx, err := strconv.Atoi(r.PathValue("index"))
	if err != nil {
		return inventory.LibrarySnapshot{}, nil, fmt.Errorf("bad drive index")
	}
	lib, ok := s.inv.Snapshot().LibraryByID(libID)
	if !ok {
		return inventory.LibrarySnapshot{}, nil, fmt.Errorf("unknown library %d", libID)
	}
	if !lib.Library.Live {
		return inventory.LibrarySnapshot{}, nil, fmt.Errorf("library %d is not being served (pending mhvtl restart?)", libID)
	}
	for i := range lib.Drives {
		if lib.Drives[i].Index == idx {
			return lib, &lib.Drives[i], nil
		}
	}
	return inventory.LibrarySnapshot{}, nil, fmt.Errorf("unknown drive %d in library %d", idx, libID)
}

func (s *Server) loadDrive(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Label string `json:"label"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Label == "" {
		badRequest(w, "label is required")
		return
	}
	lib, d, err := s.libDrive(r)
	if err != nil {
		badRequest(w, "%v", err)
		return
	}
	if d.Loaded != "" {
		badRequest(w, "drive %d already holds %s", d.Index, d.Loaded)
		return
	}
	var loc string
	for _, c := range lib.Carts {
		if c.Label == in.Label {
			loc = c.Location
		}
	}
	if loc == "" {
		badRequest(w, "cartridge %s is not in library %d", in.Label, lib.Library.ID)
		return
	}
	elem, ok := elementForCart(loc)
	if !ok {
		badRequest(w, "cart %s is not in a loadable element (location %s)", in.Label, loc)
		return
	}
	s.audit(r, "drive.load", in.Label, map[string]any{"library": lib.Library.ID, "drive": d.Index, "from": loc})
	if err := inventory.LoadDrive(r.Context(), lib.Library.ChangerSG, elem, d.Index); err != nil {
		serverError(w, err)
		return
	}
	detail := fmt.Sprintf("%s: %s -> library %d drive %d (manual)", in.Label, loc, lib.Library.ID, d.Index)
	s.db.LogEvent(r.Context(), time.Now(), "manual_load", in.Label, detail)
	writeJSON(w, 200, map[string]any{"ok": true, "detail": detail})
}

func (s *Server) unloadDrive(w http.ResponseWriter, r *http.Request) {
	lib, d, err := s.libDrive(r)
	if err != nil {
		badRequest(w, "%v", err)
		return
	}
	if d.Loaded == "" {
		badRequest(w, "drive %d is empty", d.Index)
		return
	}
	if d.Activity != "idle" {
		badRequest(w, "drive %d is %s — refusing to unload live media", d.Index, d.Activity)
		return
	}
	if !d.LastActive.IsZero() && time.Since(d.LastActive) < driveIdleFor {
		badRequest(w, "drive %d was active %s ago — wait for %s of quiet before unloading",
			d.Index, time.Since(d.LastActive).Round(time.Second), driveIdleFor)
		return
	}
	// Prefer the cart's home slot (mtx reports the source element);
	// fall back to the first empty storage slot.
	dest := 0
	for _, sl := range lib.Slots {
		if sl.Kind == "storage" && sl.Label == "" {
			if sl.Num == d.SourceSlot {
				dest = sl.Num
				break
			}
			if dest == 0 {
				dest = sl.Num
			}
		}
	}
	if dest == 0 {
		badRequest(w, "no empty storage slot to unload into")
		return
	}
	label := d.Loaded
	s.audit(r, "drive.unload", label, map[string]any{"library": lib.Library.ID, "drive": d.Index, "to_slot": dest})
	if err := inventory.UnloadDrive(r.Context(), lib.Library.ChangerSG, dest, d.Index); err != nil {
		serverError(w, err)
		return
	}
	detail := fmt.Sprintf("%s: library %d drive %d -> storage slot %d (manual)", label, lib.Library.ID, d.Index, dest)
	s.db.LogEvent(r.Context(), time.Now(), "manual_unload", label, detail)
	writeJSON(w, 200, map[string]any{"ok": true, "detail": detail, "slot": dest})
}

// elementForCart parses a cart's "storage:N"/"ie:N" location into the
// mtx element number.
func elementForCart(loc string) (int, bool) {
	parts := strings.SplitN(loc, ":", 2)
	if len(parts) != 2 || (parts[0] != "storage" && parts[0] != "ie") {
		return 0, false
	}
	n, err := strconv.Atoi(parts[1])
	return n, err == nil
}
