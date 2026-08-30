package inventory

// Library creation (v0.6): append a catalog-validated library block to
// device.conf and write its empty library_contents. NOTHING restarts
// here — mhVTL reads these files only at daemon start, so the new
// library stays declared-but-unserved (state pending_restart, Live
// false in the snapshot) until the operator-window Apply runs the
// restart + FC rebuild sequence.

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type CreateLibrarySpec struct {
	Product      string // library product id from the catalog (3573-TL, 03584L32, …)
	DriveProduct string // drive product id (ULT3580-TD5, …)
	NumDrives    int
	HomeDir      string // the paired pool's mountpoint
	NumSlots     int    // 0 = 100
	NumMAP       int    // 0 = 4
	Serial       string // adopt a specific serial (recovery); "" = random. Must be locally unique.
}

// systemdDir is a var for tests only.
var systemdDir = "/etc/systemd/system"

// randomSerial mints a random device serial (<prefix> + 6 digits), unique
// among the existing ones. Random, not sequential: under the S3 layout a
// library serial is its <library> path segment, so a sequential run would
// let a deleted-and-recreated library reuse another's namespace; drives
// get the same treatment for consistency (a recreated device never reuses
// an identity). Existing/legacy serials (OVTLLIB###, OVTLDRV###) are never
// reissued.
func randomSerial(prefix string, existing []string) string {
	taken := map[string]bool{}
	for _, s := range existing {
		taken[s] = true
	}
	for i := 0; i < 10000; i++ {
		s := fmt.Sprintf("%s%06d", prefix, randInt(1000000))
		if !taken[s] {
			return s
		}
	}
	return fmt.Sprintf("%s%06d", prefix, randInt(1000000)) // effectively unreachable
}

// randInt returns a crypto-random int in [0, n).
func randInt(n int) int {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return int(binary.BigEndian.Uint32(b[:]) % uint32(n))
}

