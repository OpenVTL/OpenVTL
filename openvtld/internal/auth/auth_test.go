package auth

import (
	"strings"
	"testing"
)

func TestHashVerifyRoundTrip(t *testing.T) {
	phc, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(phc, "$argon2id$v=19$m=65536,t=1,p=4$") {
		t.Fatalf("unexpected PHC prefix: %s", phc)
	}
	if !VerifyPassword("correct horse battery staple", phc) {
		t.Fatal("correct password rejected")
	}
	if VerifyPassword("wrong", phc) {
		t.Fatal("wrong password accepted")
	}
	if VerifyPassword("", phc) {
		t.Fatal("empty password accepted")
	}
}

func TestHashUnique(t *testing.T) {
	a, _ := HashPassword("x")
	b, _ := HashPassword("x")
	if a == b {
		t.Fatal("two hashes of the same password share a salt")
	}
}

func TestVerifyMalformed(t *testing.T) {
	for _, bad := range []string{"", "$argon2id$", "$argon2i$v=19$m=1,t=1,p=1$AA$AA", "plainhash", "$argon2id$v=18$m=65536,t=1,p=4$AA$AA"} {
		if VerifyPassword("x", bad) {
			t.Fatalf("malformed hash %q verified", bad)
		}
	}
}

func TestTokens(t *testing.T) {
	raw, hash, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) < 40 {
		t.Fatalf("token too short: %d", len(raw))
	}
	if HashToken(raw) != hash {
		t.Fatal("HashToken(raw) != returned hash")
	}
	raw2, hash2, _ := NewToken()
	if raw == raw2 || hash == hash2 {
		t.Fatal("two tokens identical")
	}
}
