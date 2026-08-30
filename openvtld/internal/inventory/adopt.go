package inventory

// AdoptCart slots already-existing cart media into a library — the media
// half of cross-instance / DR import (Phase A). An S3 import extracts a
// foreign cart's files into the target library's home dir; AdoptCart then
// runs the SAME zero-restart MAP transit a mint uses (open/load/close map →
// MOVE MEDIUM into a free slot → rewrite library_contents), minus mktape.
// The result is a cart the host sees and can mount, with its label
// preserved — no daemon restart, no operator vary off/on.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/openvtl/openvtld/internal/sysexec"
)

// libAndChanger resolves a library from device.conf plus its live changer
// sg node and media home — the common preamble to mint and adopt.
func (e *Engine) libAndChanger(ctx context.Context, libID int) (*MhvtlLibrary, string, string, error) {
	libs, err := ParseMhvtlConf(e.cfg.MhvtlConf)
	if err != nil {
		return nil, "", "", fmt.Errorf("mhvtl config: %w", err)
	}
	var lib *MhvtlLibrary
	for i := range libs {
		if libs[i].ID == libID {
			lib = &libs[i]
		}
	}
	if lib == nil {
		return nil, "", "", fmt.Errorf("library %d not in device.conf", libID)
	}
	changer, ok := e.ChangerFor(libID)
	if !ok {
		return nil, "", "", fmt.Errorf("library %d has no live changer (pending mhvtl restart?)", libID)
	}
	home := lib.HomeDir
	if home == "" {
		home = e.cfg.MediaDir
	}
	return lib, changer, home, nil
}

// LibraryHome returns a live library's media root — where imported cart
// media must be placed before AdoptCart slots it.
func (e *Engine) LibraryHome(ctx context.Context, libID int) (string, error) {
	_, _, home, err := e.libAndChanger(ctx, libID)
	return home, err
}

// findFreeSlot returns the first empty, non-reserved storage slot, and
// errors if the label is already loaded or already in the library. A
// loaded cart's home slot reads Empty but is spoken for (drive Source).
func findFreeSlot(ctx context.Context, changer, label string) (int, error) {
	st, err := PollChanger(ctx, changer)
	if err != nil {
		return 0, fmt.Errorf("changer status: %w", err)
	}
	reserved := map[int]bool{}
	for _, d := range st.Drives {
		if d.Label == label {
			return 0, fmt.Errorf("label %s already loaded in a drive", label)
		}
		if d.Label != "" && d.Source > 0 {
			reserved[d.Source] = true
		}
	}
	freeSlot := 0
	for _, s := range st.Slots {
		if s.Label == label {
			return 0, fmt.Errorf("label %s already in the library", label)
		}
		if !s.IE && s.Label == "" && !reserved[s.Num] && freeSlot == 0 {
			freeSlot = s.Num
		}
	}
	if freeSlot == 0 {
		return 0, fmt.Errorf("no free storage slot (vtlcmd 'add slot' can grow the library)")
	}
	return freeSlot, nil
}

// parkMappedMedia takes media already sitting in home/<label> through the
// MAP into freeSlot and persists library_contents. Shared by MintCart
// (after mktape) and AdoptCart (after an import extract). See MintCart for
// the annotated original of each step (placeholder, argv, IE wait).
func (e *Engine) parkMappedMedia(ctx context.Context, lib *MhvtlLibrary, changer, home, label string, freeSlot int) error {
	// vtllibrary 1.8.0's "load map" existence check probes the legacy
	// un-segmented <cart>/data; a zero-byte placeholder satisfies it and is
	// removed once the cart is parked so vtltape is never confused by it.
	placeholder := filepath.Join(home, label, "data")
	if f, err := os.OpenFile(placeholder, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640); err == nil {
		f.Close()
	}

	vtl := func(args ...string) error {
		argv := append([]string{strconv.Itoa(lib.ID)}, args...)
		_, err := sysexec.Run(ctx, 30*time.Second, "vtlcmd", argv...)
		return err
	}
	_ = vtl("open", "map") // idempotent; some builds report already-open as error
	if err := vtl("load", "map", label); err != nil {
		return fmt.Errorf("vtlcmd load map: %w", err)
	}
	if err := vtl("close", "map"); err != nil {
		return fmt.Errorf("vtlcmd close map: %w", err)
	}

	ieNum := 0
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if st, err := PollChanger(ctx, changer); err == nil {
			for _, s := range st.Slots {
				if s.IE && s.Label == label {
					ieNum = s.Num
				}
			}
		}
		if ieNum != 0 {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	if ieNum == 0 {
		return fmt.Errorf("cart %s never appeared in an IE element after load map — check vtllibrary state", label)
	}

	if err := MoveCart(ctx, changer, ieNum, freeSlot); err != nil {
		return fmt.Errorf("park %s IE %d -> slot %d: %w", label, ieNum, freeSlot, err)
	}
	if err := os.Remove(placeholder); err != nil {
		e.log.Warn("legacy data placeholder not removed", "path", placeholder, "err", err)
	}
	if err := e.RewriteLibraryContents(ctx, lib.ID); err != nil {
		e.log.Warn("library_contents rewrite failed — runtime state is fine, next mhvtl restart won't see the cart until fixed", "err", err)
	}
	return nil
}

// AdoptCart registers cart media already extracted into a library's home
// dir (by an S3 import) into a free storage slot, returning the slot
// element number. Unlike MintCart it does not create media — the caller
// (runImport) has already placed and verified home/<label>.
func (e *Engine) AdoptCart(ctx context.Context, libID int, label string) (int, error) {
	lib, changer, home, err := e.libAndChanger(ctx, libID)
	if err != nil {
		return 0, err
	}
	if _, err := os.Stat(filepath.Join(home, label)); err != nil {
		return 0, fmt.Errorf("imported media for %s not in %s: %w", label, home, err)
	}
	freeSlot, err := findFreeSlot(ctx, changer, label)
	if err != nil {
		return 0, err
	}
	if err := e.parkMappedMedia(ctx, lib, changer, home, label, freeSlot); err != nil {
		return 0, err
	}
	e.log.Info("cart adopted from import", "library", lib.ID, "label", label, "slot", freeSlot)
	e.bus.Publish("cart_imported", label, map[string]any{"library": lib.ID, "slot": freeSlot})
	return freeSlot, nil
}
