#!/usr/bin/env bash
#
# OpenVTL — single idempotent installer.
#
#   Fresh Debian 13 VM  →  working VTL platform in one run.
#
# The installer delivers the PLATFORM only: patched mhVTL (DKMS kernel
# module + userspace), the openvtld control plane, and systemd plumbing.
# Everything site-specific — storage pools, libraries, initiator ACLs,
# S3 remotes, TLS, the admin user — is configured afterwards in the
# product's own first-run UI (https://<vm>:8443). The installer asks no
# questions.
#
# Run from an unpacked release bundle (see scripts/make-release.sh):
#     sudo openvtl-<ver>/repo/packaging/install.sh
# or from a git checkout for development (binary from openvtld/bin/).
#
# Usage:
#     install.sh                 all phases (prep → mhvtl → openvtld → verify)
#     install.sh --phase NAME    one phase (prep | mhvtl | openvtld | verify)
#     install.sh --verify        acceptance checks only (exit != 0 on failure)
#
# Idempotent: a second full run is all no-ops ([=] lines). Re-running on
# a configured box never touches wizard-owned state (/etc/mhvtl configs,
# pools, the openvtld DB).
#
set -euo pipefail

# A `su` (non-login) root shell omits /usr/sbin on Debian.
export PATH="/usr/sbin:/sbin:$PATH"
export DEBIAN_FRONTEND=noninteractive

log()  { printf '\033[1;32m[+]\033[0m %s\n' "$*"; }
skip() { printf '\033[1;36m[=]\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m[!]\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31m[x]\033[0m %s\n' "$*" >&2; exit 1; }

PKG_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"   # …/repo/packaging
REPO="$(cd "$PKG_DIR/.." && pwd)"                         # …/repo
BUNDLE_ROOT="$(cd "$REPO/.." && pwd)"                     # …/openvtl-<ver> (bundle) or dev-checkout parent

# Release-signing public key (Ed25519, raw 32 bytes, base64). SINGLE SOURCE OF
# TRUTH: openvtld/internal/release/pubkey.b64 — make-release.sh fails the cut if
# this literal drifts from it. Rotating the signing key means shipping a new
# installer + binary carrying the new key.
RELEASE_PUBKEY_B64='WdL14ggTOB8OrxHns+ZWGrHn9hThBvJTst5Y8FigZQ0='

# openvtld binary: release bundle layout first, dev checkout second.
OPENVTLD_BIN=""
for c in "$REPO/../bin/openvtld" "$REPO/openvtld/bin/openvtld"; do
  [[ -f "$c" ]] && { OPENVTLD_BIN="$(cd "$(dirname "$c")" && pwd)/openvtld"; break; }
done

MHVTL_TARBALL="$(ls "$REPO"/third_party/mhvtl-*.tar.gz 2>/dev/null | head -1 || true)"
MHVTL_PIN="$(basename "${MHVTL_TARBALL:-mhvtl-unknown.tar.gz}" .tar.gz | sed 's/^mhvtl-//')"
MHVTL_SRC="/usr/src/openvtl-mhvtl"

