// Package release is the trust anchor for signed update bundles.
//
// A release bundle carries a SHA256SUMS covering every file plus a detached
// Ed25519 signature over SHA256SUMS (SHA256SUMS.sig, base64). Verification is
// pure Go (crypto/ed25519) so the appliance binary stays CGO-free and needs no
// external tool: check the signature over SHA256SUMS with the embedded public
// key, then verify every listed file against SHA256SUMS. One signature protects
// the whole bundle transitively.
//
// The public key is committed (it is public) in pubkey.b64 and baked into the
// binary here via go:embed — the single source of truth. install.sh carries the
// same key as a literal for the very first install (make-release.sh fails the cut
// if the two drift). The matching PRIVATE key stays OFFLINE on the dev box (a
// gitignored keys/ dir), read only by cmd/release-tool at release time.
package release

import (
	"bufio"
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Canonical file names inside a bundle.
const (
	SumsName    = "SHA256SUMS"
	SigName     = "SHA256SUMS.sig"
	VersionName = "VERSION"
)

//go:embed pubkey.b64
var pubkeyB64 string

// PublicKey decodes the embedded release-signing public key.
func PublicKey() (ed25519.PublicKey, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(pubkeyB64))
	if err != nil {
		return nil, fmt.Errorf("decode embedded pubkey: %w", err)
	}
	if len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("embedded pubkey is %d bytes, want %d", len(raw), ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(raw), nil
}

// PublicKeyB64 returns the embedded public key as committed (base64 of the raw
// 32 bytes) — used by cmd/release-tool to check install.sh has the same literal.
func PublicKeyB64() string { return strings.TrimSpace(pubkeyB64) }

// VerifyDetached checks a base64 Ed25519 signature over msg against the embedded
// public key.
func VerifyDetached(msg []byte, sigB64 string) error {
	pub, err := PublicKey()
	if err != nil {
		return err
	}
	return verifyDetachedWith(pub, msg, sigB64)
}

func verifyDetachedWith(pub ed25519.PublicKey, msg []byte, sigB64 string) error {
	sig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(sigB64))
	if err != nil {
		return fmt.Errorf("decode signature: %w", err)
	}
	if len(sig) != ed25519.SignatureSize {
		return fmt.Errorf("signature is %d bytes, want %d", len(sig), ed25519.SignatureSize)
	}
	if !ed25519.Verify(pub, msg, sig) {
		return errors.New("signature does not verify against the release key (tampered, corrupt, or unofficial bundle)")
	}
	return nil
}

// VerifyBundleDir verifies an unpacked bundle rooted at dir: the signature over
// SHA256SUMS, then every file listed in SHA256SUMS. Any mismatch is an error.
func VerifyBundleDir(dir string) error {
	pub, err := PublicKey()
	if err != nil {
		return err
	}
	return verifyBundleDirWith(pub, dir)
}

func verifyBundleDirWith(pub ed25519.PublicKey, dir string) error {
	sums, err := os.ReadFile(filepath.Join(dir, SumsName))
	if err != nil {
		return fmt.Errorf("read %s: %w", SumsName, err)
	}
	sig, err := os.ReadFile(filepath.Join(dir, SigName))
	if err != nil {
		return fmt.Errorf("read %s: %w (unsigned bundle?)", SigName, err)
	}
	if err := verifyDetachedWith(pub, sums, string(sig)); err != nil {
		return fmt.Errorf("release signature: %w", err)
	}
	return verifySums(dir, sums)
}

// verifySums hashes every file named in a SHA256SUMS body and compares. The
// coreutils format is "<hex>  <path>" (two spaces; a leading '*' marks binary
// mode). Paths are bundle-relative (typically "./bin/openvtld").
func verifySums(dir string, sums []byte) error {
	sc := bufio.NewScanner(bytes.NewReader(sums))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	n := 0
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		sp := strings.IndexByte(line, ' ')
		if sp < 0 {
			return fmt.Errorf("malformed SHA256SUMS line: %q", line)
		}
		want := line[:sp]
		path := strings.TrimLeft(line[sp:], " ")
		path = strings.TrimPrefix(path, "*") // binary-mode marker
		// Never let a crafted SHA256SUMS reach outside the bundle.
		clean := filepath.Clean(filepath.FromSlash(path))
		if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || filepath.IsAbs(clean) {
			return fmt.Errorf("SHA256SUMS path escapes bundle: %q", path)
		}
		got, err := hashFile(filepath.Join(dir, clean))
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		if !strings.EqualFold(got, want) {
			return fmt.Errorf("checksum mismatch: %s", path)
		}
		n++
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if n == 0 {
		return errors.New("SHA256SUMS lists no files")
	}
	return nil
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Version is the parsed bundle VERSION manifest (see make-release.sh).
type Version struct {
	Hash      string // short commit hash, e.g. "4afa261"
	BuildTime string // RFC3339 "built:" line — the monotonic ordering key
	MhvtlPin  string // vendored mhVTL pin, e.g. "mhvtl-8e79aa8" — Tier-B signal
}

// ParseVersionDir reads and parses VERSION from an unpacked bundle.
func ParseVersionDir(dir string) (Version, error) {
	b, err := os.ReadFile(filepath.Join(dir, VersionName))
	if err != nil {
		return Version{}, err
	}
	return ParseVersion(b), nil
}

// ParseVersion extracts the fields the updater compares from a VERSION body.
func ParseVersion(b []byte) Version {
	var v Version
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case strings.HasPrefix(line, "openvtl "):
			v.Hash = strings.TrimSpace(strings.TrimPrefix(line, "openvtl "))
		case strings.HasPrefix(line, "built:"):
			v.BuildTime = strings.TrimSpace(strings.TrimPrefix(line, "built:"))
		case strings.HasPrefix(line, "mhvtl:"):
			// "mhvtl: mhvtl-8e79aa8 (vendored ...)" — take the first field.
			rest := strings.Fields(strings.TrimPrefix(line, "mhvtl:"))
			if len(rest) > 0 {
				v.MhvtlPin = rest[0]
			}
		}
	}
	return v
}
