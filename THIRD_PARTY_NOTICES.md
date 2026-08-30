# Third-party notices

OpenVTL's original code — the `openvtld` daemon, the embedded web UI, and the
packaging/installer scripts — is Copyright © 2026 Emerald Coast Technologies
LLC, d/b/a OpenVTL, and is licensed under the GNU Affero General Public
License v3.0 (see [LICENSE](LICENSE)), **with one carve-out**:

## mhVTL and the OpenVTL mhVTL patches (GPL-2.0)

- `third_party/mhvtl-*.tar.gz` is a pristine, vendored snapshot of
  [mhVTL](https://github.com/markh794/mhvtl) by Mark Harvey and contributors,
  licensed under the **GNU General Public License v2.0**. It keeps its own
  license; nothing in this repository relicenses it. The tarball is unpacked
  and built on the appliance at install time.
- The OpenVTL emulation patches applied to that source
  (`packaging/mhvtl/apply-patches.sh`, `packaging/mhvtl/apply-drive-identity.sh`)
  are derivative works of mhVTL and are therefore licensed under
  **GPL-2.0**, not AGPL-3.0.

## Go module dependencies (openvtld)

The release ships `openvtld` as a statically linked binary, so the notice
conditions of these licenses attach to the bundle itself. **The full
license texts (and upstream NOTICE files) for every statically linked Go
module — direct and transitive — and for the JavaScript bundled into the
embedded web UI ship in every release bundle under
[`third_party/licenses/`](third_party/licenses/README.md).**

Direct dependencies (see `openvtld/go.mod`):

| Module | License |
|---|---|
| github.com/klauspost/compress | Apache-2.0 / BSD-3-Clause (mixed, per upstream LICENSE) |
| github.com/minio/minio-go/v7 | Apache-2.0 |
| golang.org/x/crypto | BSD-3-Clause |
| golang.org/x/sys | BSD-3-Clause |
| modernc.org/sqlite | BSD-3-Clause (SQLite itself is public domain) |

Transitive modules are recorded in `openvtld/go.sum`; their license texts
are included under `third_party/licenses/go/`.

## Web UI dependencies

Runtime (bundled into the embedded UI): react, react-dom, scheduler — MIT
(texts under `third_party/licenses/web/`).

Build-time only: vite (MIT), @vitejs/plugin-react (MIT), tailwindcss and
@tailwindcss/vite (MIT), typescript (Apache-2.0), oxlint (MIT), @types/*
(MIT). Exact versions are recorded in `openvtld/web/package-lock.json`.
