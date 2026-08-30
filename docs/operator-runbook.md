# OpenVTL operator runbook

**Applies to v1.0.** The day-to-day operational
reference: what a healthy appliance looks like, how to read and recover the
common failure states, how to handle a full pool, and how to restore or
recover carts from offsite. Scope is the appliance itself — configuration of
the IBM i / BRMS side is the customer's.

Companion docs: `docs/reference-vm-spec.md` (VM requirements + build),
`docs/why-fc-only.md` (why the product is FC-only), and the disk/RAM
sizing calculator at <https://openvtl.com/calculator>. Signed updates are
covered operationally in §7 of this runbook.

---

## 0. Orientation — where things live

The UI (`https://<host>:8443`, self-signed, admin login) is the operator
surface; the plaintext `:8080` endpoint is health/metrics only and is
optional (flag-disable). Everything destructive is admin-gated and audited.

| View | You use it to… |
|---|---|
| **Dashboard** | See at a glance: libraries, storage used, space saving (dedupe ratio), capacity trend, throughput, recent jobs. Auto-refreshes on a 10 s poll. |
| **Storage** | The system zpool + its pools; create/remove pools; **Grow storage**; **Tear down storage**; scan for new disks. |
| **Libraries** | Declare/activate libraries; mint & delete cartridges; per-cart history + vault actions; the "Finish deleting" / orphan banners. |
| **Access** | FC ports (serving toggles), registered initiators + library scoping, the LUN map. |
| **Offsite** | S3 remotes; the catalog (browse by system → library → cart → generation, Import / Recover); the **Raw** bucket browser (admin folder delete). |
| **Jobs** | Export/import/evict/pool jobs, timelines, chunk ledgers, Retry. |
| **Settings → System** | Restart (graceful / data-plane / reboot), API access keys, system name, IE-watcher & eviction policy. |
| **Settings → Updates** | Upload + apply a signed release bundle; roll back (§7). |

Golden rule that governs everything below: **raw detail lives in
`/api/status` + the journal; the UI speaks plain language.** When a caption is
vague, `journalctl -u openvtld` and `GET /api/status` have the specifics.

### 0.1 Auth & sessions

- **First-run setup gate.** Until the first admin is created, every API call
  answers `409 setup_required` and the UI shows the setup page. There are no
  default credentials — the first admin is created in the browser.
- **Passwords & sessions.** Passwords are argon2id-hashed. Login issues a
  **7-day sliding** HttpOnly session cookie (Secure, SameSite=Lax); only a
  SHA-256 of the token is stored server-side. Failed logins get the same
  delayed response for unknown user and wrong password, and are audited.
- **Roles.** `admin` may mutate; `readonly` gets every GET plus exactly one
  mutation — changing their own password. The last enabled admin can never be
  demoted, disabled, or deleted.
- **TLS.** A self-signed 10-year ECDSA cert is auto-generated on first run in
  `/var/lib/openvtld/tls/` (SANs = hostname + the box's IPs at generation
  time). To install a site certificate, replace `openvtld.crt` /
  `openvtld.key` there and restart openvtld; delete the pair to force
  regeneration (e.g. after an IP change).
- **The plaintext `:8080` listener** serves only `/healthz` + `/metrics` and
  redirects everything else to HTTPS. Start openvtld with `-listen ""` to
  disable it entirely. `/metrics` is **deliberately unauthenticated**
  (Prometheus scrape; gauges only) — keep it on a trusted management network
  or disable the plaintext listener.

---

## 1. Boot recovery

### 1.1 What a healthy boot looks like

On a ZFS appliance the boot chain is: `zfs-import-cache` → `zfs-import.target`
→ the pool mount (`var-lib-openvtl-pools-<pool>.mount`, ordered by
`x-systemd.requires=zfs-import.target` in fstab) → mhVTL daemons (ordered
after the mount by `RequiresMountsFor` drop-ins on `vtllibrary@`/`vtltape@`)
→ `openvtld`, which runs boot orchestration: settle sg discovery → engine
reload → ensure the target fabrics.

A clean boot logs a line like:

