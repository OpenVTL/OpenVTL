package store

import (
	"context"
	"testing"
)

// Migration 004 must apply on a fresh DB (Open runs it); pools and
// libraries round-trip, and a pool can't be deleted out from under a
// library that calls it home.
func TestPoolLibraryRoundTrip(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	poolID, err := s.CreatePool(ctx, Pool{
		Name: "pool1", VG: "vg_vtl", DataLV: "pool1_data",
		Mountpoint: "/var/lib/openvtl/pools/pool1", DataDev: "/dev/sdc",
		CacheSliceBytes: 25 << 30, VirtualSizeBytes: 2560 << 30,
		State: PoolCreating,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetPoolState(ctx, poolID, PoolActive, ""); err != nil {
		t.Fatal(err)
	}
	p, err := s.GetPool(ctx, poolID)
	if err != nil || p.State != PoolActive || p.CacheSliceBytes != 25<<30 {
		t.Fatalf("GetPool: %+v err=%v", p, err)
	}
	if _, err := s.CreatePool(ctx, Pool{Name: "pool1", VG: "x", DataLV: "y",
		Mountpoint: "/other", DataDev: "/dev/sdz", State: PoolCreating}); err == nil {
		t.Fatal("duplicate pool name should be rejected")
	}

	lib := Library{
		ID: 10, Name: "OVTLLIB001", Vendor: "IBM", Product: "3573-TL",
		Serial: "OVTLLIB001", DriveModel: "ULT3580-TD5", NumDrives: 2,
		LabelPrefix: "OVT", MediaDir: "/var/lib/openvtl/pools/pool1",
		HomePool: poolID, State: LibraryPendingRestart,
	}
	if err := s.CreateLibrary(ctx, lib); err != nil {
		t.Fatal(err)
	}
	if err := s.DeletePool(ctx, poolID); err == nil {
		t.Fatal("pool delete must refuse while a library lives on it")
	}
	if err := s.SetLibraryState(ctx, 10, LibraryActive); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetLibrary(ctx, 10)
	if err != nil || got.State != LibraryActive || got.HomePool != poolID ||
		got.DriveModel != "ULT3580-TD5" {
		t.Fatalf("GetLibrary: %+v err=%v", got, err)
	}
	libs, err := s.ListLibraries(ctx)
	if err != nil || len(libs) != 1 {
		t.Fatalf("ListLibraries: %v err=%v", libs, err)
	}

	// cartridge.library_id column exists and is stamped by upsert
	if err := s.UpsertCartridge(ctx, "OVT001L5", 0, "slot:1", nil, 10); err != nil {
		t.Fatal(err)
	}
	var libID int
	if err := s.db.QueryRow(
		`SELECT library_id FROM cartridge WHERE label = 'OVT001L5'`).Scan(&libID); err != nil || libID != 10 {
		t.Fatalf("cartridge.library_id = %d err=%v", libID, err)
	}

	if err := s.DeleteLibrary(ctx, 10); err != nil {
		t.Fatal(err)
	}
	if err := s.DeletePool(ctx, poolID); err != nil {
		t.Fatalf("pool delete after library removal: %v", err)
	}
	if s.Setting(ctx, CacheDeviceKey, "") != "" {
		t.Fatal("cache device should be unset on a fresh system")
	}
	if err := s.SetSetting(ctx, CacheDeviceKey, "/dev/disk/by-id/scsi-cache"); err != nil {
		t.Fatal(err)
	}
	if got := s.Setting(ctx, CacheDeviceKey, ""); got != "/dev/disk/by-id/scsi-cache" {
		t.Fatalf("cache device setting = %q", got)
	}
}
