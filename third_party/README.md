# third_party — vendored sources

## mhvtl-8e79aa8.tar.gz

Pristine mhVTL source at the pinned commit `8e79aa8` ("Remove duplicate
clean target from usr/Makefile") — the pinned commit OpenVTL builds and
patches, produced with `git archive` (upstream:
https://github.com/markh794/mhvtl, GPLv2). Vendored so installs are
offline-capable and immune to upstream availability or history rewrites
(build/packaging details: packaging/dkms/README.md).

sha256: d19bcc9584391dbab6836088f81107ff6bd3dc131e6fe025d87e64b8f3f4a8de

The installer unpacks this, applies `packaging/mhvtl/apply-patches.sh`
(Patches 1–16 must all report [+]/[=]) and
`packaging/mhvtl/apply-drive-identity.sh`, then builds the DKMS module
(packaging/dkms/) and the userspace. NEVER pre-patch this tarball: the
patch scripts are the audit trail.

Updating the pin = replace the tarball + this hash + the VERSION in
scripts/make-release.sh, then re-verify the full patch stack and the
G1 DKMS gate before shipping.
