package inventory

// Zero-restart cart creation (v0.5 media ops, per-library since v0.6).
// mhVTL 1.8.0 supports runtime insertion: mktape creates the media,
// then the library daemon takes it through the MAP — vtlcmd open map /
// load map / close map — and MOVE MEDIUM parks it in a storage slot.
// No daemon restart, no operator vary off/on. library_contents.<id> is
// rewritten afterwards so the *next* mhvtl restart agrees with runtime
// reality (the file is only read at daemon start).
//
// The caller must suppress the IE watcher for the label first: the
// cart transits an IE element, which is exactly what a vault move
// looks like (export.Runner.SuppressIE).

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/openvtl/openvtld/internal/sysexec"
)

// LabelRe: six alphanumerics + media suffix — LTO density (L1-L9) or
// 3592 media type (JA/JB). mhVTL is more permissive; the host side isn't.
var LabelRe = regexp.MustCompile(`^[A-Z0-9]{6}(L[1-9]|J[AB])$`)

var reLabelParts = regexp.MustCompile(`^([A-Z0-9]{3})(\d{3})(L[1-9]|J[AB])$`)

// NextLabel proposes the next unused label for a prefix/suffix pair
// (e.g. "OVT", "L5" -> OVT013L5) given every label the system knows
// (all libraries + catalog). Labels are globally unique, so the scan
// spans everything regardless of prefix.
func NextLabel(known []string, prefix, suffix string) string {
	max := 0
	taken := map[string]bool{}
	for _, l := range known {
		taken[l] = true
		if m := reLabelParts.FindStringSubmatch(l); m != nil && m[1] == prefix && m[3] == suffix {
			if n, _ := strconv.Atoi(m[2]); n > max {
				max = n
			}
		}
	}
	for n := max + 1; n < 1000; n++ {
		c := fmt.Sprintf("%s%03d%s", prefix, n, suffix)
		if !taken[c] {
			return c
		}
	}
	return ""
}

// defaultCapacityMB — LTO-5 (1.5 TB), the media proven against the IBM i;
// used when a library's drive model isn't in the catalog.
const defaultCapacityMB = 1_500_000

// mintDensity picks the mktape density + default suffix for a library
// from its first drive's catalog entry. Unknown products fall back to
// LTO5 — the density proven against the IBM i.
func mintDensity(lib MhvtlLibrary) (density, suffix string) {
	if len(lib.Drives) > 0 {
		if dm, ok := DriveModelByProduct(lib.Drives[0].Product); ok {
			return dm.Density, dm.Suffix
		}
	}
	return "LTO5", "L5"
}

// mintCapacityMB picks the native cart capacity for a library from its
// first drive's LTO/media generation (the operator does not set cart size;
// it follows the drive). Unknown products fall back to LTO-5.
func mintCapacityMB(lib MhvtlLibrary) int {
	if len(lib.Drives) > 0 {
		if dm, ok := DriveModelByProduct(lib.Drives[0].Product); ok && dm.CapacityMB > 0 {
			return dm.CapacityMB
		}
	}
	return defaultCapacityMB
}

// MintCapacityMB exposes a library's per-cart capacity (decimal MB) so the
// mint API can size carts from the drive model instead of a user input.
func (e *Engine) MintCapacityMB(ctx context.Context, libID int) int {
	if libs, err := ParseMhvtlConf(e.cfg.MhvtlConf); err == nil {
		for _, l := range libs {
			if l.ID == libID {
				return mintCapacityMB(l)
			}
		}
	}
	return defaultCapacityMB
}

// MintSuffix exposes a library's barcode suffix for label generation.
func (e *Engine) MintSuffix(ctx context.Context, libID int) string {
	libs, err := ParseMhvtlConf(e.cfg.MhvtlConf)
	if err == nil {
		for _, l := range libs {
			if l.ID == libID {
				_, suffix := mintDensity(l)
				return suffix
			}
		}
	}
	return "L5"
}

