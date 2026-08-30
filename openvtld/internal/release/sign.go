package release

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"
)

// Signing helpers live in this package so the verify and sign code share one
// definition of the wire format, but they are only ever called by
// cmd/release-tool on the dev box. The appliance binary never loads a private
// key — LoadPrivateKey reads a file that exists only in the gitignored keys/ dir.

// GenerateKey returns a fresh Ed25519 keypair. The seed (32 bytes) is what gets
// stored offline; the public key is what gets committed.
func GenerateKey() (pub ed25519.PublicKey, seed []byte, err error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	return pub, priv.Seed(), nil
}

// LoadPrivateKey reads a base64-encoded 32-byte Ed25519 seed from path and
// expands it to a signing key.
func LoadPrivateKey(path string) (ed25519.PrivateKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
	if err != nil {
		return nil, fmt.Errorf("decode signing key: %w", err)
	}
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("signing key seed is %d bytes, want %d", len(seed), ed25519.SeedSize)
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

// Sign returns the base64 detached signature over msg — the exact form written
// to SHA256SUMS.sig and checked by VerifyDetached.
func Sign(priv ed25519.PrivateKey, msg []byte) string {
	return base64.StdEncoding.EncodeToString(ed25519.Sign(priv, msg))
}

// EncodeSeed / EncodePublic render keys in the on-disk base64 forms.
func EncodeSeed(seed []byte) string { return base64.StdEncoding.EncodeToString(seed) }

func EncodePublic(pub ed25519.PublicKey) string { return base64.StdEncoding.EncodeToString(pub) }

// PublicPEM renders an Ed25519 public key as a SubjectPublicKeyInfo PEM — the
// form install.sh feeds to `openssl pkeyutl -verify`. Ed25519 SPKI is a fixed
// 12-byte DER prefix followed by the 32-byte key.
func PublicPEM(pub ed25519.PublicKey) (string, error) {
	if len(pub) != ed25519.PublicKeySize {
		return "", errors.New("bad public key size")
	}
	der := append([]byte{
		0x30, 0x2a, 0x30, 0x05, 0x06, 0x03, 0x2b, 0x65, 0x70, 0x03, 0x21, 0x00,
	}, pub...)
	body := base64.StdEncoding.EncodeToString(der)
	return "-----BEGIN PUBLIC KEY-----\n" + body + "\n-----END PUBLIC KEY-----\n", nil
}
