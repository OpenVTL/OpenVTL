# OpenVTL reference VM spec

The VM OpenVTL's installer assumes. Build it in **any** hypervisor — the
requirements table is what binds; the PVE section is a worked example
(what we run).

Install flow once the VM exists: install Debian 13 → unpack the release
bundle → `sudo openvtl-<ver>/repo/packaging/install.sh` → commission in
the UI (first-run admin → Storage → Library → Targets → S3).

## 1. Requirements (hypervisor-agnostic)

| Area | Requirement | Why |
|---|---|---|
| OS | **Debian 13 (trixie), amd64, minimal/headless** — no desktop | tcm_qla2xxx + full targetcli-fb are in-tree; the out-of-tree modules (mhVTL, ZFS) are DKMS-managed. The installer enables Debian `contrib` for the ZFS packages |
| CPU | 2 vCPU (host-passthrough type where offered) | proven envelope; the VTL is I/O-bound, not CPU-bound |
| RAM | **4 GiB minimum, pinned — no ballooning; more RAM = more dedupe** | 4 GiB is the floor (≤2.5 GB resident + a 512 MB ARC). Installed RAM directly scales space savings: each pool's dedupe granularity (recordsize) is picked from MemTotal at pool-creation time (≥24 GiB → 16K, ≥12 → 32K, ≥6 → 64K, else 128K — smaller catches far more duplicate data across repeated saves) and the ZFS ARC is given all RAM minus an OS/daemon reserve of max(RAM/4, 2 GiB), with a 512 MB floor — the ARC must hold the dedup table, so more RAM directly helps. Size RAM for the retention model, not the daemon. PCI passthrough requires pinned RAM anyway |
| Firmware | UEFI with **Secure Boot OFF** | DKMS-signed mhVTL loads unattended; SB requires one interactive MOK enrollment instead (`packaging/dkms/README.md` Option B). The installer refuses to run with SB enabled |
| OS disk | ~50 GB | OS + openvtld state + export chunk staging (one chunk, 10 GB default, in flight at a time) |
| Data disk(s) | **one or more, left completely untouched** (no partitions, no FS) | the Storage wizard claims raw disks only — it refuses anything with signatures or mounts. All data disks join the one system zpool; pools are datasets sharing that capacity. Size: see §2 |
| Dedupe device | **one fast (SSD/NVMe-backed) disk, dedicated**, untouched | the system zpool holds its pool-wide dedup table (DDT) on a dedicated dedup vdev — latency-bound, so it must be fast flash. Chosen ONCE at first Storage setup and permanent for the instance. Size: see §2 |
| FC HBA | QLogic 2500/2600-family passed through as a **PCIe device** (optional) | tcm_qla2xxx target mode; the target WWN is auto-detected from the port. The installer puts the HBA in target mode (`/etc/modprobe.d/qla2xxx.conf` → `qlini_mode=disabled`) — this needs a **reboot** to take effect, so a fresh FC box must be rebooted once after install before it presents targets. No HBA is a legal shape (no host-facing fabric until one is attached) |
| NIC | one virtio/vmxnet NIC on the management network | web UI :8443 (HTTPS), optional :8080 (health/metrics) |
| Serial console | recommended | headless boxes; `qm terminal` / virsh console |

During the Debian install: partition **only the OS disk**; set hostname,
IP, and credentials there (that's all the OS-level site config OpenVTL
has — everything else happens in the product UI).

## 2. Sizing — guidance, not gospel

Disk sizing is workload-dependent; the sizing calculator at
<https://openvtl.com/calculator> (inputs: current tape data volume,
growth, retention, expected dedup) turns it into a per-site estimate.
Rules of thumb:

- **Data disk(s)**: at least the size of the *physical* (post-dedup,
  post-compression) data you expect to hold locally. Field reference:
  across 30 repeated full-system IBM i saves (~7.3 TiB written), the
  measured end-to-end saving was **~12–13×** (dedup × compression);
  compression alone (~1.4×) is the reliable floor on any workload —
  the calculator turns these measurements into a per-site
  estimate. All data disks join the one
  system zpool and pools (datasets) share that capacity — there is no
  per-pool fixed size; grow by attaching another data disk (Storage →
  Scan for new disks). Eviction to S3 is the pressure-relief valve.
- **Dedupe device**: the ZFS dedup table (DDT) lives here — budget a few
  GB of fast flash per TB of *unique* data; 50 G comfortably covers a
  small–mid site. Must be SSD/NVMe — a DDT on spinning rust cripples
  every write.