```
boot: target fabrics ensured — 2 ports, 3 LUNs identity-verified, ACLs reconciled
```

At that point: `zpool status`
ONLINE, datasets mounted, mhVTL daemons active, `/dev/sch*`+`/dev/st*`
enumerated, and `fc_target_verified=1`. Verify with
`GET /api/status` (`fabrics`) or the Access view.

### 1.2 Fresh boot after a reboot legitimately rebuilds the fabric

`openvtld` stamps a daemon-generation epoch at `/var/lib/openvtld/fc-built-epoch`.
A reboot restarts the mhVTL daemons → their start time is newer than the stored
epoch → boot orchestration does a **full fabric rebuild** on the renumbered sg
nodes. This is by design, not a fault. (Contrast a routine control-plane
restart — e.g. a Tier-A update, §7 — with no intervening reboot: the epoch
matches, so Ensure is *additive* and never rebuilds.)

### 1.3 The `targetcli exit 255 · target absent` early-boot flake

Occasionally the very first boot-time fabric rebuild hits
`target create naa…: targetcli exit 255` and leaves the (often empty) target
**absent**, `fc.verified=false`. This is an early-boot race, not a data problem.
**Root cause:** the distro's LIO boot restore
(`rtslib-fb-targetctl.service`) was racing openvtld's rebuild — worse, when the
race went the other way it restored yesterday's saveconfig with **pre-reboot sg
paths**, presenting the library to the host under a **wrong identity**.

- **Automatic:** install.sh **disables
  `rtslib-fb-targetctl.service`** (openvtld owns the fabric and rebuilds it from
  intent each boot; the distro restore is redundant *and* identity-unsafe).
  clearconfig is retried ×3 inside the rebuild, and the boot goroutine
  **self-heals with a backoff retry loop (~15 min)**.
  Rebuilds are per-port best-effort: one dead port doesn't take the
  whole fabric down, and the missing port is added back additively once it
  recovers.
- **If it persists** (a fault that survives the retries): the fix is a fabric
  rebuild. Use **Settings → System → Data-plane restart** (in-place mhVTL
  restart + fabric rebuild; drops host sessions briefly) or a reboot. A routine
  control-plane restart also re-runs Ensure and clears it. The next rebuild logs
  `fc: N port(s), M LUNs identity-verified, ACLs reconciled`.

### 1.3a FC ports dead after a rebuild — missing qla2xxx firmware

If a fabric change (target rebuild, port toggle) wedges
~30 s and afterwards **every FC port is `Offline`/`Linkdown`** with dmesg
showing `Failed to load firmware image (ql2500_fw.bin)` / `qla2x00_alloc_iocbs
failed` / `Cable is unplugged`, the box is missing **firmware-qlogic**. The HBA
boots on flash-resident firmware, but any ISP reset re-loads firmware from
`/lib/firmware` — absent, the port is dead **until reboot** (no software
recovery). Fix: `apt-get install firmware-qlogic`, then reboot. install.sh now
installs it on FC boxes and `--verify` warns when it's missing; the UI badge
detail also carries a `WARNING: qla2xxx firmware missing…` line on such boxes.

### 1.4 Zero-library boot is a legitimate empty target

If the box has 0 declared libraries (e.g. after deleting the last library), a
healthy boot serves an **empty, verified** target: `2 ports, 0 LUNs
identity-verified`. `device.conf` is empty, no `/dev/sch*`, no `vtl*` unit
instances — all expected. The host simply has nothing to mount.

### 1.5 Slow startup / "Access page hangs" after a library delete

A deleted live library's sg nodes **linger in the kernel until the next reboot**.
Until then, every SCSI probe D-states and is SIGKILL-immune (`sysexec WaitDelay`
eventually reaps it), so `openvtld` startup can take ~3 min and the Access page's
discovery feels like it hangs. **A reboot clears it.** This is why a live-library
delete is designed to reboot the box (§3).

---

## 2. Full-pool handling

### 2.1 Storage model — one zpool, global dedup

