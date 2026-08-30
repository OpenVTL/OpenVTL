# QLogic QLE2562 — VFIO passthrough on Proxmox (PVE host)

Binds the dual-port **QLE2562** (8 Gb FC, QLogic ISP2532, PCI `1077:2532`) to
`vfio-pci` on the PVE host and attaches both ports to the OpenVTL VM as
`hostpci0`/`hostpci1`. Target mode (`tcm_qla2xxx`) runs **inside the guest** —
the host only needs to hand the card over cleanly.

All commands run on the **Proxmox host**.

---

## Step 0 — Confirm the card can do target mode BEFORE committing

This is the gate. Don't bind a card to VFIO and build around FC if `tcm_qla2xxx`
can't drive it.

```bash
# 0a. Identify the HBA and confirm the chip family.
lspci -nn | grep -i -e fibre -e qlogic
#   Expect two functions of one device, e.g.:
#   81:00.0 Fibre Channel [0c04]: QLogic ISP2532 ... 8Gb FC [1077:2532]
#   81:00.1 Fibre Channel [0c04]: QLogic ISP2532 ... 8Gb FC [1077:2532]
```

Decision:
- **`1077:2532` (ISP2532, 25xx series) → supported.** `tcm_qla2xxx` supports
  the 24xx/25xx/26xx generations; the QLE2562 is 25xx.
- **A 23xx series card (`1077:2312`/`2322`, ISP23xx) → STOP.** Older QLA23xx do
  **not** support `tcm_qla2xxx` target mode. Source a 25xx/26xx card.

```bash
# 0b. Confirm the host's qla2xxx driver is target-capable on this kernel.
modinfo qla2xxx     | grep -i qlini_mode      # the target-mode param must exist
modinfo tcm_qla2xxx | head -3                  # the LIO FC fabric module must be present
```

```bash
# 0c. (Optional, strongest pre-commit proof) exercise target mode on the HOST
#     once, while the card is still bound to qla2xxx — before VFIO.
modprobe tcm_qla2xxx
cat /sys/module/qla2xxx/parameters/qlini_mode      # default is usually 'enabled' (initiator)
ls /sys/kernel/config/target/qla2xxx/ 2>/dev/null  # exists once tcm_qla2xxx is loaded
# Bringing the port up as a target here proves the firmware allows it. Full
# proof is the in-guest bring-up after install; 0a+0b are sufficient to commit to FC.
```

> WWPN caveat (affects host-side enrollment): `tcm_qla2xxx` presents the
> **physical port WWPN** of the passed-through card as the target, and that
> WWPN is what the IBM i / BRMS side enrolls. **A new HBA means a new WWPN**,
> so the IBM i/BRMS side re-enrolls the library port. When replacing an
> appliance whose enrollment must survive, either **reuse the same physical
> HBA** or plan to match the WWPN via NPIV. Record this per site.

---

## Step 1 — Enable IOMMU on the host

Add the IOMMU flags to the kernel command line. Pick by CPU vendor:

- Intel: `intel_iommu=on iommu=pt`
- AMD:   `amd_iommu=on iommu=pt`

Find your bootloader, then edit the right place:

```bash
proxmox-boot-tool status      # tells you systemd-boot vs grub
```

**systemd-boot** (typical on ZFS-root PVE):
```bash
# /etc/kernel/cmdline is a single line — append the flags, keep root=... intact.
nano /etc/kernel/cmdline
proxmox-boot-tool refresh
```

**GRUB**:
```bash
# Append inside the quotes of GRUB_CMDLINE_LINUX_DEFAULT="quiet ...".
nano /etc/default/grub
update-grub
```

Reboot, then verify:
```bash
dmesg | grep -i -e DMAR -e IOMMU | grep -i enabled    # IOMMU/DMAR active
```

---

## Step 2 — Load VFIO modules

```bash
cat >> /etc/modules <<'EOF'
vfio
vfio_iommu_type1
vfio_pci
EOF
# vfio_virqfd is only a separate module on kernels < 6.2; harmless to omit on PVE 8.

update-initramfs -u -k all
```