// CreateLibrary validates against current device.conf state, appends
// the new blocks, and writes an empty library_contents.<id>. Returns
// the allocated (libraryID, librarySerial, driveSerials).
func (e *Engine) CreateLibrary(ctx context.Context, spec CreateLibrarySpec) (int, string, []string, error) {
	if spec.NumSlots == 0 {
		spec.NumSlots = 100
	}
	if spec.NumMAP == 0 {
		spec.NumMAP = 4
	}
	if _, err := os.Stat(spec.HomeDir); err != nil {
		return 0, "", nil, fmt.Errorf("home directory %s: %w", spec.HomeDir, err)
	}
	model, variant, ok := LibraryVariantByProduct(spec.Product)
	if !ok || !model.Creatable || !variant.Creatable {
		return 0, "", nil, fmt.Errorf("library product %q is not creatable (not in the catalog, not IBM i-compatible, or not yet validated)", spec.Product)
	}
	if spec.NumDrives < 1 || spec.NumDrives > model.MaxDrives {
		return 0, "", nil, fmt.Errorf("num_drives must be 1-%d for a %s", model.MaxDrives, model.Display)
	}
	dm, ok := DriveModelByProduct(spec.DriveProduct)
	if !ok || dm.Family != variant.Family {
		return 0, "", nil, fmt.Errorf("drive %q does not fit a %s-family library", spec.DriveProduct, variant.Family)
	}

	libs, err := ParseMhvtlConf(e.cfg.MhvtlConf)
	if err != nil {
		return 0, "", nil, fmt.Errorf("mhvtl config: %w", err)
	}
	// Allocations: library ids land on multiples of 10 (10, 20, …);
	// vtltape queue ids are <libID>+1.. (11, 12 / 21, 22 — the installed
	// convention). Queue ids must be unique across ALL daemons, and a
	// 3584 can carry up to 12 drives — more than fit inside one decade —
	// so the next library id clears every used queue id (drives
	// included), not just the highest library id. SCSI targets continue
	// past the HIGHEST target in use on channel 0 — never a count of
	// devices, which would re-issue a deleted library's targets while a
	// survivor still holds higher ones (kernel refuses the duplicate
	// address and the new devices never register). Serials continue the
	// OVTLLIB/OVTLDRV numeric runs.
	maxQ, maxTarget := 0, -1
	var libSerials, drvSerials []string
	for _, l := range libs {
		if l.ID > maxQ {
			maxQ = l.ID
		}
		if l.Target > maxTarget {
			maxTarget = l.Target
		}
		libSerials = append(libSerials, l.Serial)
		for _, d := range l.Drives {
			drvSerials = append(drvSerials, d.Serial)
			if d.QueueID > maxQ {
				maxQ = d.QueueID
			}
			if d.Target > maxTarget {
				maxTarget = d.Target
			}
		}
	}
	libID := ((maxQ / 10) + 1) * 10
	nextTarget := maxTarget + 1
	// Recovery adopts the source library's serial (faithful DR; the S3
	// <system> segment keeps instances separate). Must be locally unique.
	serial := spec.Serial
	if serial == "" {
		serial = randomSerial("OVTL", libSerials)
	} else {
		for _, s := range libSerials {
			if s == serial {
				return 0, "", nil, fmt.Errorf("a library with serial %s already exists here", serial)
			}
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "\nLibrary: %d CHANNEL: 00 TARGET: %02d LUN: 00\n", libID, nextTarget)
	fmt.Fprintf(&b, " Vendor identification: %s\n", model.Vendor)
	fmt.Fprintf(&b, " Product identification: %s\n", spec.Product)
	fmt.Fprintf(&b, " Unit serial number: %s\n", serial)
	fmt.Fprintf(&b, " NAA: 30:00:00:00:00:00:00:%02d\n", libID)
	fmt.Fprintf(&b, " Home directory: %s\n", spec.HomeDir)
	fmt.Fprintf(&b, " PERSIST: True\n")
	fmt.Fprintf(&b, " Backoff: 400\n")
	fmt.Fprintf(&b, " Product revision level: D.00\n")

	var newDrvSerials []string
	for i := 0; i < spec.NumDrives; i++ {
		queue := libID + 1 + i
		dserial := randomSerial("OVTD", append(drvSerials, newDrvSerials...))
		newDrvSerials = append(newDrvSerials, dserial)
		fmt.Fprintf(&b, "\nDrive: %d CHANNEL: 00 TARGET: %02d LUN: 00\n", queue, nextTarget+1+i)
		fmt.Fprintf(&b, " Library ID: %d Slot: %02d\n", libID, i+1)
		fmt.Fprintf(&b, " Vendor identification: %s\n", dm.Vendor)
		fmt.Fprintf(&b, " Product identification: %s\n", spec.DriveProduct)
		fmt.Fprintf(&b, " Product revision level: H991\n")
		fmt.Fprintf(&b, " Unit serial number: %s\n", dserial)
		fmt.Fprintf(&b, " NAA: 50:05:07:60:00:00:00:%02d\n", queue)
		// Patch-9 raw writes: per-cart compression stays OFF or dedupe dies.
		fmt.Fprintf(&b, " Compression: factor 1 enabled 0\n")
		fmt.Fprintf(&b, " Compression type: lzo\n")
		fmt.Fprintf(&b, " Backoff: 400\n")
	}

	// Append to device.conf via tmp+rename; keep a one-time backup.
	confPath := filepath.Join(e.cfg.MhvtlConf, "device.conf")
	orig, err := os.ReadFile(confPath)
	if err != nil {
		return 0, "", nil, err
	}
	if backup := confPath + ".bak-openvtl"; !fileExists(backup) {
		_ = os.WriteFile(backup, orig, 0o644)
	}
	tmp := confPath + ".tmp"
	if err := os.WriteFile(tmp, append(orig, []byte(b.String())...), 0o644); err != nil {
		return 0, "", nil, err
	}
	if err := os.Rename(tmp, confPath); err != nil {
		return 0, "", nil, err
	}

	// Empty library_contents.<id>.
	var lc strings.Builder
	lc.WriteString("VERSION: 2\n\n")
	for i := 1; i <= spec.NumDrives; i++ {
		fmt.Fprintf(&lc, "Drive %d:\n", i)
	}
	lc.WriteString("\nPicker 1:\n\n")
	for i := 1; i <= spec.NumMAP; i++ {
		fmt.Fprintf(&lc, "MAP %d:\n", i)
	}
	lc.WriteString("\n")
	for i := 1; i <= spec.NumSlots; i++ {
		fmt.Fprintf(&lc, "Slot %d:\n", i)
	}
	lcPath := filepath.Join(e.cfg.MhvtlConf, fmt.Sprintf("library_contents.%d", libID))
	if err := os.WriteFile(lcPath, []byte(lc.String()), 0o644); err != nil {
		return 0, "", nil, err
	}

	// mhVTL daemons must never start against an unmounted pool: per-
	// instance systemd drop-ins (inert until the Apply daemon-reload).
	dropin := fmt.Sprintf("[Unit]\nRequiresMountsFor=%s\n", spec.HomeDir)
	unitDirs := []string{fmt.Sprintf("%s/vtllibrary@%d.service.d", systemdDir, libID)}
	for i := 0; i < spec.NumDrives; i++ {
		unitDirs = append(unitDirs, fmt.Sprintf("%s/vtltape@%d.service.d", systemdDir, libID+1+i))
	}
	for _, unitDir := range unitDirs {
		if err := os.MkdirAll(unitDir, 0o755); err != nil {
			return 0, "", nil, err
		}
		if err := os.WriteFile(filepath.Join(unitDir, "openvtl-pool.conf"), []byte(dropin), 0o644); err != nil {
			return 0, "", nil, err
		}
	}

	// Surface the new library in the snapshot immediately (live=false).
	if err := e.Reload(ctx); err != nil {
		e.log.Warn("engine reload after library create", "err", err)
	}
	e.log.Info("library declared (pending mhvtl restart)",
		"library", libID, "serial", serial, "product", spec.Product,
		"drives", spec.NumDrives, "home", spec.HomeDir)
	e.bus.Publish("library_created", serial, map[string]any{"library": libID, "pending_restart": true})
	return libID, serial, newDrvSerials, nil
}
