package update

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildNewer(t *testing.T) {
	cases := []struct {
		bundle, cur      string
		newer, comparabl bool
	}{
		{"2026-07-08T00:00:00Z", "2026-07-07T00:00:00Z", true, true},
		{"2026-07-07T00:00:00Z", "2026-07-08T00:00:00Z", false, true},
		{"2026-07-07T00:00:00Z", "2026-07-07T00:00:00Z", false, true}, // equal is not newer
		{"2026-07-08T00:00:00Z", "unknown", false, false},             // dev binary -> incomparable
		{"", "2026-07-07T00:00:00Z", false, false},
	}
	for _, c := range cases {
		n, ok := buildNewer(c.bundle, c.cur)
		if n != c.newer || ok != c.comparabl {
			t.Errorf("buildNewer(%q,%q)=(%v,%v) want (%v,%v)", c.bundle, c.cur, n, ok, c.newer, c.comparabl)
		}
	}
}

func TestMhvtlPinChanged(t *testing.T) {
	dir := t.TempDir()
	pinFile := filepath.Join(dir, ".openvtl-pin")
	if err := os.WriteFile(pinFile, []byte("8e79aa8\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Bundle VERSION carries the "mhvtl-" prefix; installed file does not.
	if changed, known := mhvtlPinChanged("mhvtl-8e79aa8", pinFile); changed || !known {
		t.Errorf("same pin: changed=%v known=%v", changed, known)
	}
	if changed, known := mhvtlPinChanged("mhvtl-deadbee", pinFile); !changed || !known {
		t.Errorf("different pin should be a Tier-B change: changed=%v known=%v", changed, known)
	}
	if _, known := mhvtlPinChanged("mhvtl-8e79aa8", filepath.Join(dir, "absent")); known {
		t.Error("missing installed pin should be unknown, not a false match")
	}
}

func TestMarkerRoundTripAndEnv(t *testing.T) {
	dir := t.TempDir()
	p := Paths{StateDir: dir}
	m := Marker{
		FromVersion: "aaa1111", ToVersion: "bbb2222",
		Binary: "/usr/local/bin/openvtld", PrevBinary: "/usr/local/bin/openvtld.prev",
		DBPath: "/var/lib/openvtld/openvtld.db", DBSnapshot: "/var/lib/openvtld/backups/x.db",
		AttemptLimit: 3,
	}
	if err := writeMarker(p, m); err != nil {
		t.Fatal(err)
	}
	got, ok := readMarker(p)
	if !ok || got.ToVersion != "bbb2222" || got.DBSnapshot != m.DBSnapshot {
		t.Fatalf("marker round-trip: %+v ok=%v", got, ok)
	}
	// The sourceable twin the watchdog reads must carry the fields it needs.
	env, err := os.ReadFile(p.markerEnv())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"OVTL_PREV_BINARY=/usr/local/bin/openvtld.prev",
		"OVTL_DB_SNAPSHOT=/var/lib/openvtld/backups/x.db",
		"OVTL_ATTEMPT_LIMIT=3",
	} {
		if !strings.Contains(string(env), want) {
			t.Errorf("env missing %q in:\n%s", want, env)
		}
	}
	// The attempts counter starts at 0.
	att, _ := os.ReadFile(p.attempts())
	if string(att) != "0\n" {
		t.Errorf("attempts = %q, want 0", att)
	}
	clearMarker(p)
	if _, ok := readMarker(p); ok {
		t.Error("marker should be gone after clear")
	}
}

func TestUnpackBundleRejectsMissing(t *testing.T) {
	dir := t.TempDir()
	if _, err := unpackBundle(filepath.Join(dir, "nope.tar.gz"), dir); err == nil {
		t.Error("missing tarball should error")
	}
}
