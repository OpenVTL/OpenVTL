#!/usr/bin/env bash
#
# OpenVTL — list PCI devices grouped by IOMMU group (PVE host).
#
# Run on the Proxmox host AFTER enabling IOMMU and rebooting. Use it to confirm
# the QLogic HBA's two functions sit in an IOMMU group that is clean enough to
# pass through (ideally only the HBA functions, optionally with their PCIe
# bridge — nothing else important).
#
# Usage:
#   ./iommu-groups.sh            # all groups
#   ./iommu-groups.sh 1077       # only groups containing a QLogic (vendor 1077) device
#
set -euo pipefail

filter="${1:-}"

shopt -s nullglob
groups=(/sys/kernel/iommu_groups/*)
if [[ ${#groups[@]} -eq 0 ]]; then
  echo "No IOMMU groups found. Is IOMMU enabled? Check: dmesg | grep -i -e DMAR -e IOMMU" >&2
  exit 1
fi

for g in $(printf '%s\n' "${groups[@]}" | sort -t/ -k5 -n); do
  gid="${g##*/}"
  # Collect device lines for this group.
  lines=()
  for d in "$g"/devices/*; do
    bdf="${d##*/}"
    lines+=("$(lspci -nns "$bdf")")
  done
  # Apply optional vendor/text filter to the whole group.
  if [[ -n "$filter" ]]; then
    printf '%s\n' "${lines[@]}" | grep -qi "$filter" || continue
  fi
  printf 'IOMMU group %s:\n' "$gid"
  printf '    %s\n' "${lines[@]}"
done
