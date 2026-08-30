package inventory

import (
	"os"
	"path/filepath"
	"testing"
)

// Two-library device.conf in the exact shape the installed mhVTL 1.8.0
// writes (verified against a live install's file), plus a
// library_contents for each.
const twoLibConf = `VERSION: 5

Library: 10 CHANNEL: 00 TARGET: 00 LUN: 00
 Vendor identification: IBM
 Product identification: 3573-TL
 Unit serial number: OVTLLIB001
 NAA: 30:00:00:00:00:00:00:10
 Home directory: /var/lib/openvtl/pools/pool1
 PERSIST: True
 Backoff: 400

Drive: 12 CHANNEL: 00 TARGET: 02 LUN: 00
 Library ID: 10 Slot: 02
 Vendor identification: IBM
 Product identification: ULT3580-TD5
 Unit serial number: OVTLDRV002
 Compression: factor 1 enabled 0

Drive: 11 CHANNEL: 00 TARGET: 01 LUN: 00
 Library ID: 10 Slot: 01
 Vendor identification: IBM
 Product identification: ULT3580-TD5
 Unit serial number: OVTLDRV001

Library: 20 CHANNEL: 00 TARGET: 03 LUN: 00
 Vendor identification: IBM
 Product identification: 03584L32
 Unit serial number: OVTLLIB002
 Home directory: /var/lib/openvtl/pools/pool2

Drive: 21 CHANNEL: 00 TARGET: 04 LUN: 00
 Library ID: 20 Slot: 01
 Vendor identification: IBM
 Product identification: ULT3580-TD6
 Unit serial number: OVTLDRV003
`

func TestParseMhvtlConfMultiLibrary(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "device.conf"), []byte(twoLibConf), 0o644); err != nil {
		t.Fatal(err)
	}
	lc10 := "VERSION: 2\n\nDrive 1:\nDrive 2:\n\nPicker 1:\n\nMAP 1:\n\nSlot 1: OVT001L5\nSlot 2:\n"
	if err := os.WriteFile(filepath.Join(dir, "library_contents.10"), []byte(lc10), 0o644); err != nil {
		t.Fatal(err)
	}

	libs, err := ParseMhvtlConf(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(libs) != 2 || libs[0].ID != 10 || libs[1].ID != 20 {
		t.Fatalf("libraries: %+v", libs)
	}
	l10 := libs[0]
	if l10.Serial != "OVTLLIB001" || l10.Product != "3573-TL" ||
		l10.HomeDir != "/var/lib/openvtl/pools/pool1" {
		t.Fatalf("lib10: %+v", l10)
	}
	// Drives declared out of order in the file must sort by Slot.
	if len(l10.Drives) != 2 || l10.Drives[0].QueueID != 11 || l10.Drives[1].QueueID != 12 {
		t.Fatalf("lib10 drives not slot-ordered: %+v", l10.Drives)
	}
	if l10.Drives[0].Serial != "OVTLDRV001" || l10.Drives[0].Slot != 1 || l10.Drives[0].LibraryID != 10 {
		t.Fatalf("lib10 drive0: %+v", l10.Drives[0])
	}
	if len(l10.Barcodes) != 1 || l10.Barcodes[0] != "OVT001L5" {
		t.Fatalf("lib10 barcodes: %v", l10.Barcodes)
	}
	l20 := libs[1]
	if l20.Serial != "OVTLLIB002" || l20.Product != "03584L32" ||
		len(l20.Drives) != 1 || l20.Drives[0].QueueID != 21 ||
		l20.Drives[0].Product != "ULT3580-TD6" {
		t.Fatalf("lib20: %+v", l20)
	}
	if len(l20.Barcodes) != 0 {
		t.Fatalf("lib20 barcodes should be empty (no contents file): %v", l20.Barcodes)
	}
}