All pools live in **one system zpool `ovz`** (data vdev(s) + a permanent dedupe
SSD vdev). Dedup and compression are global across every `dedup=on` dataset in
the instance — that is the whole point of the ZFS design, so **cross-customer
isolation lives at the instance / S3-namespace level, not per pool.** A "pool"
is a ZFS dataset; removing one is `zfs destroy -r`.

Two numbers matter when judging fullness (Dashboard top strip + Storage card):

- **Physical / allocated** (`zpool alloc`, zfs `used`) — real space consumed
  after dedup+compression. This is what fills up.
- **Logical** (zfs `logicalused`) — pre-dedup size the host thinks it wrote.
  The gap between the two is your space saving (dedupe ratio).

The Storage card splits capacity **per vdev**: *data capacity* and *dedupe
capacity*. The dedupe SSD's alloc stays tiny (a few KB of DDT per unique block)
even when the data vdev is full — that line is mostly headroom by design.

**Storage tunables (measured on real IBM i saves):** the zpool is
created with **zstd** compression (~16 % more capacity than lz4 and *faster* on
disk-bound boxes; systems built on older releases stay on lz4 — the Storage card shows
which) and **`dedup_table_quota=auto`** (the dedup table is bounded by the
dedupe SSD; when it fills, new writes simply stop deduping instead of
thrashing). Each pool's **recordsize — the dedupe granularity — is scaled from
installed RAM at pool-creation time**: ≥24 GB → 16K, ≥12 GB → 32K, ≥6 GB →
64K, else 128K (shown as "dedupe granularity" on the pool card). Smaller
records catch far more duplicate data (repeat full-system saves: 1.0× at the
old 1M default vs ~1.3–1.9× at 16K; repeated library saves ~10× at 16K) at a
modest compression cost. The choice is stamped at creation — **adding RAM
later means new pools get finer granularity; existing pools keep theirs**
(recordsize only affects new writes). To re-tier an existing pool, export its
carts, remove and recreate the pool, and re-import.

### 2.2 Growing storage

When you enlarge a data vHDD (or the dedupe SSD) at the hypervisor, tell the
appliance to claim the new space:

1. **Storage → Scan for new disks** if the enlargement isn't visible yet
   (`POST /api/storage/rescan` — a SCSI bus re-probe; a true hot-add usually
   self-appears via kernel hot-plug).
2. **Storage → Grow storage** (`POST /api/storage/grow`). This runs
   `zpool online -e <by-id>` on **every data *and* dedupe vdev** — online,
   non-destructive, idempotent, a no-op for any vdev that didn't grow. It
   reports the delta. (`zpool` requires the by-id vdev name, which the daemon
   resolves; a bare `/dev/sdX` is rejected.)

Growing the **dedupe** vdev raises the reported pool *size* (dedupe vdev space
counts toward `zpool SIZE`) even though it's DDT/metadata headroom, not tape
capacity — expected, consistent with how the total is summed.

Not covered by Grow: **adding a new disk** (widening a vdev) — a different
operation, do it deliberately.

### 2.3 Freeing space when a pool is genuinely full

Options, least to most drastic:

- **Evict exported carts.** A cart that has been verified-uploaded to S3 can be
  evicted (Library → cart → Evict, or the pool-fullness eviction policy in
  Settings). Eviction leaves a stub (`.openvtl-evicted.json` + `mam` +
  `mhvtl_data`) and frees the bulk data; the host reads the stub as an
  unlabelled tape and errors loudly rather than silently overwriting — on the
  IBM i a mount fails with **`Media error on volume *N device <DEVD>`** (the
  `*N` is the host seeing an unlabelled medium). Re-import
  restores it byte-identical (§4). Default policy keeps data local; enable
  threshold eviction per site if wanted.
- **Grow the data vdev** (§2.2) if the hypervisor has room.
- **Remove an empty pool** you no longer need (Storage → pool card → Remove
  pool; requires its library be deleted first). Under ZFS this is a synchronous
  `zfs destroy -r` of that dataset only — the zpool and disks are untouched.

### 2.4 Remove pool vs. Tear down storage (don't confuse them)