# ZFS (zfsutils-linux, zfs-dkms) live in Debian's `contrib` component; a
# minimal netinst typically enables only `main`. Enable contrib idempotently
# on both the classic sources.list and any deb822 .sources file, honoring
# whatever mirror the site already uses.
ensure_contrib() {
  local changed=0
  if [[ -f /etc/apt/sources.list ]] \
     && grep -qE '^[[:space:]]*deb(-src)?[[:space:]].* main' /etc/apt/sources.list \
     && ! grep -qE '^[[:space:]]*deb[[:space:]].* contrib' /etc/apt/sources.list; then
    sed -i -E '/^[[:space:]]*deb(-src)?[[:space:]]/{/ contrib/!s/ main/ main contrib/}' /etc/apt/sources.list
    changed=1
  fi
  local f
  for f in /etc/apt/sources.list.d/*.sources; do
    [[ -e "$f" ]] || continue
    if grep -qE '^Components:' "$f" && ! grep -qE '^Components:.*contrib' "$f"; then
      sed -i -E 's/^(Components:.*)$/\1 contrib/' "$f"
      changed=1
    fi
  done
  if [[ "$changed" == 1 ]]; then
    log "enabled Debian 'contrib' (ZFS packages)"
    apt-get update -y -qq
  else
    skip "Debian 'contrib' already enabled"
  fi
}

# QLogic FC HBA detection: any PCI device bound to the qla2xxx driver. On an
# OpenVTL appliance that HBA IS the VTL target fabric (IBM i → FC → VTL), so it
# belongs in target mode (below). Boxes with no FC HBA match none.
fc_hba_present() {
  local d
  for d in /sys/bus/pci/drivers/qla2xxx/*:*; do
    [[ -d "$d" ]] && return 0
  done
  return 1
}

# FC target mode. qla2xxx defaults to INITIATOR mode, in which LIO cannot bind a
# target to the port — targetcli fails "Could not create Target in configFS" and
# the host sees zero targets (field lesson). A VTL box needs
# the HBA target-only. qlini_mode is a module load-time parameter, so this takes
# effect on the NEXT boot. Idempotent; never clobbers an operator-authored file.
configure_fc_target_mode() {
  if ! fc_hba_present; then
    skip "no QLogic FC HBA detected — FC target-mode config not needed (no-fabric box)"
    return 0
  fi
  local conf=/etc/modprobe.d/qla2xxx.conf
  if [[ -f "$conf" ]]; then
    if grep -qE '^[[:space:]]*options[[:space:]]+qla2xxx[[:space:]].*qlini_mode=disabled' "$conf"; then
      skip "qla2xxx target mode already configured ($conf)"
    else
      warn "$conf exists without qlini_mode=disabled — leaving it untouched. A VTL target HBA needs: options qla2xxx qlini_mode=disabled"
    fi
  else
    echo 'options qla2xxx qlini_mode=disabled' > "$conf"
    # Belt-and-suspenders for boxes that load qla2xxx from the initramfs
    # (FC-boot); harmless otherwise.
    update-initramfs -u >/dev/null 2>&1 || true
    log "FC target mode configured ($conf) — qla2xxx will load target-only"
  fi
  # The running module keeps its boot-time mode, so a FRESH FC install won't
  # present targets until a reboot.
  if [[ "$(cat /sys/module/qla2xxx/parameters/qlini_mode 2>/dev/null)" != "disabled" ]]; then
    warn "FC target mode is NOT active yet — REBOOT REQUIRED before openvtld can present FC targets (qla2xxx is currently in initiator mode)."
  else
    log "FC target mode active (qla2xxx qlini_mode=disabled)"
  fi
}

# qla2xxx boots on the HBA's flash-resident firmware, but ANY ISP reset (target
# create/delete, error recovery) re-loads firmware from /lib/firmware — when the
# file is absent the port is dead until reboot ("Failed to load firmware image",
# alloc_iocbs failures; field lesson — a fabric rebuild wedged and took every
# port Offline/Linkdown). firmware-qlogic (non-free-
# firmware, in the default Debian 13 sources) ships the whole ql2xxx family.
configure_fc_firmware() {
  if ! fc_hba_present; then
    skip "no QLogic FC HBA detected — firmware-qlogic not needed"
    return 0
  fi
  if [[ -e /lib/firmware/ql2500_fw.bin || -e /lib/firmware/ql2400_fw.bin ]]; then
    skip "qla2xxx firmware present in /lib/firmware"
  elif apt-get install -y firmware-qlogic; then
    log "firmware-qlogic installed — FC ports can now survive ISP resets"
  else
    warn "firmware-qlogic could not be installed (needs the non-free-firmware component) — FC ports will NOT survive a reset until it is."
  fi
}

# openvtld owns the LIO fabric and rebuilds it from intent at every boot (the
# mhVTL daemon epoch is stale by definition after a reboot). The distro's boot
# restore of saveconfig.json is therefore not just redundant — it is HARMFUL: it
# races openvtld's boot rebuild (targetcli exit 255) and resurrects pscsi
# backstores bound to PRE-reboot sg paths, so hosts briefly see the library
# under a WRONG identity (field lesson).
disable_lio_boot_restore() {
  if [[ "$(systemctl is-enabled rtslib-fb-targetctl.service 2>/dev/null)" == "enabled" ]]; then
    systemctl disable rtslib-fb-targetctl.service >/dev/null 2>&1 || true
    log "rtslib-fb-targetctl boot restore disabled — openvtld owns the fabric"
  else
    skip "LIO boot restore already disabled or absent"
  fi
}

# ------------------------------------------------------- signature ----------

# Verify the release bundle is authentic BEFORE consuming any of its files
# (v0.8, design §12.1): check the vendor Ed25519 signature over SHA256SUMS with
# the public key baked into THIS script — trusting nothing from the bundle — then
# every file against SHA256SUMS. openssl does the crypto (OpenSSL 3.x on Debian
# 13); the key is built into a PEM from RELEASE_PUBKEY_B64 (the SPKI DER prefix
# is a whole number of bytes, so prepending its base64 to the key's base64 is a
# valid concatenation). A dev checkout has no SHA256SUMS and is skipped.
verify_bundle_signature() {
  local sums="$BUNDLE_ROOT/SHA256SUMS" sig="$BUNDLE_ROOT/SHA256SUMS.sig"
  if [[ ! -f "$sums" ]]; then
    warn "no SHA256SUMS at $BUNDLE_ROOT (dev checkout?) — skipping release signature verification"
    return 0
  fi
  [[ -f "$sig" ]] || die "SHA256SUMS present but SHA256SUMS.sig missing — refusing an unsigned bundle."
  command -v openssl >/dev/null 2>&1 || die "openssl is required to verify the release signature (apt-get install openssl), then re-run."
  local tmp; tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' RETURN
  printf '%s\n%s\n%s\n' '-----BEGIN PUBLIC KEY-----' "MCowBQYDK2VwAyEA${RELEASE_PUBKEY_B64}" '-----END PUBLIC KEY-----' > "$tmp/pub.pem"
  base64 -d < "$sig" > "$tmp/sig.raw" 2>/dev/null || die "SHA256SUMS.sig is not valid base64 — corrupt bundle."
  openssl pkeyutl -verify -pubin -inkey "$tmp/pub.pem" -rawin -in "$sums" -sigfile "$tmp/sig.raw" >/dev/null 2>&1 \
    || die "release signature INVALID — this bundle is not authentic (tampered, corrupt, or not signed by OpenVTL; needs OpenSSL 3.x). Refusing to install."
  ( cd "$BUNDLE_ROOT" && LC_ALL=C sha256sum -c --quiet SHA256SUMS ) >/dev/null 2>&1 \
    || die "bundle file checksums do not match SHA256SUMS — tampered or corrupt. Refusing to install."
  log "release signature verified (Ed25519) + file checksums OK"
}

# ---------------------------------------------------------------- prep ------

phase_prep() {
  log "phase: prep (OS packages + platform checks)"

  [[ -r /etc/os-release ]] || die "/etc/os-release missing."
  # shellcheck disable=SC1091
  . /etc/os-release
  case "${ID}:${VERSION_ID:-}" in
    debian:13*) log "Debian ${VERSION_ID} (trixie)." ;;
    debian:*)   warn "Debian ${VERSION_ID} — untested; OpenVTL targets Debian 13 (ZFS + mhVTL are DKMS-built against the running kernel; contrib must carry a compatible zfs-dkms)." ;;
    *)          die "Unsupported OS '${PRETTY_NAME:-unknown}'. OpenVTL targets Debian 13." ;;
  esac

  # Secure Boot must be off (docs/reference-vm-spec.md): DKMS-signed
  # modules are rejected until a MOK is enrolled, which breaks the
  # unattended kernel-update contract.
  if command -v mokutil >/dev/null 2>&1 && mokutil --sb-state 2>/dev/null | grep -q "enabled"; then
    die "Secure Boot is ENABLED — the mhVTL module will not load. Disable it in the VM firmware (reference VM spec) or enroll the DKMS MOK (packaging/dkms/README.md, Option B), then re-run."
  fi

  # mhVTL and ZFS are both out-of-tree — DKMS builds them against the
  # RUNNING kernel, so headers must be in place before anything triggers a
  # DKMS build. Install the toolchain + headers first and verify, THEN pull
  # zfs-dkms (which builds in its postinst).
  # Match the RUNNING kernel's flavour. Cloud images (IBM Cloud VPC, EC2, GCE)
  # boot linux-image-cloud-amd64; installing the generic meta there would add a
  # second kernel and leave the running one header-less until a reboot into a
  # different flavour — a needless kernel switch on a remote appliance.
  local kflavor=amd64
  case "$(uname -r)" in
    *-cloud-amd64) kflavor=cloud-amd64 ;;
    *-rt-amd64)    kflavor=rt-amd64 ;;
  esac
  local toolchain=(
    # toolchain + DKMS; the meta-packages keep kernel and headers advancing
    # TOGETHER — the one known way to silently lose DKMS auto-rebuild is a
    # versioned headers pin going stale (packaging/dkms/README.md).
    dkms build-essential zlib1g-dev "linux-image-${kflavor}" "linux-headers-${kflavor}"
  )
  log "apt-get update + toolchain: ${toolchain[*]}"
  apt-get update -y -qq
  apt-get install -y -qq "${toolchain[@]}"

  local krel; krel="$(uname -r)"
  [[ -d "/lib/modules/${krel}/build" ]] \
    || die "No /lib/modules/${krel}/build — the running kernel has no headers (reboot into the kernel apt just installed, then re-run)."
  log "kernel ${krel}: headers present"

  # ZFS is the storage plane (one system zpool, dedup vdev + per-pool
  # datasets; internal/storage/manager.go). Debian ships it as source only:
  # zfs-dkms builds the module (contrib), zfsutils-linux is the zpool/zfs
  # userspace. This is the SECOND out-of-tree DKMS module besides mhVTL; the
  # headers meta-package keeps both rebuilding across kernel updates.
  ensure_contrib

  local pkgs=(
    # storage plane (ZFS: pool wizard drives zpool/zfs; module via DKMS)
    zfsutils-linux zfs-dkms
    # SCSI / tape userspace
    sg3-utils lsscsi mtx mt-st
    # LIO management (qla2xxx FC fabric)
    targetcli-fb
    # diagnostics: the coredump trap that caught the v0.6 pair-crash
    systemd-coredump
    # verify-phase probes
    curl ca-certificates
  )
  log "apt-get install: ${pkgs[*]}"
  apt-get install -y -qq "${pkgs[@]}"

  # Load the freshly-built module now and confirm the DKMS build actually
  # produced a loadable module (a newer kernel than packaged ZFS supports
  # would fail here rather than mysteriously at first pool create).
  modprobe zfs 2>/dev/null || true
  [[ -d /sys/module/zfs ]] || die "zfs kernel module not loaded — zfs-dkms build likely failed (check: dkms status zfs; /var/lib/dkms/zfs/*/build/make.log). Kernel ${krel} may be newer than packaged ZFS supports."
  log "zfs: DKMS module built + loaded"

  # Pool datasets mount at boot via fstab x-systemd.requires=zfs-import.target
  # (manager.go); zfs-import-cache imports the system zpool from the cache.
  systemctl enable zfs-import-cache.service zfs.target >/dev/null 2>&1 || true

  modinfo tcm_qla2xxx >/dev/null 2>&1 && log "tcm_qla2xxx: in-tree" || warn "tcm_qla2xxx not found (unexpected on Debian 13)"

  # Put the FC HBA in target mode (needed before openvtld can present targets).
  configure_fc_target_mode
  # FC firmware on disk (ISP resets brick ports without it).
  configure_fc_firmware
  # openvtld rebuilds the fabric at boot; the distro restore races it.
  disable_lio_boot_restore
}

