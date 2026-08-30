package inventory

// Zero-restart cart deletion (v0.7) — the reverse
// of mint.go's MAP transit: park the cart in an I/E element, open the
// MAP, `vtlcmd empty map` (clears occupied state on every MAP slot —
// which is why deletion refuses to run while any OTHER cart sits in an
// I/E element), close, then remove the media directory and rewrite
// library_contents so the next mhVTL restart agrees.
//
// The caller must suppress the IE watcher for the label first: the
// slot→IE move is indistinguishable from a vault move on the wire.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/openvtl/openvtld/internal/sysexec"
)

// DeleteCart removes a cart from a live library and deletes its media
// files. The cart must not be loaded in a drive and every other I/E
// element must be empty (empty map is MAP-wide).
func (e *Engine) DeleteCart(ctx context.Context, libID int, label string) error {
	libs, err := ParseMhvtlConf(e.cfg.MhvtlConf)
	if err != nil {
		return fmt.Errorf("mhvtl config: %w", err)
	}
	var lib *MhvtlLibrary
	for i := range libs {
		if libs[i].ID == libID {
			lib = &libs[i]
		}
	}
	if lib == nil {
		return fmt.Errorf("library %d not in device.conf", libID)
	}
	changer, ok := e.ChangerFor(libID)
	if !ok {
		return fmt.Errorf("library %d has no live changer (pending mhvtl restart?)", libID)
	}
	home := lib.HomeDir
	if home == "" {
		home = e.cfg.MediaDir
	}

	st, err := PollChanger(ctx, changer)
	if err != nil {
		return fmt.Errorf("changer status: %w", err)
	}
	for _, d := range st.Drives {
		if d.Label == label {
			return fmt.Errorf("%s is loaded in a drive — unload it first", label)
		}
	}
	srcSlot, freeIE, inIE := 0, 0, false
	for _, s := range st.Slots {
		switch {
		case s.Label == label && s.IE:
			inIE = true
		case s.Label == label:
			srcSlot = s.Num
		case s.IE && s.Label != "":
			return fmt.Errorf("I/E element %d holds %s — empty map is MAP-wide, clear the I/E station first", s.Num, s.Label)
		case s.IE && freeIE == 0:
			freeIE = s.Num
		}
	}
	if srcSlot == 0 && !inIE {
		return fmt.Errorf("%s not found in library %d", label, libID)
	}

	// 1. park it in the MAP (unless it already sits there)
	if !inIE {
		if freeIE == 0 {
			return fmt.Errorf("no free I/E element to stage the deletion")
		}
		if err := MoveCart(ctx, changer, srcSlot, freeIE); err != nil {
			return fmt.Errorf("move %s slot %d -> IE %d: %w", label, srcSlot, freeIE, err)
		}
	}

	// 2. clear it from the library inventory. empty map requires the
	// MAP open; close afterwards so the changer state is normal.
	vtl := func(args ...string) error {
		argv := append([]string{strconv.Itoa(lib.ID)}, args...)
		_, err := sysexec.Run(ctx, 30*time.Second, "vtlcmd", argv...)
		return err
	}
	_ = vtl("open", "map") // idempotent; some builds report already-open as error
	if err := vtl("empty", "map"); err != nil {
		// Recovery: try to put the cart back where it came from.
		if !inIE && srcSlot != 0 {
			_ = MoveCart(ctx, changer, freeIE, srcSlot)
		}
		return fmt.Errorf("vtlcmd empty map: %w", err)
	}
	if err := vtl("close", "map"); err != nil {
		e.log.Warn("close map after empty", "library", lib.ID, "err", err)
	}

	// 3. confirm the changer no longer lists the barcode
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		st, err := PollChanger(ctx, changer)
		if err == nil {
			gone := true
			for _, s := range st.Slots {
				if s.Label == label {
					gone = false
				}
			}
			if gone {
				break
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}

	// 4. remove the media directory — but only if it looks like mhVTL
	// media (mam present) or an eviction stub; never RemoveAll blind.
	mediaDir := filepath.Join(home, label)
	if _, err := os.Stat(mediaDir); err == nil {
		if !fileExists(filepath.Join(mediaDir, "mam")) && !fileExists(filepath.Join(mediaDir, ".openvtl-evicted.json")) {
			return fmt.Errorf("%s does not look like mhVTL media (no mam file) — inventory cleared, files left in place", mediaDir)
		}
		if err := os.RemoveAll(mediaDir); err != nil {
			return fmt.Errorf("remove media %s (inventory already cleared): %w", mediaDir, err)
		}
	}

	// 5. persist for the next daemon restart
	if err := e.RewriteLibraryContents(ctx, lib.ID); err != nil {
		e.log.Warn("library_contents rewrite failed after delete — next mhvtl restart may resurrect the slot entry", "err", err)
	}

	// 6. forget it in the live snapshot. The cart list is otherwise
	// append-only (changer poll only flips locations to "missing", and
	// applyMedia re-upserts every listed cart into the DB — which would
	// resurrect the row the API is about to delete).
	e.forgetCart(libID, label)

	e.log.Info("cart deleted", "library", lib.ID, "label", label)
	e.bus.Publish("cart_deleted", label, map[string]any{"library": lib.ID})
	return nil
}

func (e *Engine) forgetCart(libID int, label string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for li := range e.snap.Libraries {
		if e.snap.Libraries[li].Library.ID != libID {
			continue
		}
		carts := e.snap.Libraries[li].Carts
		for i := range carts {
			if carts[i].Label == label {
				e.snap.Libraries[li].Carts = append(carts[:i], carts[i+1:]...)
				break
			}
		}
	}
	e.snap.UpdatedAt = time.Now().UTC()
}