These were **deliberately decoupled**:

- **Remove pool** destroys one pool's dataset. Storage stays set up; you can add
  another pool immediately. Removing the *last* pool no longer tears anything
  down.
- **Tear down storage** (Storage → System-storage panel → *Tear down storage*,
  enabled only at 0 pools; `POST /api/storage/teardown`, confirm `"teardown"`)
  destroys zpool `ovz` and `wipefs -a`'s the data disk(s) back to *available*.
  The **dedupe SSD is permanent** — chosen once at setup, kept reserved, and
  re-added automatically at the next setup (setup rejects a *different* dedupe
  device: "fixed for this system").

Both are pure ZFS/wipefs — no SCSI/mhVTL/FC touch, no reboot.

---

## 3. Library & cartridge maintenance

- **Declaring / activating a library** and the **data-plane restart** happen
  in place (mhVTL restart + fabric rebuild). Safe, because every *declared*
  daemon returns.
- **Deleting a LIVE library reboots the box.** This is the
  design: remove config + purge that library's media + clean DB, then
  `systemctl --no-block reboot`; the fresh boot serves only the survivors and
  the removed devices are never re-created. Delete is **cascade** — the
  library's drives and *every* cartridge in it go with it (S3 copies are kept).
  It is a maintenance-window action; the maintenance overlay shows a persistent
  "reconnecting…" until the box is back.