# --------------------------------------------------------------- mhvtl ------

phase_mhvtl() {
  log "phase: mhvtl (pinned source ${MHVTL_PIN}: patches → DKMS module → userspace)"
  [[ -n "$MHVTL_TARBALL" ]] || die "vendored mhVTL tarball missing under $REPO/third_party/."

  # Unpack the pristine pin (marker-gated; the patch scripts are
  # idempotent so re-patching an unpacked tree is also a no-op).
  if [[ -f "$MHVTL_SRC/.openvtl-pin" && "$(cat "$MHVTL_SRC/.openvtl-pin")" == "$MHVTL_PIN" ]]; then
    skip "mhVTL source ${MHVTL_PIN} already unpacked at $MHVTL_SRC"
  else
    log "unpacking $MHVTL_TARBALL → $MHVTL_SRC"
    rm -rf "$MHVTL_SRC"
    mkdir -p "$MHVTL_SRC"
    tar -xzf "$MHVTL_TARBALL" -C "$MHVTL_SRC" --strip-components=1
    echo "$MHVTL_PIN" > "$MHVTL_SRC/.openvtl-pin"
  fi

  # Emulation patches 1–16 + drive identity. Both scripts self-verify
  # and abort loudly; a failed patch means the pin and the patch set
  # have diverged and the install MUST stop.
  "$PKG_DIR/mhvtl/apply-patches.sh" "$MHVTL_SRC"
  "$PKG_DIR/mhvtl/apply-drive-identity.sh" "$MHVTL_SRC"

  # Kernel module via DKMS (kernel-update survival: G1 test). Capture
  # instead of `| grep -q` — pipefail + early-exit grep SIGPIPEs the
  # left side (the install-dkms.sh lesson).
  if [[ "$(dkms status "mhvtl/${MHVTL_PIN}" 2>/dev/null)" == *installed* && -d /sys/module/mhvtl ]]; then
    skip "DKMS mhvtl/${MHVTL_PIN} already installed and loaded"
  else
    "$PKG_DIR/dkms/stage-sources.sh" --src "$MHVTL_SRC" --version "$MHVTL_PIN"
    "$PKG_DIR/dkms/install-dkms.sh" --version "$MHVTL_PIN"
  fi

  # Userspace. `make -C usr install` dies at a trailing restorecon on
  # Debian AFTER installing binaries+libs but BEFORE its ldconfig
  # (v0.6 field lesson) — tolerate, then ldconfig + verify explicitly.
  local first_install=1
  [[ -f /etc/mhvtl/device.conf ]] && first_install=0
  log "building mhVTL userspace"
  make -s -C "$MHVTL_SRC/usr" >/dev/null
  make -s -C "$MHVTL_SRC/usr" install >/dev/null 2>&1 || true
  make -s -i -C "$MHVTL_SRC/etc" install >/dev/null 2>&1 || true
  make -s -i -C "$MHVTL_SRC/scripts" install >/dev/null 2>&1 || true
  ldconfig
  local b
  for b in vtltape vtllibrary vtlcmd mktape dump_tape; do
    [[ -x "/usr/bin/$b" ]] || die "userspace install incomplete: /usr/bin/$b missing"
  done
  ldconfig -p | grep -q libvtlscsi || die "libvtlscsi not in the linker cache after ldconfig"
  log "userspace installed (vtltape/vtllibrary/vtlcmd/mktape + libs)"

  # First install only: the stock `make -C etc install` generated a
  # DEMO configuration (two libraries, eight drives). openvtld's
  # library wizard owns /etc/mhvtl — hand it an EMPTY baseline. On
  # re-runs the make file-targets skip existing configs and we never
  # touch them.
  if [[ "$first_install" == 1 ]]; then
    log "first install: replacing stock demo config with an empty wizard-owned baseline"
    rm -f /etc/mhvtl/library_contents.*
    printf 'VERSION: 5\n\n# Libraries are created by the OpenVTL UI (Library → New library).\n' > /etc/mhvtl/device.conf
    sed -i 's|^MHVTL_HOME_PATH=.*|MHVTL_HOME_PATH=/opt/mhvtl|' /etc/mhvtl/mhvtl.conf 2>/dev/null || true
  else
    skip "/etc/mhvtl exists — wizard-owned config left untouched"
  fi

  # openvtld's drive-activity telemetry (the dashboard drive cards' write/read
  # pulse + throughput) parses vtltape's VERBOSE=3 "firehose" journal — the
  # ssc_write_6/ssc_read_6 lines in internal/inventory/journal.go. Stock
  # mhvtl.conf ships VERBOSE=1, which starves it and the cards show no activity
  # even during a live save (field lesson). Enforce VERBOSE=3
  # + DAEMON_DEBUG=-d idempotently (this is global mhVTL config, not per-library).
  # Takes effect on the next vtltape start (data-plane restart / reboot).
  local mconf=/etc/mhvtl/mhvtl.conf verbose_changed=0
  if [[ -f "$mconf" ]]; then
    if ! grep -qxE 'VERBOSE=3' "$mconf"; then
      sed -i -E 's/^#?[[:space:]]*VERBOSE=.*/VERBOSE=3/' "$mconf"
      grep -qxE 'VERBOSE=3' "$mconf" || printf 'VERBOSE=3\n' >> "$mconf"
      verbose_changed=1
    fi
    if ! grep -qxE 'DAEMON_DEBUG=-d' "$mconf"; then
      sed -i -E 's/^#?[[:space:]]*DAEMON_DEBUG=.*/DAEMON_DEBUG=-d/' "$mconf"
      grep -qxE 'DAEMON_DEBUG=-d' "$mconf" || printf 'DAEMON_DEBUG=-d\n' >> "$mconf"
      verbose_changed=1
    fi
    if [[ "$verbose_changed" == 1 ]]; then
      warn "mhvtl.conf set to VERBOSE=3 + DAEMON_DEBUG=-d (openvtld drive-activity firehose) — restart the data plane (or reboot) for it to take effect."
    else
      skip "mhvtl.conf already VERBOSE=3 (drive-activity firehose)"
    fi
  fi

  # Media user + home (units run as root; media files are chowned vtl).
  getent group vtl >/dev/null || groupadd -r vtl
  getent passwd vtl >/dev/null || useradd -r -g vtl -M -d /opt/mhvtl -s /usr/sbin/nologin vtl
  mkdir -p /opt/mhvtl
  chown vtl:vtl /opt/mhvtl

  # Coredump trap (how the v0.6 pair-crash was caught) — permanent.
  local d
  for d in vtltape@.service.d vtllibrary@.service.d; do
    mkdir -p "/etc/systemd/system/$d"
    install -m644 "$PKG_DIR/systemd/vtl-coredump-dropin.conf" "/etc/systemd/system/$d/openvtl-coredump.conf"
  done

  systemctl daemon-reload
  systemctl enable mhvtl.target >/dev/null 2>&1 || warn "mhvtl.target enable failed — check the etc install"
  log "mhvtl.target enabled (no daemons until a library is created in the UI)"
}