- Test/lab shape: 50 G OS + ~100 G–1 T data +
  ~50–150 G dedupe SSD. A small production site: 50 G OS + 1–3 T data +
  ~50 G+ dedupe SSD.

## 3. Worked example — Proxmox VE

Assumes ISO uploaded as `local:iso/debian-13-amd64-netinst.iso`, storage
`local-zfs`, bridge `vmbr0`. Adjust names/sizes to taste (§2).

```bash
VMID=9002
STORAGE=local-zfs
BRIDGE=vmbr0
ISO=local:iso/debian-13-amd64-netinst.iso

qm create "$VMID" \
  --name openvtl \
  --machine q35 \
  --bios ovmf \
  --cpu host \
  --sockets 1 --cores 2 \
  --memory 4096 --balloon 0 \
  --numa 0 \
  --ostype l26 \
  --scsihw virtio-scsi-single \
  --net0 virtio,bridge="$BRIDGE" \
  --serial0 socket \
  --vga std \
  --agent enabled=1 \
  --tablet 0 \
  --onboot 1

# UEFI vars, Secure Boot OFF (pre-enrolled-keys=0) — installer checks this
qm set "$VMID" --efidisk0 "$STORAGE:1,efitype=4m,pre-enrolled-keys=0"

# Disks: OS + pool data + shared cache. discard/ssd for thin reclaim.
qm set "$VMID" --scsi0 "$STORAGE:50,ssd=1,discard=on,iothread=1"    # OS
qm set "$VMID" --scsi1 "$STORAGE:100,ssd=1,discard=on,iothread=1"   # pool data (size per §2)
qm set "$VMID" --scsi2 "$STORAGE:50,ssd=1,discard=on,iothread=1"    # dedupe device (SSD/NVMe)

# Install media + boot order
qm set "$VMID" --ide2 "$ISO,media=cdrom"
qm set "$VMID" --boot order='ide2;scsi0'

# FC sites only — after the VFIO bind (docs/proxmox/qlogic-vfio-passthrough.md;
# IOMMU group check: scripts/proxmox/iommu-groups.sh):
# qm set "$VMID" --hostpci0 0000:81:00.0,pcie=1
# qm set "$VMID" --hostpci1 0000:81:00.1,pcie=1   # second port, if cabled
```

Resulting `/etc/pve/qemu-server/<VMID>.conf` (volume names assigned by PVE):

```ini
agent: enabled=1
balloon: 0
bios: ovmf
boot: order=ide2;scsi0
cores: 2
cpu: host
efidisk0: local-zfs:vm-9002-disk-0,efitype=4m,pre-enrolled-keys=0,size=4M
ide2: local:iso/debian-13-amd64-netinst.iso,media=cdrom
machine: q35
memory: 4096
name: openvtl
net0: virtio=BC:24:11:xx:xx:xx,bridge=vmbr0
numa: 0
onboot: 1
ostype: l26
scsi0: local-zfs:vm-9002-disk-1,discard=on,iothread=1,size=50G,ssd=1
scsi1: local-zfs:vm-9002-disk-2,discard=on,iothread=1,size=100G,ssd=1
scsi2: local-zfs:vm-9002-disk-3,discard=on,iothread=1,size=50G,ssd=1
scsihw: virtio-scsi-single
serial0: socket
sockets: 1
tablet: 0
vga: std
# FC sites only, after VFIO bind:
# hostpci0: 0000:81:00.0,pcie=1
# hostpci1: 0000:81:00.1,pcie=1
```

### PVE host notes

- **IOMMU/VFIO** for the HBA: `docs/proxmox/qlogic-vfio-passthrough.md`;
  verify grouping with `scripts/proxmox/iommu-groups.sh`.
- **e1000e host NICs**: a host with an Intel e1000e management NIC hung
  once under load (a hardware/driver EEE bug — not guest-related);
  disabling EEE/offloads on the host NIC is cheap insurance.
- Guest disk letters (`/dev/sdX`) **shuffle between boots** — normal;
  OpenVTL persists pool devices by-id and the Storage view shows the
  live mapping. Never script against remembered letters.

## 4. Guest post-install (before running the installer)

- Serial console (headless ops): `console=ttyS0,115200` in
  `GRUB_CMDLINE_LINUX`, `update-grub`, `systemctl enable serial-getty@ttyS0`.
- `apt install qemu-guest-agent` on PVE (IP display / clean shutdown).
- Leave the data + dedupe disks alone — the Storage wizard takes them raw.