- **Deleting a *pending*/*orphan* library** does **not** reboot (nothing is
  registered in the kernel).
- **Minting cartridges:** count is capped at the library's free slots; size is
  derived from the drive's LTO type (no size input); the next label autofills.
  Rapid back-to-back API mints can read a stale snapshot — pace them (the UI
  does; the label rail catches any dup).
- **"Finish deleting" / orphan banner** on Libraries: a DB row with no live
  device (e.g. an interrupted delete). Complete it or leave it — cart data on
  disk is intact.

### 3.1 NEVER do these

- **Never `echo 1 > /sys/…/delete` an mhVTL SCSI device.** It corrupts mhVTL's
  kernel LU list → panic (`add_lu_store` / `__list_add_valid_or_report`), box
  needs a console hard-reset (a kernel oops on a lab box). The *only* safe
  way to release a removed live library's devices is a **reboot** — which is
  exactly what live-delete does.
- **Never `systemctl restart openvtld` directly over SSH on a live FC
  appliance.** Use the UI System button (or the updater, which restarts it for
  you — §7). A raw restart during a live host session risks the fabric.
- **Never delete an individual chunk/manifest from the bucket by hand, and never
  raw-folder-delete without first confirming the carts have copies under another
  serial/generation.** The Raw browser enforces folder-only deletes for this
  reason.

---

## 4. Offsite, restore, and DR

### 4.1 S3 layout (the mental model)

Objects are keyed **System → Library serial → cart Label → Generation**:

```
<prefix>/<system-name>/<library-serial>/<label>/<generation>/{manifest.json, chunk-*.tar.zst}
```

- **System** = `system.name` (friendly, path-safe, editable in Settings) paired
  with a stable `system.instance_uuid` (marker at
  `<system>/.openvtl-system.json`, and in every manifest) so cloned/duplicate
  names are detectable and each subtree's producing instance stays traceable.
  The name must be **path-safe**: 1–32 chars, lowercase letters, digits, `-`
  or `_`, starting with a letter or digit (default: the sanitized hostname).
  **Renaming the system** is allowed but S3 keys are immutable — existing
  exports keep their old-name keys and appear as a separate system in the
  catalog; new exports land under the new name. The instance UUID in every
  manifest and marker is what ties both subtrees to the same appliance.
- **Library serial** is immutable (S3 keys are). The friendly library *name* is
  display-only in the manifest.
- **Cart label is sacred** — preserved end-to-end, source = S3 = target. It is
  the cartridge primary key.

The manifest (v2) is uploaded **last** as the completion marker. The **catalog
is rebuildable from the bucket listing alone** (Offsite → Rebuild ingests every
system's subtree). Multiple instances can share one bucket and see each other's
catalogs.

### 4.2 Export / evict / import round trip (same instance)

- **Export**: manual (Library → cart → Export) or automatic via the IE watcher
  (an eject on the host lands the cart in the I/E element → auto-export → verified
  upload → auto-unvault returns it to a slot). Export is a deterministic PAX tar
  → 10 GB chunks → per-chunk zstd → resumable per-chunk ledger.
- **Evict** (after verified upload): re-verifies the bucket, then leaves the
  stub. Frees the bulk data. A host that mounts the stub fails loudly — on the
  IBM i the command errors with **`Media error on volume *N device <DEVD>`**
  (the medium reads as unlabelled `*N`); nothing host-side will silently
  overwrite it.
- **Import**: streams from S3, verifies, rename-swaps into place. Byte-identical
  restore (`sha256sum -c` proven repeatedly). Import of a same-instance evicted
  stub is an in-place re-import.

**Resume/cancel:** a cancelled or failed export **resumes from the chunk
ledger** on Retry (contiguous uploaded chunks under the same generation are
skipped) — so leaving a cancelled export's partial chunks in the bucket is
correct; the retry reuses them. Cancel is near-instant even
mid-chunk. This is the flow to reach for when a big cart (e.g. a ~280 GB IBM i
system save) is interrupted across restarts.

### 4.3 The offsite scratch-recovery / DR flow (cross-instance)

When the local carts are gone (dead site, wiped appliance, cross-site DR) and you
need to pull them back from a shared bucket, **Rebuild does not recreate a
library** — it only refreshes the catalog cache. Recovery is one of two paths in
the **Offsite → Catalog** view:

**Single cart → an existing library (Phase A).** Catalog → the generation →
**Import into [library ▾]**. The foreign cart is extracted, placed at the target
library's home slot, adopted via the same MAP-transit a mint uses, slot-assigned,
and registered — **label preserved**. If the label already exists locally, import
**refuses** ("import under a new label") rather than silently renaming; in real DR
the target library is empty, so no collision.

**Whole library → one click (Phase B).** Catalog groups System → Library → cart
with a **Recover library** button on any library not present locally (with a
free-pool picker). It reads the library's `topology.json` from S3 (model, drive
model, drive/slot/MAP counts, prefix, serial), **creates a fresh local library on
the chosen pool adopting the original serial**, applies it, then enqueues a
foreign import of every cart's newest generation (job trigger `recover`). Result:
the library and all its carts come back with **labels preserved**.

> After recovery, the **library serial is the flexible part** — the operator
> re-points the IBM i hardware resource (RSC) + device description (DEVD) at the
> recovered library. The cart labels are what BRMS/the catalog care about, and
> those are preserved.

**Raw browser (admin, cleanup only):** Offsite → remote → **Raw** lists objects
and allows **folder-level** delete (`prefix` must end in `/`; individual
chunks/manifests and the whole-bucket prefix are refused). Before deleting a
folder, confirm the carts have copies under another serial/generation.

---

## 5. Targets & access (FC)

- `GET /api/targets` / the Access view is one picture: FC ports, the LUN
  map (with library column), registered initiators, and any *unmanaged* configfs
  ACLs (surfaced, never auto-deleted).
- **Every target-capable FC port serves the full LUN table** (dark ports
  pre-armed). Toggle a port off per-port if needed (`fc.disabled_ports`). FC is
  symmetric across ports — there is no target-WWN to pick.
- **Access is hard-deny.** An unregistered initiator is refused (the kernel logs
  "not authorized" — that's correct). Register it (Access → initiators) with an
  optional **library scope**; scoping maps exactly that library's LUNs. A scope
  change bounces that one initiator's session briefly (the UI warns); other
  initiators are untouched.
- **There is no iSCSI.** OpenVTL is FC-only: the
  IBM i 298A-001 iSCSI IOP crashes on every successful iSCSI login (an IBM
  firmware defect — see `docs/why-fc-only.md`), so the product ships no iSCSI
  fabric. A box upgraded from a pre-release iSCSI-era build sheds any leftover
  iSCSI target at its next reboot (openvtld rebuilds targets from scratch at
  boot and does not create one).
- **A target FAULT during a host-side IOP reset / IPL is a NORMAL VTL-world
  event.** The fault tooltip gives operator guidance (IOP Reset / Concurrent
  Maintenance / IPL); raw detail is in `/api/status` + the journal. Don't chase
  a transient fault that clears when the host comes back.

---

## 6. Reading system health quickly

- `GET /api/status` — libraries, pools, `fabrics{fc}`, jobs, drives.
- `GET /api/system` — system name/uuid, restart/reboot controls context.
- `journalctl -u openvtld -b` — boot orchestration lines, ensure/rebuild
  results, any faults (boot ensure now logs both success **and**
  not-verified-after-retry).
- Prometheus `:8080/metrics` (if enabled) — `openvtl_target_sessions_active{fabric}`,
  capacity, throughput.
- On the FC box, active host sessions are visible **only** in
  `/sys/kernel/debug/qla2xxx/qla2xxx_<host>/tgt_port_database` — NOT in
  `fc_remote_ports`, `targetcli sessions`, or WRKHDWRSC ("Active" there is
  link-level). The nexus follows the host's device vary on/off.

Reading the storage numbers without fooling yourself:

- `zpool status -D`'s **"in core"** DDT figure is the *theoretical
  fully-loaded* table size, not current residency — compare it against the
  ARC cap for context, but it is **not an alarm** by itself.
- Measure compression on **whole streams**: a mid-stream slice under-reads
  real compressibility (a partial save can read ~1.2× where the full stream
  is ~1.45×).
- **Byte-copy duplicates are invalid dedup evidence.** A `DUPTAP` (or any
  byte-for-byte copy) dedupes trivially; only independent repeat saves say
  anything about real-world savings.
- A cart is **truly blank** only if mhVTL's `dump_tape` shows zero filemarks
  and EOD at block 0 — never judge blankness from the size of the cart's
  on-disk data file.

---

## 7. Installing & updating the appliance

### 7.1 Fresh install

The release bundle (`openvtl-<ver>.tar.gz`) is unpacked on the VM and installed
with `repo/packaging/install.sh` (idempotent; phases prep / mhvtl / openvtld /
verify; `--verify` runs the gate alone). Before consuming any bundle file the
installer **verifies the bundle**: the Ed25519 signature over `SHA256SUMS` is
checked (openssl) against the public key baked into the installer itself, then
every file against `SHA256SUMS`. It then enables Debian `contrib`,
installs `zfsutils-linux` + `zfs-dkms` and the mhVTL DKMS module, applies the
mhVTL source patches (1–16; patch 16 targets a retired identity variant no
library uses), and runs a verify gate. Secure Boot must be **off** (the
installer refuses otherwise). On an FC box the installer also configures the
HBA for target mode (`qlini_mode=disabled` via `/etc/modprobe.d/qla2xxx.conf`)
— **one reboot** after a fresh FC install before targets present.

### 7.2 Routine updates — signed bundles, Tier A

Two ways to apply a release bundle to a fielded appliance, same core either
way:

- **CLI:** copy the bundle to the box, `sudo openvtld update <bundle.tar.gz>`.
- **Browser:** **Settings → Updates** (admin) uploads the bundle; the daemon
  prechecks it (bad bundles are refused in the upload response), then a
  detached unit runs the same apply path. The UI rides out the restart by
  polling `/healthz` until the new version answers.

What the updater guarantees:

- **Authenticity first.** Every bundle carries an Ed25519 signature over its
  `SHA256SUMS`; the updater verifies it against the public key baked into the
  running binary before trusting anything else in the bundle, then hashes
  every listed file. `openvtld verify-bundle <bundle.tar.gz|dir>` runs the
  same check without applying.
- **Tier A is control-plane-only.** An accepted update restarts *only*
  `openvtld`; mhVTL is untouched, so tape I/O and live FC host sessions ride
  through (the fabric epoch matches — Ensure is an additive no-op verify,
  §1.2).
- **Refusals leave the box untouched** (CLI exit 3):
  - **Same version** — nothing to update (`--force`-able).
  - **Downgrade** — the bundle's build date is older than the running
    binary's. Not `--force`-able: backward motion is `openvtld rollback`,
    which pairs the matching DB snapshot (migrations are forward-only).
  - **Tier B** — the bundle's mhVTL pin differs from the installed one, i.e.
    it updates the data plane. Not `--force`-able: run
    `<bundle>/repo/packaging/install.sh` in a maintenance window instead.
  - **Mid-flight jobs** — retry when idle, or `--force` if you accept the
    interruption.
- **The flow:** verify → preflight → back up the current binary
  (`openvtld.prev`) + a consistent DB snapshot (`/var/lib/openvtld/backups/`)
  → stage the new binary and self-check its `-version` → atomic swap →
  restart → confirm healthy (or roll back, §7.3).

`openvtld update-status` shows any pending update and the last-known-good
rollback target. `GET /healthz` reports the running `version`
(unauthenticated) — probe it to see which binary is actually answering.

### 7.3 Rollback & the auto-rollback watchdog

- **Manual:** `openvtld rollback` (or **Settings → Updates → Roll back**)
  restores the last-known-good **binary + DB snapshot pair** and restarts.
  The two always revert together — never run an older binary against a
  database a newer one has migrated.
- **Automatic:** if an applied update never becomes healthy, three cooperating
  mechanisms cover it: the updater process blocks on a health probe (90 s
  default) and rolls back itself if the new version never answers; the freshly
  started daemon self-confirms once healthy; and an `ExecStartPre` watchdog
  runs before *every* service start — after the crash-restart budget it
  restores the previous binary + DB snapshot and lets the good binary start.
  A DOA binary, a panic on start, and a failed migration all present as a
  crash loop, and all are caught.
- **Documented residual gap** (rare): a new binary that *runs but is unhealthy
  without crashing*, after the updater process is gone, is not auto-reverted.
  Recover by hand via the previous binary:
  `/usr/local/bin/openvtld.prev rollback` (or copy `openvtld.prev` over
  `/usr/local/bin/openvtld`, restore the newest snapshot from
  `/var/lib/openvtld/backups/`, and restart the service).

---

## 8. Quick incident index

| Symptom | Section |
|---|---|
| Post-reboot FC target shows a fault / `verified=false` | §1.3 |
| Startup slow / Access page hangs after a library delete | §1.5 |
| Pool filling up | §2.3 |
| Enlarged a disk, appliance doesn't see the space | §2.2 |
| Need to free the data disk entirely | §2.4 |
| Local carts gone, restore from bucket | §4.2 / §4.3 |
| Big export interrupted across restarts | §4.2 |
| Whole library lost, one-click bring-back | §4.3 (Phase B) |
| Initiator refused / needs scoping | §5 |
| Update refused (same-version / downgrade / Tier-B / mid-job) | §7.2 |
| Update applied but the box is unhealthy | §7.3 |
| UI down, data plane wedged | Appendix A |
| Box unreachable after touching `/sys/.../delete` | §3.1 (do not do this) |

---

## Appendix A — Manual data-plane recovery (UI down)

The normal path for a wedged data plane is the UI's managed restart
(**Settings → System → Data-plane restart**), which handles ordering and the
fabric rebuild for you. If the UI itself is unreachable, do the same thing by
hand — the ordering matters, because a plain `systemctl stop mhvtl.target`
can leave vtltape/vtllibrary orphans holding `/var/lock/mhvtl/*`, and
restarted instances then exit with "found another running daemon":

```
systemctl stop openvtld
systemctl stop mhvtl.target
pkill -9 -x vtltape; pkill -9 -x vtllibrary
rm -f /var/lock/mhvtl/*
systemctl start mhvtl.target      # verify the vtl* units are active and
                                  # lsscsi -g shows the changer with an sg node
systemctl start openvtld
```

If anything is D-state wedged in the kernel (SIGKILL undeliverable, probes
hanging), **reboot** — boot orchestration is designed to survive it (§1).
