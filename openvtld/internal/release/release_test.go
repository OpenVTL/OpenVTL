package release

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

// makeBundle writes a minimal signed bundle into a temp dir: a couple of files,
// a coreutils-style SHA256SUMS covering them, and a detached signature over
// SHA256SUMS made with priv. Returns the dir.
func makeBundle(t *testing.T, priv ed25519.PrivateKey, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	var sums []byte
	for name, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		h := sha256.Sum256([]byte(content))
		sums = append(sums, []byte(hex.EncodeToString(h[:])+"  ./"+name+"\n")...)
	}
	if err := os.WriteFile(filepath.Join(dir, SumsName), sums, 0o644); err != nil {
		t.Fatal(err)
	}
	sig := Sign(priv, sums)
	if err := os.WriteFile(filepath.Join(dir, SigName), []byte(sig+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestVerifyBundleRoundTrip(t *testing.T) {
	pub, seed, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	priv := ed25519.NewKeyFromSeed(seed)

	dir := makeBundle(t, priv, map[string]string{
		"bin/openvtld": "ELF-ish bytes",
		"VERSION":      "openvtl abc1234\nbuilt: 2026-07-07T00:00:00Z\n",
	})
	if err := verifyBundleDirWith(pub, dir); err != nil {
		t.Fatalf("clean bundle should verify: %v", err)
	}

	// Tampered file: same SHA256SUMS, changed content -> checksum mismatch.
	if err := os.WriteFile(filepath.Join(dir, "bin", "openvtld"), []byte("evil"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := verifyBundleDirWith(pub, dir); err == nil {
		t.Fatal("tampered file must be rejected")
	}
}

func TestBadSignatureRejected(t *testing.T) {
	pub, seed, _ := GenerateKey()
	priv := ed25519.NewKeyFromSeed(seed)
	dir := makeBundle(t, priv, map[string]string{"VERSION": "openvtl abc1234\n"})

	// Re-sign SHA256SUMS with a DIFFERENT key -> signature must not verify
	// against pub (the attacker-can't-forge case).
	_, seed2, _ := GenerateKey()
	priv2 := ed25519.NewKeyFromSeed(seed2)
	sums, _ := os.ReadFile(filepath.Join(dir, SumsName))
	if err := os.WriteFile(filepath.Join(dir, SigName), []byte(Sign(priv2, sums)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := verifyBundleDirWith(pub, dir); err == nil {
		t.Fatal("signature from the wrong key must be rejected")
	}
}

func TestWrongSumsRejected(t *testing.T) {
	pub, seed, _ := GenerateKey()
	priv := ed25519.NewKeyFromSeed(seed)
	dir := makeBundle(t, priv, map[string]string{"VERSION": "openvtl abc1234\n"})

	// Edit SHA256SUMS after signing -> signature no longer matches.
	if err := os.WriteFile(filepath.Join(dir, SumsName), []byte("deadbeef  ./VERSION\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := verifyBundleDirWith(pub, dir); err == nil {
		t.Fatal("modified SHA256SUMS must be rejected")
	}
}

func TestPathEscapeRejected(t *testing.T) {
	pub, seed, _ := GenerateKey()
	priv := ed25519.NewKeyFromSeed(seed)
	dir := t.TempDir()
	// A crafted SHA256SUMS referencing a parent path must be refused even if
	// the signature is valid over it.
	sums := []byte("deadbeef  ../escape\n")
	if err := os.WriteFile(filepath.Join(dir, SumsName), sums, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, SigName), []byte(Sign(priv, sums)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := verifyBundleDirWith(pub, dir); err == nil {
		t.Fatal("path escaping the bundle must be rejected")
	}
}

func TestEmbeddedPublicKeyValid(t *testing.T) {
	pub, err := PublicKey()
	if err != nil {
		t.Fatalf("committed pubkey.b64 must decode to a valid Ed25519 key: %v", err)
	}
	if len(pub) != ed25519.PublicKeySize {
		t.Fatalf("embedded key size %d", len(pub))
	}
}

func TestPublicPEMConcatMatchesInstaller(t *testing.T) {
	// The install.sh trick builds the SPKI PEM by prepending the fixed base64
	// prefix "MCowBQYDK2VwAyEA" to the raw-key base64. That is only valid
	// because the 12-byte DER prefix is a multiple of 3 bytes. Assert the Go
	// PEM equals that concatenation for the embedded key.
	pub, err := PublicKey()
	if err != nil {
		t.Fatal(err)
	}
	pem, err := PublicPEM(pub)
	if err != nil {
		t.Fatal(err)
	}
	want := "-----BEGIN PUBLIC KEY-----\nMCowBQYDK2VwAyEA" + PublicKeyB64() + "\n-----END PUBLIC KEY-----\n"
	if pem != want {
		t.Fatalf("PEM concat mismatch:\n got: %q\nwant: %q", pem, want)
	}
}

func TestParseVersion(t *testing.T) {
	v := ParseVersion([]byte("openvtl 4afa261\nbuilt: 2026-07-07T12:00:00Z\nopenvtld: 4afa261 (linux/amd64)\nmhvtl: mhvtl-8e79aa8 (vendored source)\ninstaller: repo/packaging/install.sh\n"))
	if v.Hash != "4afa261" {
		t.Errorf("hash %q", v.Hash)
	}
	if v.BuildTime != "2026-07-07T12:00:00Z" {
		t.Errorf("built %q", v.BuildTime)
	}
	if v.MhvtlPin != "mhvtl-8e79aa8" {
		t.Errorf("pin %q", v.MhvtlPin)
	}
}
