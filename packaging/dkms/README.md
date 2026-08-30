# mhVTL DKMS packaging

Registers the mhVTL pseudo-HBA kernel module with DKMS so it rebuilds
automatically on every kernel update — no manual module step after a
`linux-image` upgrade, ever.

## Files

| File | Purpose |
|------|---------|
| `dkms.conf.in` | DKMS config template; `@PKGVER@` substituted at staging time |
| `stage-sources.sh` | Copies the upstream `kernel/` tree (sources, headers, AND upstream's `Makefile`) into `/usr/src/mhvtl-<ver>/` and layers our `dkms.conf` on top |
| `install-dkms.sh` | `dkms add` / `build` / `install`, loads the module, verifies |

mhVTL is GPLv2. Its source is **vendored** as
`third_party/mhvtl-<pin>.tar.gz` (a pristine `git archive` of the pinned
upstream commit — see `third_party/README.md`); `packaging/install.sh`
unpacks it, applies the OpenVTL patch scripts, and feeds the tree to the
scripts here. Nothing is fetched from the network at install or build time.

The module is NOT a single source file: `mhvtl.c` builds together with
version-specific sources and backport handling that only **upstream's own
`kernel/Makefile`** wires up correctly, so DKMS builds with that Makefile
(DKMS still controls which kernel it builds against, keeping auto-rebuild
intact). A `PRE_BUILD` hook regenerates the kernel-dependent `config.h`
for whichever kernel DKMS is building.

## Procedure

`packaging/install.sh` runs all of this for you. By hand, against an
already-unpacked and patched source tree:

```bash
# 1. Stage kernel sources + DKMS glue into /usr/src/mhvtl-<ver>.
sudo packaging/dkms/stage-sources.sh --src /usr/src/openvtl-mhvtl --version "$MHVTL_PIN"

# 2. Register, build, install, load.
sudo packaging/dkms/install-dkms.sh --version "$MHVTL_PIN"

# 3. Build + install the mhVTL USERSPACE (daemons/tools) the normal way
#    (make / make install in the source tree) — that half is not DKMS-managed.
```

## G1 — auto-rebuild validation (the milestone gate)

```bash
sudo tests/g1-dkms-kernel-update.sh pre      # install newest kernel + matching headers
sudo reboot                                  # boot the new kernel
sudo tests/g1-dkms-kernel-update.sh verify   # assert mhvtl rebuilt + loaded
```

`verify` passes only if DKMS shows `mhvtl` **installed** for the new kernel and
the module loads with a matching `vermagic` — with no manual `dkms` step after
the reboot.

## Operational note — keep kernel headers tracking the kernel

DKMS autoinstall on a kernel update builds against `/lib/modules/<newkernel>/build`
(→ `/usr/src/linux-headers-<newkernel>`), which only exists if the matching
`linux-headers-<newkernel>` package is installed. Debian makes this easy: the
**`linux-headers-amd64` meta-package** depends on the headers for the current
kernel flavour, so whenever `linux-image-amd64` pulls a new kernel, APT pulls the
matching headers in the same transaction.

- Install the meta-packages once and let them track:
  `apt install linux-image-amd64 linux-headers-amd64`. Both advance together on
  `apt full-upgrade`, so the new kernel always has its headers — and Debian keeps
  the previous kernel + headers side-by-side until `apt autoremove`.
- Match the metas to the FLAVOUR the box actually boots. Cloud images (IBM Cloud
  VPC, EC2, GCE) run `linux-image-cloud-amd64`; there the pair is
  `linux-image-cloud-amd64` + `linux-headers-cloud-amd64`. Installing the generic
  `amd64` metas on such a box adds a second kernel and leaves the running one
  header-less until a reboot into the other flavour. `install.sh` derives the
  flavour from `uname -r` (amd64 / cloud-amd64 / rt-amd64).
- Don't pin to a single versioned `linux-headers-<ver>` and forget it; that's the
  one way to end up with a kernel that has no headers and a silently skipped DKMS
  rebuild.

On Debian, DKMS rebuilds registered modules from the kernel package's `postinst`
hooks (`/etc/kernel/postinst.d/dkms`) — there is **no `dkms.service` to enable**
and no `installonlypkgs` knob. The G1 `pre` phase installs the matching headers
explicitly, so the test itself is unaffected — this note is about steady-state
production updates.

## Secure Boot / module signing

DKMS signs the built module with a self-generated MOK (`/var/lib/dkms/mok.*`).
If the VM boots with **Secure Boot enabled**, the kernel rejects the module at
load time with `modprobe: ERROR: could not insert 'mhvtl': Required key not
available`, even though `dkms status` shows it `installed`. Check with
`mokutil --sb-state`.

This is a decision to make **on the VM template**, so all clones match:

- **Option A — Secure Boot off (recommended).** OpenVTL does not require
  Secure Boot. Disabling it (OVMF setup → Device Manager → Secure Boot
  Configuration → uncheck *Attempt Secure Boot*) lets DKMS-signed modules load
  with no further steps, preserving the "single install script, no manual
  intervention" goal.
- **Option B — Secure Boot on, enroll the MOK once.** More secure. Run
  `mokutil --import /var/lib/dkms/mok.pub`, set a one-time password, reboot, and
  complete enrollment at the MOK Manager screen. Do this **on the template**:
  clones inherit the enrolled key (in the efidisk) and `/var/lib/dkms/mok.key`,
  so subsequent kernel auto-rebuilds re-sign with the same key and load
  unattended. The one interactive blue-screen step per template build is
  unavoidable by design.