---

## Step 3 — Verify IOMMU grouping

```bash
scripts/proxmox/iommu-groups.sh 1077
```

You want the QLE2562's two functions (`81:00.0`, `81:00.1`) **alone** in their
group, or sharing it only with their own PCIe root port — nothing else
important.

- **Clean group → proceed.**
- **Group contaminated** with unrelated devices (common on consumer boards):
  move the card to a different physical slot first. Only consider an ACS-override
  kernel arg (`pcie_acs_override=...`) as a last resort — it weakens isolation
  between devices and is a real security/stability tradeoff. Server boards
  almost always group HBAs cleanly.

---

## Step 4 — Bind the card to vfio-pci

Bind by PCI ID so both functions are captured, and make `vfio-pci` win the race
against `qla2xxx`:

```bash
cat > /etc/modprobe.d/vfio.conf <<'EOF'
options vfio-pci ids=1077:2532
softdep qla2xxx pre: vfio-pci
EOF
```

On a dedicated VTL host (the host never uses FC itself) you can also hard-stop
the host driver — belt and suspenders:

```bash
echo 'blacklist qla2xxx' > /etc/modprobe.d/blacklist-qla2xxx.conf
```

> If the host has *other* QLogic 2532 cards it must keep for itself, do **not**
> blacklist by vendor:device — bind by specific PCI address with a
> `driver_override` instead. Not the case for a dedicated VTL host.

Apply and reboot:
```bash
update-initramfs -u -k all
reboot
```

---

## Step 5 — Verify the bind

```bash
lspci -nnk -d 1077:2532
# Both functions must show:
#   Kernel driver in use: vfio-pci
ls -l /dev/vfio/          # a numbered group node should exist for the HBA's group
```

If either function still shows `qla2xxx`, the bind lost the race — recheck
`/etc/modprobe.d/vfio.conf`, that you ran `update-initramfs -u -k all`, and that
the card is not contaminating a group with a device pulled in earlier.

---

## Step 6 — Attach both ports to the VM

VM must be **powered off**; machine type must be **q35** (it is, per the VM spec):

```bash
VMID=9002        # match your VM (the reference-vm-spec example uses 9002)
qm set "$VMID" --hostpci0 0000:81:00.0,pcie=1
qm set "$VMID" --hostpci1 0000:81:00.1,pcie=1
```

`pcie=1` exposes them as native PCIe (q35 only). No `x-vga`/`romfile` needed —
this is an HBA, not a GPU; the QLE2562 supports FLR so it resets cleanly between
VM restarts.

---

## Step 7 — In-guest check

After the guest boots with the HBA:
```bash
# inside the Debian 13 guest
lspci -nn | grep -i qlogic
dmesg | grep -i qla2xxx
ls /sys/class/fc_host/                 # host ports appear here
```
Target-mode configuration is handled by the OpenVTL installer: on an FC box it
puts the HBA in target mode (`qlini_mode=disabled` via
`/etc/modprobe.d/qla2xxx.conf`), which is a module load-time parameter — so
**reboot once after the install** before the box presents targets. Step 0
already told us the card can do it; this step just confirms the guest sees the
passed-through hardware.

---

## Risk checklist

| Risk | Mitigation |
|------|-----------|
| Card is a 23xx series | Step 0a catches it — no `tcm_qla2xxx` support, source a 25xx/26xx |
| IOMMU group contamination | Step 3; relocate slot before resorting to ACS override |
| `vfio-pci` loses bind race to `qla2xxx` | `softdep` + optional blacklist + rebuild initramfs |
| Enrolled WWPN ≠ passed-through card WWPN | Reuse the same physical HBA, or NPIV — record per site |
| Both ports forced to one guest | Same IOMMU group can't be split host/guest — expected here, both go to the VTL VM |
| Final target-mode firmware proof | The in-guest bring-up after install (Step 7); Steps 0a/0b de-risk the commitment now |
