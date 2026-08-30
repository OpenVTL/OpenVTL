package license

import (
	"regexp"
	"testing"
)

var keyRe = regexp.MustCompile(`^OVTL(-[0-9ABCDEFGHJKMNPQRSTVWXYZ]{5}){4}$`)

func TestDeriveShapeDeterminismRekey(t *testing.T) {
	a := derive("machine-aaaa", "dmi:board-1")
	if !keyRe.MatchString(a) {
		t.Fatalf("bad key format: %q", a)
	}
	if a != derive("machine-aaaa", "dmi:board-1") {
		t.Fatal("not deterministic")
	}
	// Any identity change re-keys.
	if a == derive("machine-bbbb", "dmi:board-1") {
		t.Fatal("machine-id change did not re-key")
	}
	if a == derive("machine-aaaa", "dmi:board-2") {
		t.Fatal("board change did not re-key")
	}
	// A missing board id (degraded) still yields a valid, distinct, stable key.
	d := derive("machine-aaaa", "")
	if !keyRe.MatchString(d) || d == a {
		t.Fatalf("degraded key wrong: %q", d)
	}
}

func TestValidChecksum(t *testing.T) {
	k := derive("machine-xyz", "dmi:board-9")
	if !Valid(k) {
		t.Fatalf("derived key fails Valid: %q", k)
	}
	// Corrupting the checksum symbol must be rejected.
	bad := []byte(k)
	repl := byte(crockford[0])
	if bad[len(bad)-1] == repl {
		repl = crockford[1]
	}
	bad[len(bad)-1] = repl
	if Valid(string(bad)) {
		t.Fatal("a wrong checksum symbol was accepted")
	}
	// Wrong length / prefix rejected.
	if Valid("OVTL-XXX") || Valid("NOPE-00000-00000-00000-00000") {
		t.Fatal("malformed key accepted")
	}
}
