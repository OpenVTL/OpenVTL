package inventory

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/openvtl/openvtld/internal/config"
	"github.com/openvtl/openvtld/internal/events"
)

// The single-library device.conf shape as installed by mhVTL 1.8.0.
const oneLibConf = `VERSION: 5

Library: 10 CHANNEL: 00 TARGET: 00 LUN: 00
 Vendor identification: IBM
 Product identification: 3573-TL
 Unit serial number: OVTLLIB001
 NAA: 30:00:00:00:00:00:00:10
 Home directory: /opt/mhvtl
 PERSIST: True
 Backoff: 400
 Product revision level: D.00

Drive: 11 CHANNEL: 00 TARGET: 01 LUN: 00
 Library ID: 10 Slot: 01
 Vendor identification: IBM
 Product identification: ULT3580-TD5
 Unit serial number: OVTLDRV001

Drive: 12 CHANNEL: 00 TARGET: 02 LUN: 00
 Library ID: 10 Slot: 02
 Vendor identification: IBM
 Product identification: ULT3580-TD5
 Unit serial number: OVTLDRV002
`

func TestCreateLibraryWritesConf(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "pool2")
	os.MkdirAll(home, 0o755)
	if err := os.WriteFile(filepath.Join(dir, "device.conf"), []byte(oneLibConf), 0o644); err != nil {
		t.Fatal(err)
	}
	oldSysd := systemdDir
	systemdDir = filepath.Join(dir, "systemd")
	defer func() { systemdDir = oldSysd }()

	cfg := &config.Config{MhvtlConf: dir, MediaDir: "/opt/mhvtl"}
	e := New(cfg, events.NewBus(), nil, slog.Default())

	libID, serial, drives, err := e.CreateLibrary(context.Background(), CreateLibrarySpec{
		Product: "3573-TL", DriveProduct: "ULT3580-TD6", NumDrives: 2, HomeDir: home,
	})
	// ULT3580-TD6 fits the LTO family of 3573-TL — must succeed.
	if err != nil {
		t.Fatal(err)
	}
	if libID != 20 {
		t.Fatalf("allocated libID %d", libID)
	}
	// The library serial is now a fresh random OVTL###### — never the
	// sequential/legacy OVTLLIB run.
	if !regexp.MustCompile(`^OVTL\d{6}$`).MatchString(serial) || serial == "OVTLLIB001" {
		t.Fatalf("library serial %q: want a fresh random OVTL######", serial)
	}
	// Drive serials are now fresh random OVTD###### too, distinct from
	// each other and never the legacy OVTLDRV run.
	drvRe := regexp.MustCompile(`^OVTD\d{6}$`)
	if len(drives) != 2 || !drvRe.MatchString(drives[0]) || !drvRe.MatchString(drives[1]) || drives[0] == drives[1] {
		t.Fatalf("drive serials %v: want two distinct OVTD######", drives)
	}

	// Reparse: both libraries visible, drives attached, home dirs right.
	libs, err := ParseMhvtlConf(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(libs) != 2 {
		t.Fatalf("expected 2 libraries, got %+v", libs)
	}
	l20 := libs[1]
	if l20.ID != 20 || l20.Serial != serial || l20.HomeDir != home ||
		len(l20.Drives) != 2 || l20.Drives[0].QueueID != 21 || l20.Drives[1].QueueID != 22 ||
		l20.Drives[0].Product != "ULT3580-TD6" {
		t.Fatalf("lib20: %+v", l20)
	}
	// SCSI targets continue past lib10's three devices.
	conf, _ := os.ReadFile(filepath.Join(dir, "device.conf"))
	for _, want := range []string{
		"Library: 20 CHANNEL: 00 TARGET: 03 LUN: 00",
		"Drive: 21 CHANNEL: 00 TARGET: 04 LUN: 00",
		"Drive: 22 CHANNEL: 00 TARGET: 05 LUN: 00",
		"NAA: 30:00:00:00:00:00:00:20",
		"Compression: factor 1 enabled 0", // Patch-9 raw writes
	} {
		if !strings.Contains(string(conf), want) {
			t.Errorf("device.conf missing %q", want)
		}
	}
	// Empty contents file with the default geometry.
	lc, err := os.ReadFile(filepath.Join(dir, "library_contents.20"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(lc)
	if !strings.Contains(s, "Drive 2:") || !strings.Contains(s, "MAP 4:") ||
		!strings.Contains(s, "Slot 100:") || strings.Contains(s, "Slot 101:") ||
		strings.Contains(s, "Slot 1: O") {
		t.Fatalf("library_contents.20 malformed:\n%s", s[:200])
	}
	// Per-instance mount drop-ins.
	for _, u := range []string{"vtllibrary@20.service.d", "vtltape@21.service.d", "vtltape@22.service.d"} {
		b, err := os.ReadFile(filepath.Join(systemdDir, u, "openvtl-pool.conf"))
		if err != nil || !strings.Contains(string(b), "RequiresMountsFor="+home) {
			t.Errorf("dropin %s: %v %q", u, err, b)
		}
	}
	// A backup of the original conf exists exactly once.
	if _, err := os.Stat(filepath.Join(dir, "device.conf.bak-openvtl")); err != nil {
		t.Error("device.conf backup missing")
	}

	// Wrong family must refuse: 3592 drive in an LTO library.
	if _, _, _, err := e.CreateLibrary(context.Background(), CreateLibrarySpec{
		Product: "3573-TL", DriveProduct: "03592E06", NumDrives: 1, HomeDir: home,
	}); err == nil {
		t.Fatal("3592 drive in 3573-TL should be refused")
	}
	// Non-creatable personality must refuse.
	if _, _, _, err := e.CreateLibrary(context.Background(), CreateLibrarySpec{
		Product: "L700", DriveProduct: "T10000B", NumDrives: 1, HomeDir: home,
	}); err == nil {
		t.Fatal("non-catalog library should be refused")
	}
	// Over the model's drive cap must refuse (3573-TL caps at 4).
	if _, _, _, err := e.CreateLibrary(context.Background(), CreateLibrarySpec{
		Product: "3573-TL", DriveProduct: "ULT3580-TD5", NumDrives: 5, HomeDir: home,
	}); err == nil {
		t.Fatal("5 drives in a 3573-TL should be refused")
	}
}

// Regression (v1.0.0 DR blocker): after a delete leaves a surviving
// library on high SCSI targets, a new library must continue past the
// highest target IN USE — not restart at the device count, which
// re-issues the survivor's addresses and the kernel then refuses the
// duplicate LUs ("device struct already in place"), so the recovered
// library never registers.
func TestCreateLibraryAfterDeleteSkipsUsedTargets(t *testing.T) {
	// device.conf as left after: create lib 10 (targets 0-2), create
	// lib 20 (targets 3-5), delete lib 10. Only lib 20 remains.
	const survivorConf = `VERSION: 5

Library: 20 CHANNEL: 00 TARGET: 03 LUN: 00
 Vendor identification: IBM
 Product identification: 03584L32
 Unit serial number: OVTL843347
 NAA: 30:00:00:00:00:00:00:20
 Home directory: /opt/mhvtl
 PERSIST: True
 Backoff: 400
 Product revision level: D.00

Drive: 21 CHANNEL: 00 TARGET: 04 LUN: 00
 Library ID: 20 Slot: 01
 Vendor identification: IBM
 Product identification: ULT3580-TD5
 Unit serial number: OVTD382868

Drive: 22 CHANNEL: 00 TARGET: 05 LUN: 00
 Library ID: 20 Slot: 02
 Vendor identification: IBM
 Product identification: ULT3580-TD5
 Unit serial number: OVTD252431
`
	dir := t.TempDir()
	home := filepath.Join(dir, "pool1")
	os.MkdirAll(home, 0o755)
	if err := os.WriteFile(filepath.Join(dir, "device.conf"), []byte(survivorConf), 0o644); err != nil {
		t.Fatal(err)
	}
	oldSysd := systemdDir
	systemdDir = filepath.Join(dir, "systemd")
	defer func() { systemdDir = oldSysd }()

	cfg := &config.Config{MhvtlConf: dir, MediaDir: "/opt/mhvtl"}
	e := New(cfg, events.NewBus(), nil, slog.Default())

	libID, _, _, err := e.CreateLibrary(context.Background(), CreateLibrarySpec{
		Product: "3573-TL", DriveProduct: "ULT3580-TD5", NumDrives: 2, HomeDir: home,
		Serial: "OVTL280164", // recovery adopts the dead library's serial
	})
	if err != nil {
		t.Fatal(err)
	}
	if libID != 30 {
		t.Fatalf("allocated libID %d, want 30", libID)
	}
	conf, _ := os.ReadFile(filepath.Join(dir, "device.conf"))
	for _, want := range []string{
		"Library: 30 CHANNEL: 00 TARGET: 06 LUN: 00",
		"Drive: 31 CHANNEL: 00 TARGET: 07 LUN: 00",
		"Drive: 32 CHANNEL: 00 TARGET: 08 LUN: 00",
	} {
		if !strings.Contains(string(conf), want) {
			t.Errorf("device.conf missing %q — target allocation reused a survivor's address?\n%s", want, conf)
		}
	}
	// The survivor's addresses are untouched.
	if !strings.Contains(string(conf), "Library: 20 CHANNEL: 00 TARGET: 03 LUN: 00") {
		t.Error("survivor library block was modified")
	}
}

func TestCreateLibrary3584(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "pool2")
	os.MkdirAll(home, 0o755)
	if err := os.WriteFile(filepath.Join(dir, "device.conf"), []byte(oneLibConf), 0o644); err != nil {
		t.Fatal(err)
	}
	oldSysd := systemdDir
	systemdDir = filepath.Join(dir, "systemd")
	defer func() { systemdDir = oldSysd }()

	cfg := &config.Config{MhvtlConf: dir, MediaDir: "/opt/mhvtl"}
	e := New(cfg, events.NewBus(), nil, slog.Default())

	// A full 12-drive L32 — the real frame's drive complement.
	libID, _, drives, err := e.CreateLibrary(context.Background(), CreateLibrarySpec{
		Product: "03584L32", DriveProduct: "ULT3580-TD5", NumDrives: 12, HomeDir: home,
	})
	if err != nil {
		t.Fatal(err)
	}
	if libID != 20 || len(drives) != 12 {
		t.Fatalf("libID %d drives %d", libID, len(drives))
	}
	libs, err := ParseMhvtlConf(dir)
	if err != nil {
		t.Fatal(err)
	}
	l20 := libs[1]
	if l20.Product != "03584L32" || len(l20.Drives) != 12 ||
		l20.Drives[0].QueueID != 21 || l20.Drives[11].QueueID != 32 {
		t.Fatalf("lib20: %+v", l20)
	}

	// The 12 drives spilled past queue 29, so the next library must clear
	// every used queue id: 40, not a colliding 30.
	nextID, _, _, err := e.CreateLibrary(context.Background(), CreateLibrarySpec{
		Product: "3573-TL", DriveProduct: "ULT3580-TD5", NumDrives: 1, HomeDir: home,
	})
	if err != nil {
		t.Fatal(err)
	}
	if nextID != 40 {
		t.Fatalf("next library after a 12-drive lib 20 (queues 21-32) must be 40, got %d", nextID)
	}

	// 13 drives exceeds the 3584 frame cap.
	if _, _, _, err := e.CreateLibrary(context.Background(), CreateLibrarySpec{
		Product: "03584L32", DriveProduct: "ULT3580-TD5", NumDrives: 13, HomeDir: home,
	}); err == nil {
		t.Fatal("13 drives in a 3584 should be refused")
	}
	// Catalog-listed but non-creatable variants must refuse (operator
	// decision: only the spec-validated L32 until IBM i field validation).
	for _, p := range []string{"03584L53", "03584D32", "03584L23"} {
		dp := "ULT3580-TD5"
		if p == "03584L23" {
			dp = "03592E06"
		}
		if _, _, _, err := e.CreateLibrary(context.Background(), CreateLibrarySpec{
			Product: p, DriveProduct: dp, NumDrives: 1, HomeDir: home,
		}); err == nil {
			t.Fatalf("non-creatable variant %s should be refused", p)
		}
	}
	// Family gate: a 3592 drive does not fit an LTO frame.
	if _, _, _, err := e.CreateLibrary(context.Background(), CreateLibrarySpec{
		Product: "03584L32", DriveProduct: "03592E06", NumDrives: 1, HomeDir: home,
	}); err == nil {
		t.Fatal("3592 drive in an L32 (LTO) frame should be refused")
	}
	// The retired 3584-403 iSCSI VTL variant is gone from the catalog
	// (FC-only product since 2026-08-24) — creating it must refuse.
	if _, _, _, err := e.CreateLibrary(context.Background(), CreateLibrarySpec{
		Product: "03584403", DriveProduct: "ULT3580-TD5", NumDrives: 2, HomeDir: home,
	}); err == nil {
		t.Fatal("removed 03584403 variant should be refused")
	}
}
