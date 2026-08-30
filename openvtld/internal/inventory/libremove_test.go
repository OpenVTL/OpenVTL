package inventory

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openvtl/openvtld/internal/config"
	"github.com/openvtl/openvtld/internal/events"
)

// RemoveLibrary must excise exactly the target library block and its
// drive blocks — the survivor stays byte-meaningful (re-parseable with
// identical fields) because a mistake here bricks every library at
// the next mhVTL restart.
func TestRemoveLibraryExcisesOnlyItsBlocks(t *testing.T) {
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

	// Second library via the real writer, then remove it again.
	libID, _, _, err := e.CreateLibrary(context.Background(), CreateLibrarySpec{
		Product: "3573-TL", DriveProduct: "ULT3580-TD6", NumDrives: 2, HomeDir: home,
	})
	if err != nil {
		t.Fatal(err)
	}
	if libs, _ := ParseMhvtlConf(dir); len(libs) != 2 {
		t.Fatalf("setup: want 2 libraries, got %d", len(libs))
	}
	dropin := filepath.Join(systemdDir, "vtllibrary@20.service.d")
	if _, err := os.Stat(dropin); err != nil {
		t.Fatalf("setup: drop-in missing: %v", err)
	}

	if err := e.RemoveLibrary(context.Background(), libID); err != nil {
		t.Fatal(err)
	}

	libs, err := ParseMhvtlConf(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(libs) != 1 || libs[0].ID != 10 {
		t.Fatalf("want only library 10 left, got %+v", libs)
	}
	if len(libs[0].Drives) != 2 || libs[0].Drives[0].Serial != "OVTLDRV001" || libs[0].Drives[1].Serial != "OVTLDRV002" {
		t.Fatalf("library 10 drives damaged: %+v", libs[0].Drives)
	}
	if libs[0].Serial != "OVTLLIB001" || libs[0].Product != "3573-TL" || libs[0].HomeDir != "/opt/mhvtl" {
		t.Fatalf("library 10 fields damaged: %+v", libs[0])
	}
	conf, _ := os.ReadFile(filepath.Join(dir, "device.conf"))
	for _, gone := range []string{"Library: 20", "Drive: 21", "Drive: 22", "OVTLLIB002"} {
		if strings.Contains(string(conf), gone) {
			t.Errorf("device.conf still contains %q", gone)
		}
	}
	if _, err := os.Stat(dropin); !os.IsNotExist(err) {
		t.Error("vtllibrary@20 drop-in survived removal")
	}
	// contents file is a separate, later step
	lc := filepath.Join(dir, "library_contents.20")
	if _, err := os.Stat(lc); err != nil {
		t.Fatal("library_contents.20 should survive RemoveLibrary")
	}
	e.RemoveLibraryContents(libID)
	if _, err := os.Stat(lc); !os.IsNotExist(err) {
		t.Error("library_contents.20 should be gone after RemoveLibraryContents")
	}

	// Unknown id refuses.
	if err := e.RemoveLibrary(context.Background(), 90); err == nil {
		t.Error("removing an unknown library must error")
	}
}
