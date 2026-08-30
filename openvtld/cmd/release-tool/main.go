// release-tool — dev-box helper for OpenVTL release signing (v0.8).
//
// NOT shipped in the appliance bundle. It handles the private-key side of the
// signing scheme — generate the vendor keypair,
// sign SHA256SUMS at release time, and guard against install.sh drifting from
// the committed public key.
//
//	go run ./cmd/release-tool keygen [--force]   generate the vendor keypair
//	go run ./cmd/release-tool sign <file> [--key path]   detached-sign a file
//	go run ./cmd/release-tool pubkey             print the embedded public key (b64)
//	go run ./cmd/release-tool check-installer <install.sh>   assert install.sh key matches
//
// Run from the openvtld module directory. keygen writes the private seed to the
// gitignored ../keys/, the public key to internal/release/pubkey.b64 (committed,
// baked into the binary) and pubkey.pem (reference).
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/openvtl/openvtld/internal/release"
)

const (
	defaultKeysDir  = "../keys"
	defaultKeyFile  = "../keys/openvtl-signing.key"
	pubB64Path      = "internal/release/pubkey.b64"
	pubPEMPath      = "internal/release/pubkey.pem"
	defaultInstall  = "../packaging/install.sh"
	signingKeyEnv   = "OPENVTL_SIGNING_KEY"
	installerKeyVar = "RELEASE_PUBKEY_B64"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "keygen":
		keygen(os.Args[2:])
	case "sign":
		sign(os.Args[2:])
	case "pubkey":
		fmt.Println(release.PublicKeyB64())
	case "check-installer":
		path := defaultInstall
		if len(os.Args) > 2 {
			path = os.Args[2]
		}
		checkInstaller(path)
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: release-tool keygen|sign <file>|pubkey|check-installer [args]")
	os.Exit(2)
}

func die(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "[x] "+format+"\n", a...)
	os.Exit(1)
}

func keygen(args []string) {
	force := false
	for _, a := range args {
		if a == "--force" {
			force = true
		}
	}
	if _, err := os.Stat(defaultKeyFile); err == nil && !force {
		die("%s already exists — rotating the signing key means shipping a new installer/binary. Pass --force only if you mean it.", defaultKeyFile)
	}
	pub, seed, err := release.GenerateKey()
	if err != nil {
		die("generate: %v", err)
	}
	if err := os.MkdirAll(defaultKeysDir, 0o700); err != nil {
		die("mkdir keys: %v", err)
	}
	if err := os.WriteFile(defaultKeyFile, []byte(release.EncodeSeed(seed)+"\n"), 0o600); err != nil {
		die("write key: %v", err)
	}
	pubB64 := release.EncodePublic(pub)
	if err := os.WriteFile(pubB64Path, []byte(pubB64+"\n"), 0o644); err != nil {
		die("write pubkey.b64: %v", err)
	}
	pem, err := release.PublicPEM(pub)
	if err != nil {
		die("pem: %v", err)
	}
	if err := os.WriteFile(pubPEMPath, []byte(pem), 0o644); err != nil {
		die("write pubkey.pem: %v", err)
	}
	fmt.Printf("[+] private seed  -> %s  (OFFLINE, gitignored — never commit)\n", defaultKeyFile)
	fmt.Printf("[+] public key    -> %s  (commit this)\n", pubB64Path)
	fmt.Printf("[+] public PEM    -> %s  (commit this)\n", pubPEMPath)
	fmt.Println()
	fmt.Println("Update the install.sh literal to match (single source of truth):")
	fmt.Printf("    %s='%s'\n", installerKeyVar, pubB64)
	fmt.Println()
	fmt.Printf("Then export the key for release cuts:  export %s=%q\n", signingKeyEnv, mustAbs(defaultKeyFile))
}

func sign(args []string) {
	keyPath := os.Getenv(signingKeyEnv)
	var files []string
	for i := 0; i < len(args); i++ {
		if args[i] == "--key" && i+1 < len(args) {
			keyPath = args[i+1]
			i++
			continue
		}
		files = append(files, args[i])
	}
	if len(files) != 1 {
		die("sign takes exactly one file (got %d)", len(files))
	}
	if keyPath == "" {
		die("no signing key: set $%s or pass --key <path>", signingKeyEnv)
	}
	priv, err := release.LoadPrivateKey(keyPath)
	if err != nil {
		die("load key: %v", err)
	}
	msg, err := os.ReadFile(files[0])
	if err != nil {
		die("read %s: %v", files[0], err)
	}
	sig := release.Sign(priv, msg)
	out := files[0] + ".sig"
	if err := os.WriteFile(out, []byte(sig+"\n"), 0o644); err != nil {
		die("write %s: %v", out, err)
	}
	// Fail loud if the key we signed with does not match the committed pubkey —
	// a signature the appliance can't verify is worse than none.
	if err := release.VerifyDetached(msg, sig); err != nil {
		die("SELF-CHECK FAILED: %v — the signing key does not match the committed public key.", err)
	}
	fmt.Printf("[+] signed %s -> %s (verifies against the embedded public key)\n", files[0], out)
}

var installerKeyRe = regexp.MustCompile(installerKeyVar + `='([A-Za-z0-9+/=]+)'`)

func checkInstaller(path string) {
	b, err := os.ReadFile(path)
	if err != nil {
		die("read %s: %v", path, err)
	}
	m := installerKeyRe.FindSubmatch(b)
	if m == nil {
		die("%s has no %s='...' literal", path, installerKeyVar)
	}
	got := string(m[1])
	want := release.PublicKeyB64()
	if got != want {
		die("install.sh public key drifted from %s\n    install.sh: %s\n    committed : %s\n    (rotate deliberately, or restore the literal)", pubB64Path, got, want)
	}
	fmt.Printf("[+] install.sh public key matches %s\n", pubB64Path)
}

func mustAbs(p string) string {
	a, err := filepath.Abs(p)
	if err != nil {
		return strings.TrimPrefix(p, "../")
	}
	return a
}