// MintCart creates a labelled cart in a library and parks it in a free
// storage slot, returning the slot element number. The MAP transit + park
// is shared with AdoptCart (adopt.go); mint adds mktape up front.
func (e *Engine) MintCart(ctx context.Context, libID int, label string, sizeMB int) (int, error) {
	lib, changer, home, err := e.libAndChanger(ctx, libID)
	if err != nil {
		return 0, err
	}
	freeSlot, err := findFreeSlot(ctx, changer, label)
	if err != nil {
		return 0, err
	}
	mediaDir := filepath.Join(home, label)
	if _, err := os.Stat(mediaDir); err == nil {
		return 0, fmt.Errorf("media directory %s already exists", mediaDir)
	}

	// media files (density from the library's drive model). -H is required:
	// mktape does NOT read the per-library Home directory from device.conf —
	// without it the cart lands in the default /opt/mhvtl while vtltape
	// serves the library's own home.
	density, _ := mintDensity(*lib)
	if _, err := sysexec.Run(ctx, 30*time.Second, "mktape",
		"-C", e.cfg.MhvtlConf, "-H", home, "-l", strconv.Itoa(lib.ID),
		"-m", label, "-s", strconv.Itoa(sizeMB), "-t", "data", "-d", density); err != nil {
		return 0, fmt.Errorf("mktape: %w", err)
	}
	// Fresh media, never in the library — safe to drop on any park failure.
	if err := e.parkMappedMedia(ctx, lib, changer, home, label, freeSlot); err != nil {
		os.RemoveAll(mediaDir)
		return 0, err
	}

	e.log.Info("cart minted", "library", lib.ID, "label", label, "slot", freeSlot, "size_mb", sizeMB)
	e.bus.Publish("cart_minted", label, map[string]any{"library": lib.ID, "slot": freeSlot, "size_mb": sizeMB})
	return freeSlot, nil
}

// RewriteLibraryContents regenerates library_contents.<id> from live
// changer state — after a mint, and immediately before any mhvtl
// restart so runtime cart positions survive the daemon re-reading the
// file (carts otherwise teleport to stale home slots). Loaded carts
// are written at their home slots. The pre-openvtl file is kept once
// as .bak-openvtl.
func (e *Engine) RewriteLibraryContents(ctx context.Context, libID int) error {
	changer, ok := e.ChangerFor(libID)
	if !ok {
		return fmt.Errorf("library %d has no live changer", libID)
	}
	st, err := PollChanger(ctx, changer)
	if err != nil {
		return err
	}
	path := filepath.Join(e.cfg.MhvtlConf, fmt.Sprintf("library_contents.%d", libID))
	if backup := path + ".bak-openvtl"; !fileExists(backup) {
		if b, err := os.ReadFile(path); err == nil {
			_ = os.WriteFile(backup, b, 0o644)
		}
	}

	// home-slot map: slot -> label, including loaded carts' home slots
	slotLabel := map[int]string{}
	var storage []int
	nIE := 0
	for _, s := range st.Slots {
		if s.IE {
			nIE++
			continue
		}
		storage = append(storage, s.Num)
		if s.Label != "" {
			slotLabel[s.Num] = s.Label
		}
	}
	for _, d := range st.Drives {
		if d.Label != "" && d.Source > 0 {
			slotLabel[d.Source] = d.Label
		}
	}
	sort.Ints(storage)

	var b strings.Builder
	b.WriteString("VERSION: 2\n\n")
	for i := range st.Drives {
		fmt.Fprintf(&b, "Drive %d:\n", i+1)
	}
	b.WriteString("\nPicker 1:\n\n")
	for i := 1; i <= nIE; i++ {
		fmt.Fprintf(&b, "MAP %d:\n", i)
	}
	b.WriteString("\n")
	for _, n := range storage {
		if l := slotLabel[n]; l != "" {
			fmt.Fprintf(&b, "Slot %d: %s\n", n, l)
		} else {
			fmt.Fprintf(&b, "Slot %d:\n", n)
		}
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
