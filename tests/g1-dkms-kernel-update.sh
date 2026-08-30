#!/usr/bin/env bash
#
# OpenVTL — G1 validation: mhVTL DKMS module survives a kernel update.
#
# Prove that installing a new kernel and rebooting results in the mhvtl
# module being AUTOMATICALLY rebuilt and loaded, with zero manual steps —
# the property that makes unattended kernel updates safe on an appliance.
#
# The test spans a reboot, so it runs in two phases:
#
#   sudo ./g1-dkms-kernel-update.sh pre      # install newest kernel + matching headers
#   reboot                                   # boot into the new kernel
#   sudo ./g1-dkms-kernel-update.sh verify   # assert auto-rebuild happened
#
set -euo pipefail

log()  { printf '\033[1;32m[+]\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m[!]\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31m[x]\033[0m %s\n' "$*" >&2; exit 1; }
pass() { printf '\033[1;32m[PASS]\033[0m %s\n' "$*"; }
fail() { printf '\033[1;31m[FAIL]\033[0m %s\n' "$*" >&2; exit 1; }

STATE="/var/tmp/openvtl-g1.state"
[[ $EUID -eq 0 ]] || die "Run as root."

phase_pre() {
  local before; before="$(uname -r)"
  log "Current (pre-update) kernel: ${before}"
  printf 'KERNEL_BEFORE=%s\n' "$before" > "$STATE"

  # mhvtl must already be registered and built for the current kernel.
  dkms status mhvtl 2>/dev/null | grep -q . \
    || die "mhvtl is not registered with DKMS. Run install-dkms.sh first."
  log "DKMS state before update:"
  dkms status mhvtl | sed 's/^/    /'

  # Debian has no dkms.service: autoinstall runs from the kernel package's
  # postinst hooks (/etc/kernel/postinst.d/dkms) at install time, so the
  # rebuild happens during the apt-get below — not at the next boot.
  if [[ -e /etc/kernel/postinst.d/dkms ]]; then
    log "DKMS kernel postinst hook present (/etc/kernel/postinst.d/dkms) — autoinstall runs at kernel install."
  else
    warn "/etc/kernel/postinst.d/dkms is MISSING — DKMS will not auto-rebuild on kernel installs; is the dkms package intact?"
  fi

  # Install the newest kernel AND its matching headers. Both are required:
  # without headers for the new kernel, autoinstall has nothing to build
  # against. Match the RUNNING kernel's flavour the same way install.sh does —
  # cloud images boot linux-image-cloud-amd64, and the generic metas there
  # would add a second kernel instead of updating the running one.
  local kflavor=amd64
  case "$before" in
    *-cloud-amd64) kflavor=cloud-amd64 ;;
    *-rt-amd64)    kflavor=rt-amd64 ;;
  esac
  log "Installing newest kernel and matching headers (linux-image-${kflavor} + linux-headers-${kflavor})..."
  apt-get update
  apt-get install -y "linux-image-${kflavor}" "linux-headers-${kflavor}"

  log "Installed kernels now present:"
  linux-version list | sed 's/^/    /'

  local newest
  newest="$(linux-version list | linux-version sort | tail -1)"
  if [[ "$newest" == "$before" ]]; then
    warn "No newer kernel was available than the running one (${before})."
    warn "The repo may already be at the running version. To force a meaningful"
    warn "test, boot an older kernel first, or wait for the next kernel update."
  else
    log "Newest installed kernel: ${newest} (expected after reboot)."
    printf 'KERNEL_EXPECTED=%s\n' "$newest" >> "$STATE"
  fi

  cat <<EOF

$(log "G1 'pre' phase done.")
  Reboot into the newest kernel, then run:
        sudo $0 verify
EOF
}

phase_verify() {
  [[ -f "$STATE" ]] || die "State file $STATE missing — run '$0 pre' first."
  # shellcheck disable=SC1090
  . "$STATE"

  local now; now="$(uname -r)"
  log "Running kernel after reboot: ${now}"
  log "Kernel before update      : ${KERNEL_BEFORE:-unknown}"

  if [[ "${now}" == "${KERNEL_BEFORE:-}" ]]; then
    fail "Still running the pre-update kernel (${now}). Did you reboot into the new one?"
  fi
  pass "Kernel changed (${KERNEL_BEFORE} -> ${now})."

  # The core assertion: DKMS shows mhvtl built+installed for the CURRENT kernel,
  # without anyone running dkms by hand after the reboot.
  log "DKMS status for current kernel:"
  dkms status mhvtl | sed 's/^/    /'

  if ! dkms status mhvtl 2>/dev/null | grep -F "$now" | grep -qiE 'installed'; then
    fail "mhvtl is NOT 'installed' for ${now} — auto-rebuild did not occur."
  fi
  pass "mhvtl auto-rebuilt and installed for ${now}."

  # And it must actually load on this kernel.
  modprobe mhvtl
  lsmod | grep -q '^mhvtl' || fail "mhvtl present in DKMS but failed to load."
  local vermagic; vermagic="$(modinfo mhvtl | awk '/^vermagic:/{print $2}')"
  [[ "$vermagic" == "$now" ]] || fail "vermagic (${vermagic}) != running kernel (${now})."
  pass "mhvtl loaded; vermagic matches running kernel."

  cat <<EOF

$(pass "G1 PASSED — mhVTL survives a kernel update with zero manual intervention.")
EOF
  rm -f "$STATE"
}

case "${1:-}" in
  pre)    phase_pre ;;
  verify) phase_verify ;;
  *)      die "Usage: $0 {pre|verify}" ;;
esac
