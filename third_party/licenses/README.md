# Bundled third-party license texts

These are the license texts (and, where upstream provides one, NOTICE
files) for every third-party component **statically linked into the
`openvtld` binary or bundled into its embedded web UI**. They travel with
every release bundle so that binary distribution satisfies the notice
conditions of the Apache-2.0, BSD, and MIT licenses involved.

- `go/` — Go module dependencies of `openvtld` (direct and transitive),
  extracted from the exact module versions in `openvtld/go.mod`/`go.sum`
  with `go-licenses save`, plus `modernc.org/mathutil` (added manually;
  its layout defeats the tool's detection).
- `web/` — runtime JavaScript bundled into the embedded UI (react,
  react-dom, scheduler — MIT). Build-time-only tooling (vite, tailwind,
  typescript, oxlint) is not distributed and is listed in
  [THIRD_PARTY_NOTICES.md](../../THIRD_PARTY_NOTICES.md) for reference
  only.

**When dependencies change** (any edit to `openvtld/go.mod` or a runtime
web dependency), regenerate before cutting a release:

```bash
cd openvtld && go run github.com/google/go-licenses@v1.6.0 save ./cmd/openvtld \
  --save_path=../third_party/licenses/go --force \
  --ignore github.com/openvtl/openvtld --ignore modernc.org/mathutil
```

then re-copy `modernc.org/mathutil/LICENSE` from the module cache and any
new runtime web-dependency licenses into `web/`. `scripts/make-release.sh`
refuses to cut a bundle if this directory is missing.

mhVTL's own license and the GPL-2.0 status of the OpenVTL mhVTL patches
are covered separately in [THIRD_PARTY_NOTICES.md](../../THIRD_PARTY_NOTICES.md);
the vendored mhVTL tarball carries its COPYING file.
