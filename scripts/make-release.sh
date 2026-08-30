#!/usr/bin/env bash
#
# OpenVTL — assemble the release bundle: the self-contained artifact the
# installer consumes; no network, no Go, no Node needed on the appliance.
#
#   releases/openvtl-<version>.tar.gz
#     openvtl-<version>/
#       bin/openvtld        linux/amd64 static binary, web UI embedded
#       repo/               the source tree: packaging/ (install.sh,
#                           patches, dkms, systemd), scripts/, docs/, tests/,
#                           openvtld/ sources, third_party/mhvtl-<pin>.tar.gz
#                           (vendored).
#       VERSION             component manifest
#       SHA256SUMS          integrity for install.sh --verify
#
# Runs anywhere with bash, git, tar, gzip, coreutils, Go, and npm on PATH;
# OPENVTL_SIGNING_KEY must point at the offline signing key. The bundle is
# built from HEAD — uncommitted changes are deliberately excluded (warned
# below); commit first, then cut.
#
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO"

# A tag names the release (v1.0.0 → openvtl-v1.0.0); untagged HEADs fall
# back to the short commit hash.
VER="$(git describe --tags --always)"
if [[ -n "$(git status --porcelain)" ]]; then
  echo "[!] working tree dirty — the bundle is cut from HEAD ($VER); uncommitted changes are EXCLUDED." >&2
fi

MHVTL_TAR="$(ls third_party/mhvtl-*.tar.gz 2>/dev/null | head -1)"
[[ -n "$MHVTL_TAR" ]] || { echo "[x] third_party/mhvtl-*.tar.gz missing — the vendored pin is part of the bundle." >&2; exit 1; }
MHVTL_PIN="$(basename "$MHVTL_TAR" .tar.gz)"

# The bundle distributes a statically linked binary; the Apache/BSD/MIT
# notice texts for everything linked into it must travel with it
# (third_party/licenses/, committed — regenerate per its README when
# go.mod or a runtime web dependency changes).
if [[ "$(git ls-files third_party/licenses/go third_party/licenses/web | wc -l)" -lt 10 ]]; then
  echo "[x] third_party/licenses/ missing or empty in HEAD — binary notice texts are part of the bundle (see third_party/licenses/README.md)." >&2
  exit 1
fi

# One build timestamp, reused for the VERSION `built:` line, the binary's
# embedded buildDate, AND every tar entry's mtime — the updater orders
# releases by this monotonic value — so the first two
# MUST match; normalizing mtimes to it keeps the artifact free of
# build-machine timing detail.
BUILD_EPOCH="$(date -u +%s)"
BUILD_TS="$(date -u -d "@$BUILD_EPOCH" +%Y-%m-%dT%H:%M:%SZ)"

# Signing is mandatory: no accidental unsigned releases.
: "${OPENVTL_SIGNING_KEY:?set OPENVTL_SIGNING_KEY to the offline vendor key (keys/openvtl-signing.key) — refusing to cut an unsigned release}"
echo "[*] verifying install.sh carries the committed signing public key"
(cd openvtld && go run ./cmd/release-tool check-installer ../packaging/install.sh)

echo "[*] web UI build"
(cd openvtld/web && npm run build >/dev/null)
rm -rf openvtld/internal/api/dist
cp -r openvtld/web/dist openvtld/internal/api/dist

echo "[*] openvtld linux/amd64 build (version $VER)"
# -trimpath: no build-machine filesystem paths in the binary (GOROOT,
# module cache, checkout dir all end up in the function table otherwise).
# -buildvcs=false: no VCS metadata either — version/buildDate are stamped
# explicitly via ldflags, so the binary carries product identity only.
(cd openvtld && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
  go build -trimpath -buildvcs=false \
  -ldflags "-X main.version=$VER -X main.buildDate=$BUILD_TS" -o bin/openvtld ./cmd/openvtld)

STAGE="releases/openvtl-$VER"
echo "[*] staging $STAGE"
rm -rf "$STAGE"
mkdir -p "$STAGE/bin" "$STAGE/repo"
git archive HEAD | tar -x -C "$STAGE/repo"

# Normalize the two dotfiles to their minimal public rules — the dev
# tree's versions carry local tooling configuration that has no business
# in a bundle, so the shipped copies are written here explicitly:
# line-ending/binary rules and build-output ignores only.
cat > "$STAGE/repo/.gitattributes" <<'EOF'
# Everything deployable targets Linux — keep LF everywhere, on every platform.
* text=auto eol=lf
*.pdf binary
*.pcap binary
EOF
cat > "$STAGE/repo/.gitignore" <<'EOF'
# Editor / OS noise
*.swp
*~
.DS_Store
Thumbs.db

# Build output
openvtld/bin/
**/node_modules/
openvtld/web/dist/

# Release bundles — assembled by scripts/make-release.sh
releases/
EOF

# Vendor pre-release checks (lint, packaging consistency). The script is
# part of the vendor build environment, not the public tree; a checkout
# without it (which also has no signing key) skips this step.
if [[ -f scripts/pre-release-checks.sh ]]; then
  bash scripts/pre-release-checks.sh "$STAGE/repo"
else
  echo "[!] vendor pre-release checks (lint, packaging consistency) — script is not part of the public tree; skipping." >&2
fi

cp openvtld/bin/openvtld "$STAGE/bin/openvtld"
# Staging on a non-POSIX filesystem can drop the exec bit — set it
# explicitly so the installer's version probe works straight from the
# unpacked bundle (install.sh re-asserts it as well).
chmod 755 "$STAGE/bin/openvtld"

cat > "$STAGE/VERSION" <<EOF
openvtl $VER
built: $BUILD_TS
openvtld: $VER (linux/amd64, CGO off, web UI embedded)
mhvtl: $MHVTL_PIN (vendored source; patched at install by repo/packaging/mhvtl/apply-patches.sh)
installer: repo/packaging/install.sh
EOF

echo "[*] checksums"
(cd "$STAGE" && find . -type f ! -name SHA256SUMS ! -name SHA256SUMS.sig -print0 | LC_ALL=C sort -z | xargs -0 sha256sum > SHA256SUMS)

# Detached Ed25519 signature over SHA256SUMS. SHA256SUMS covers
# every file, so one signature protects the whole bundle transitively. The sig
# travels inside the bundle; install.sh and `openvtld update` both verify it
# against the committed public key.
echo "[*] signing SHA256SUMS"
(cd openvtld && go run ./cmd/release-tool sign "$REPO/$STAGE/SHA256SUMS")
[[ -f "$STAGE/SHA256SUMS.sig" ]] || { echo "[x] signing produced no SHA256SUMS.sig" >&2; exit 1; }

# Anonymized, deterministic tar: no local owner/group names or uids, no
# build-machine timestamps, stable entry order, no gzip timestamp — the
# artifact describes the product, not the machine that built it.
TARBALL="releases/openvtl-$VER.tar.gz"
tar -C releases --sort=name --owner=0 --group=0 --numeric-owner \
  --mtime="@$BUILD_EPOCH" -cf - "openvtl-$VER" | gzip -n > "$TARBALL"
rm -rf "$STAGE"

echo "[+] $TARBALL ($(du -h "$TARBALL" | cut -f1))"
echo "    install: unpack on the VM, then: sudo openvtl-$VER/repo/packaging/install.sh"
