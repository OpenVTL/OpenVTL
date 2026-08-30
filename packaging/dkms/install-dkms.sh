#!/usr/bin/env bash
#
# OpenVTL — register, build, and install the mhVTL module via DKMS.
#
# Run AFTER stage-sources.sh has populated /usr/src/mhvtl-<version>.
# Idempotent: removes any prior DKMS registration of the same version first.
#
# Usage:  sudo ./install-dkms.sh --version 1.7.4
#
set -euo pipefail

# dkms/depmod/modprobe/modinfo live in /usr/sbin, which a `su` (non-login) root
# shell may omit from PATH; ensure they resolve regardless of how root was entered.
export PATH="/usr/sbin:/sbin:$PATH"

log()  { printf '\033[1;32m[+]\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m[!]\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31m[x]\033[0m %s\n' "$*" >&2; exit 1; }

VERSION=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --version) VERSION="${2:?}"; shift 2 ;;
    -h|--help) grep '^#' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *)         die "Unknown argument: $1" ;;
  esac
done

[[ $EUID -eq 0 ]]   || die "Run as root."
[[ -n "$VERSION" ]] || die "--version is required."

MOD="mhvtl/${VERSION}"
SRC="/usr/src/mhvtl-${VERSION}"
KREL="$(uname -r)"

[[ -f "${SRC}/dkms.conf" ]] || die "${SRC}/dkms.conf not found — run stage-sources.sh first."
# Kernel build tree: /lib/modules/<rel>/build works on both Debian (linux-headers)
# and EL (kernel-devel). This is the path DKMS itself uses.
[[ -d "/lib/modules/${KREL}/build" ]] \
  || die "No /lib/modules/${KREL}/build — install kernel headers (Debian: linux-headers-${KREL}; run install.sh --phase prep)."

# Clean any prior registration of this exact version (idempotency).
# Capture to a string rather than piping into `grep -q`: under `set -o pipefail`,
# `grep -q` closes the pipe on its first match and SIGPIPEs the left-hand command,
# which pipefail then reports as a pipeline failure — a false negative.
if [[ -n "$(dkms status "$MOD" 2>/dev/null)" ]]; then
  log "Removing existing DKMS registration for ${MOD}..."
  dkms remove "$MOD" --all || true
fi

log "dkms add ${MOD}"
dkms add "$MOD"

log "dkms build ${MOD} (kernel ${KREL})"
dkms build "$MOD"

log "dkms install ${MOD}"
dkms install "$MOD"

log "Refreshing module dependencies and loading mhvtl..."
depmod -a
modprobe mhvtl

# --- verification ----------------------------------------------------------
log "DKMS status:"
dkms status "$MOD" | sed 's/^/    /'

# Check sysfs, not `lsmod | grep -q`: a freshly loaded module sorts to the top of
# lsmod, so `grep -q` matches and exits on line 1, SIGPIPEs lsmod, and under
# `set -o pipefail` that surfaces as a (false) pipeline failure — the module loads
# fine but the check reports it absent. /sys/module/<name> is the canonical, pipe-free test.
if [[ ! -d /sys/module/mhvtl ]]; then
  die "mhvtl module did not load (/sys/module/mhvtl absent)."
fi
log "mhvtl loaded:"
modinfo mhvtl 2>/dev/null | grep -E '^(filename|version|vermagic):' | sed 's/^/    /'

cat <<EOF

$(log "DKMS install complete.")

  Module     : ${MOD}
  Built for  : ${KREL}
  Loaded     : yes

  Validate auto-rebuild before trusting it at fleet scale:
        sudo tests/g1-dkms-kernel-update.sh pre
        reboot
        sudo tests/g1-dkms-kernel-update.sh verify

  Then start the mhVTL userspace daemons; all configuration is owned by
  install.sh and the product's first-run UI.
EOF