# ------------------------------------------------------------- openvtld -----

phase_openvtld() {
  log "phase: openvtld (control plane)"
  [[ -n "$OPENVTLD_BIN" ]] || die "openvtld binary not found (bundle bin/ or openvtld/bin/) — run scripts/make-release.sh first."

  local cur="" new
  chmod +x "$OPENVTLD_BIN" 2>/dev/null || true # bundle tars may lack the exec bit
  new="$("$OPENVTLD_BIN" -version 2>/dev/null || true)"
  [[ -x /usr/local/bin/openvtld ]] && cur="$(/usr/local/bin/openvtld -version 2>/dev/null || true)"
  if [[ -n "$cur" && "$cur" == "$new" ]]; then
    skip "openvtld $cur already installed"
  else
    install -m755 "$OPENVTLD_BIN" /usr/local/bin/openvtld.new
    mv /usr/local/bin/openvtld.new /usr/local/bin/openvtld
    log "openvtld ${new:-?} installed"
  fi

  # Update auto-rollback watchdog (v0.8): openvtld.service runs it as
  # ExecStartPre, so it must exist before the unit starts.
  install -D -m755 "$PKG_DIR/systemd/openvtld-update-watchdog.sh" /usr/local/lib/openvtld/update-watchdog.sh

  install -m644 "$PKG_DIR/systemd/openvtld.service" /etc/systemd/system/openvtld.service
  # The one sanctioned flag-override point (design §4): sites edit this
  # drop-in, never the unit.
  mkdir -p /etc/systemd/system/openvtld.service.d
  if [[ ! -f /etc/systemd/system/openvtld.service.d/site.conf ]]; then
    cat > /etc/systemd/system/openvtld.service.d/site.conf <<'EOF'
# OpenVTL site overrides. To change flags, uncomment BOTH lines (the
# empty ExecStart= clears the unit's default) and edit the second:
#
#   disable the plaintext :8080 listener (metrics/health) — HTTPS-only site:
# [Service]
# ExecStart=
# ExecStart=/usr/local/bin/openvtld -listen ""
EOF
  fi
  systemctl daemon-reload
  systemctl enable openvtld >/dev/null 2>&1
  systemctl restart openvtld
  log "openvtld enabled + started"
}

# -------------------------------------------------------------- verify ------

VFAIL=0
chk() { # chk <description> <command...>
  local desc="$1"; shift
  if "$@" >/dev/null 2>&1; then
    log "verify: $desc"
  else
    printf '\033[1;31m[x]\033[0m verify FAILED: %s\n' "$desc" >&2
    VFAIL=1
  fi
}

phase_verify() {
  log "phase: verify (acceptance gate)"
  local krel; krel="$(uname -r)"

  chk "Secure Boot off (or no mokutil)" bash -c '! command -v mokutil >/dev/null || ! mokutil --sb-state 2>/dev/null | grep -q enabled'
  chk "DKMS mhvtl/${MHVTL_PIN} installed" bash -c "[[ \"\$(dkms status 'mhvtl/${MHVTL_PIN}' 2>/dev/null)\" == *installed* ]]"
  chk "mhvtl module loaded" test -d /sys/module/mhvtl
  chk "mhvtl vermagic matches ${krel}" bash -c "[[ \"\$(modinfo -F vermagic mhvtl 2>/dev/null)\" == ${krel}\ * ]]"
  # apply-patches.sh is content-matched + self-verifying: on a patched
  # tree it exits 0 with all [=] lines; any regression exits non-zero.
  chk "mhVTL patches verified (idempotent re-run green)" "$PKG_DIR/mhvtl/apply-patches.sh" "$MHVTL_SRC"
  chk "userspace binaries present" bash -c 'for b in vtltape vtllibrary vtlcmd mktape; do [ -x /usr/bin/$b ] || exit 1; done'
  chk "libvtlscsi in linker cache" bash -c 'ldconfig -p | grep -q libvtlscsi'
  chk "mhvtl.target enabled" bash -c 'systemctl is-enabled mhvtl.target | grep -q enabled'
  chk "coredump trap drop-ins installed" test -f /etc/systemd/system/vtltape@.service.d/openvtl-coredump.conf
  chk "openvtld active" systemctl is-active openvtld
  chk "openvtld -version answers" /usr/local/bin/openvtld -version
  chk "HTTPS API answers (:8443 healthz)" curl -skf --max-time 10 https://localhost:8443/healthz
  chk "targetcli present (LIO)" bash -c 'command -v targetcli'
  chk "zfs userspace present (pool plane)" bash -c 'command -v zpool && command -v zfs'
  chk "zfs DKMS module installed" bash -c '[[ "$(dkms status zfs 2>/dev/null)" == *installed* ]]'
  chk "zfs module loaded" test -d /sys/module/zfs

  # FC target mode — only on boxes with a QLogic FC HBA. A WARN, not a gate: a
  # fresh FC install writes the config in prep but it needs a reboot to activate,
  # and no-HBA boxes never need it at all.
  if fc_hba_present; then
    if [[ "$(cat /sys/module/qla2xxx/parameters/qlini_mode 2>/dev/null)" == "disabled" ]]; then
      log "verify: FC target mode active (qla2xxx qlini_mode=disabled)"
    else
      warn "verify: FC HBA present but qla2xxx is in initiator mode — REBOOT required for openvtld to present FC targets."
    fi
  fi

  # FC firmware on disk: without it any ISP reset bricks the port until reboot
  # (WARN, not a gate — the fabric runs on flash firmware until then).
  if fc_hba_present; then
    if [[ -e /lib/firmware/ql2500_fw.bin || -e /lib/firmware/ql2400_fw.bin ]]; then
      log "verify: qla2xxx firmware present (/lib/firmware)"
    else
      warn "verify: qla2xxx firmware MISSING — install firmware-qlogic; FC ports will not survive a reset (target rebuilds, error recovery) until it is."
    fi
  fi

  # The distro LIO boot restore races openvtld's boot rebuild and resurrects
  # stale pscsi identities — it must stay disabled (openvtld owns the fabric).
  if [[ "$(systemctl is-enabled rtslib-fb-targetctl.service 2>/dev/null)" == "enabled" ]]; then
    warn "verify: rtslib-fb-targetctl boot restore is ENABLED — it races openvtld's boot rebuild and can present the library under a stale identity; re-run install.sh to disable it."
  else
    log "verify: LIO boot restore disabled (openvtld owns the fabric)"
  fi

  # Drive-activity telemetry depends on mhvtl.conf VERBOSE=3 (the vtltape journal
  # firehose openvtld parses). A WARN, not a gate — saves work regardless; only
  # the dashboard drive cards go blind without it.
  if grep -qxE 'VERBOSE=3' /etc/mhvtl/mhvtl.conf 2>/dev/null; then
    log "verify: mhvtl.conf VERBOSE=3 (drive-activity firehose)"
  else
    warn "verify: mhvtl.conf is not VERBOSE=3 — dashboard drive cards show no activity; re-run install.sh and restart the data plane."
  fi

  if [[ "$VFAIL" == 0 ]]; then
    local ip; ip="$(hostname -I 2>/dev/null | awk '{print $1}')"
    cat <<EOF

$(log "OpenVTL platform verified.")

  Next (commissioning, in the UI — https://${ip:-<vm-ip>}:8443):
    1. First-run: create the admin user.
    2. Storage → designate the dedupe device, create a pool.
    3. Library → New library wizard → Apply (operator window).
    4. Targets → initiator ACLs (FC auto-detects the HBA WWN).
    5. S3 → add a remote (optional — the dashboard nags until offsite exists).
  Kernel-update survival: tests/g1-dkms-kernel-update.sh pre → reboot → verify.
EOF
    if fc_hba_present && [[ "$(cat /sys/module/qla2xxx/parameters/qlini_mode 2>/dev/null)" != "disabled" ]]; then
      warn "FC box: REBOOT NOW to activate qla2xxx target mode — the host will not see the VTL targets until you do."
    fi
  else
    die "verification FAILED — see [x] lines above."
  fi
}

# ---------------------------------------------------------------- main ------

PHASE="all"
case "${1:-}" in
  "")            ;;
  --verify)      PHASE="verify" ;;
  --phase)       PHASE="${2:?--phase needs a name}" ;;
  -h|--help)     grep '^#' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
  *)             die "Unknown argument: $1 (see --help)" ;;
esac

[[ $EUID -eq 0 ]] || die "Run as root (sudo)."

# Authenticity gate for every phase that consumes bundle files (not the
# post-install acceptance check). A tampered file or bad signature stops here.
case "$PHASE" in
  all|prep|mhvtl|openvtld) verify_bundle_signature ;;
esac

case "$PHASE" in
  all)      phase_prep; phase_mhvtl; phase_openvtld; phase_verify ;;
  prep)     phase_prep ;;
  mhvtl)    phase_mhvtl ;;
  openvtld) phase_openvtld ;;
  verify)   phase_verify ;;
  *)        die "Unknown phase '$PHASE' (prep | mhvtl | openvtld | verify)" ;;
esac
