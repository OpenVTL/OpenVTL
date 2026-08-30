// List-shaped since v0.6: N libraries, N pools. The top-level
// cartridges list is the flat cross-library join carrying persisted
// meta (local_state, last_export_gen); per-library cartridges inside
// libraries[] are live inventory only.
export interface Snapshot {
  updated_at: string
  libraries: LibrarySnapshot[]
  pools: PoolStats[]
  cartridges: Cart[]
  fc: FCState
  fabrics: Fabrics
}

// Aggregate fabric health for the header chip (v0.7 UI; FC-only since
// 2026-08-24): FC state plus live session count in one glance.
export interface Fabrics {
  fc: { present: boolean; verified: boolean; detail: string; sessions: number }
}

export interface LibrarySnapshot {
  library: Library
  drives: Drive[]
  slots: Slot[]
  cartridges: Cart[]
}

export interface Library {
  id: number
  name: string // display name (defaults to serial); serial stays the host-facing identity
  product: string
  serial: string
  home_dir: string
  num_slots: number
  num_ie: number
  num_drives: number
  changer_sg: string
  live: boolean
}

export interface Drive {
  index: number
  library: number
  queue_id: number
  serial: string
  product: string
  sg: string
  st: string
  loaded: string
  activity: 'idle' | 'reading' | 'writing' | 'mounting'
  blocks_written: number
  blocks_read: number
  last_active: string
}

export interface Slot {
  kind: 'storage' | 'ie'
  num: number
  label: string
}

export interface Cart {
  label: string
  library: number
  size_bytes: number
  phys_bytes?: number // allocated on disk (post-compression)
  modified: string
  location: string
  local_state?: 'resident' | 'evicted'
  last_export_gen?: string
}

export interface PoolStats {
  name: string
  mountpoint: string
  fs_total_bytes: number
  fs_used_bytes: number
  vdo_phys_bytes: number
  vdo_used_bytes: number
  vdo_saving_pct: number
  cache_used_pct: number
  dedup_ratio: number
  compress_ratio: number
  logical_bytes: number
  record_bytes?: number // dedupe granularity (RAM-scaled at creation, v0.9)
  phys_est_bytes?: number // ≈ used ÷ global dedupratio (honest per-pool disk share)
  zpool_size_bytes?: number
  zpool_alloc_bytes?: number
  collected_at: string
}

// System ZFS pool (one per appliance): data disk(s) + one SSD dedup vdev.
export interface SystemStatus {
  ready: boolean
  zpool: string
  data_devs: string[]
  dedup_dev: string
  dedup_fixed: boolean // dedupe device is remembered/permanent (locked once chosen)
  dedup_ratio: number
  size_bytes: number
  alloc_bytes: number
  data_size_bytes: number
  data_alloc_bytes: number
  dedup_size_bytes: number
  dedup_alloc_bytes: number
  compression?: string // zstd since v0.9; lz4 on older systems
}

export interface FCState {
  target_wwn: string
  verified: boolean
  no_hba: boolean // no FC hardware: idle is healthy, not a fault
  detail: string
  verified_at: string
}

export interface VTLEvent {
  ts: string
  kind: string
  subject: string
  data?: Record<string, unknown>
}

// Support licensing key — a derived fingerprint of this machine. `changed`
// flips when a re-key (OS reinstall / hardware transfer) is detected.
export interface LicenseInfo {
  fingerprint: string
  changed: boolean
  previous?: string
}

// Persisted event_log rows (journal viewer)
export interface LoggedEvent {
  id: number
  ts: string
  kind: string
  subject: string
  detail?: string
}

// --- v0.4 pipeline ---

export type JobKind = 'export' | 'import' | 'evict' | 'pool_create' | 'pool_remove'
export type JobState =
  | 'detected' | 'quiescing' | 'chunking' | 'uploading' | 'verifying' | 'unvaulting'
  | 'queued' | 'fetching' | 'unpacking' | 'slotting' | 'evicting'
  | 'validating' | 'lvm' | 'cache' | 'vdo' | 'filesystem' | 'plumbing'
  | 'removing' | 'unmount' | 'lvremove' | 'release'
  | 'done' | 'failed' | 'cancelled'

export interface Job {
  id: number
  kind: JobKind
  cart_label: string
  remote_id?: number
  generation?: string
  state: JobState
  trigger: string
  bytes_total: number
  bytes_done: number
  chunks_total: number
  chunks_done: number
  error?: string
  created_at: string
  updated_at: string
  finished_at?: string
  system_name?: string
  target_library?: number
}

export interface JobEvent {
  id: number
  job_id: number
  ts: string
  from_state?: string
  to_state: string
  detail?: string
}

export interface ExportChunk {
  job_id: number
  idx: number
  s3_key: string
  raw_bytes: number
  stored_bytes: number
  sha256: string
  uploaded_at?: string
}

export interface Remote {
  id: number
  name: string
  endpoint: string
  region: string
  bucket: string
  prefix: string
  access_key: string
  use_ssl: boolean
  path_style: boolean
  created_at: string
  last_test_at?: string
  last_test_ok?: boolean
  last_test_detail?: string
  has_secret: boolean
}

export interface CatalogEntry {
  remote_id: number
  system_name: string
  library_serial: string
  cart_label: string
  generation: string
  logical_bytes: number
  stored_bytes: number
  chunk_count: number
  exported_at: string
  synced_at: string
}

export interface RebuildResult {
  complete: number
  incomplete: { system: string; library: string; label: string; generation: string }[]
  errors?: string[]
}

// One raw object in the bucket (key relative to the remote's prefix).
export interface BucketObject {
  key: string
  size: number
}

export interface AuditEntry {
  id: number
  ts: string
  actor: string
  remote_addr?: string
  action: string
  subject?: string
  params?: string
}

// --- v0.5 auth ---

export type Role = 'admin' | 'readonly'

export interface User {
  id: number
  username: string
  role: Role
  disabled: boolean
  created_at: string
}

// --- v0.5 targets; v0.7 multi-port + initiator registry ---

export interface PortView {
  host: string
  wwpn: string
  state: string
  speed: string
  serving: boolean
  built: boolean
}

export interface LUNView {
  lun: number
  backstore: string
  device: string
  library: number
}

export interface InitiatorView {
  wwpn: string // naa. WWPN
  alias: string
  fabric: 'fc'
  ports: string // comma-sep target WWNs, '' = all
  libraries: string // comma-sep library ids, '' = all
  created_at: string
  logged_in: boolean
  port_state?: string
  applied: boolean
}

export interface TargetsView {
  fc: { ports: PortView[]; no_hba: boolean }
  luns: LUNView[]
  libraries: number[]
  initiators: InitiatorView[]
  unmanaged?: string[]
  error?: string
}

// --- v0.6 libraries ---

export interface DriveModel {
  product: string
  vendor: string
  display: string
  family: string
  density: string
  suffix: string
  capacity_mb: number
  ibmi_compatible: boolean
  note?: string
}

export interface LibraryVariantModel {
  product: string
  family: string
  display: string
  creatable: boolean
}

export interface LibraryModel {
  vendor: string
  display: string
  variants?: LibraryVariantModel[]
  ibmi_compatible: boolean
  creatable: boolean
  max_drives: number
  note?: string
}

// Persisted library row (pairing, prefix, lifecycle state) — the live
// topology comes from the snapshot.
export interface LibraryRow {
  id: number
  name: string
  vendor: string
  product: string
  variant?: string
  serial: string
  drive_model: string
  num_drives: number
  label_prefix: string
  media_dir: string
  home_pool: number
  state: 'pending_restart' | 'active'
  created_at: string
}

// --- v0.6 storage ---

export interface BlockDevice {
  path: string
  by_id?: string
  size_bytes: number
  model?: string
  transport?: string
  rotational: boolean
  eligible: boolean
  reason?: string
  role?: string
}

export interface Pool {
  id: number
  name: string
  vg: string
  data_lv: string
  mountpoint: string
  data_dev: string
  cache_slice_bytes: number
  virtual_size_bytes: number
  state: 'creating' | 'active' | 'removing' | 'error'
  detail?: string
  created_at: string
}

// --- v0.9 in-UI updates ---

export interface UpdateMarker {
  from_version: string
  to_version: string
  from_build?: string
  to_build?: string
  db_snapshot?: string
  staged_at?: string
}

export interface UpdateStatus {
  version: string
  build: string
  pending: UpdateMarker | null // update staged/in flight, not yet confirmed
  last_good: UpdateMarker | null // rollback target
}

export interface UpdateLaunched {
  from: string
  to: string
  built?: string
  unit: string
  detail: string
}

// --- v0.7 system control + API keys ---

export interface SystemInfo {
  version: string
  started_at: string
  uptime_sec: number
  active_jobs: number
  plain_listener: string // '' = disabled
  draining: boolean
  apikeys_enabled: boolean
  system_name: string // friendly instance name (S3 <system> segment)
  system_uuid: string // stable backstop
}

export interface APIKey {
  id: number
  name: string
  role: Role
  created_by: string
  created_at: string
  last_used_at?: string
}

export type Settings = Record<string, string>

export const TERMINAL_STATES: JobState[] = ['done', 'failed', 'cancelled']

export function isActive(j: Job): boolean {
  return !TERMINAL_STATES.includes(j.state)
}

export function fmtBytes(n: number): string {
  if (n <= 0) return '0 B'
  const u = ['B', 'KiB', 'MiB', 'GiB', 'TiB']
  const i = Math.min(u.length - 1, Math.floor(Math.log2(n) / 10))
  const v = n / 2 ** (10 * i)
  return `${v >= 100 ? v.toFixed(0) : v.toFixed(1)} ${u[i]}`
}

// Standard VTL dedup-ratio presentation: data stored : physical
// consumed, from bytes. Below a floor of stored data the number is
// meaningless (physical usage includes the dedup table and dataset
// metadata), so return null and let callers render '—'.
const RATIO_FLOOR = 1 << 30 // 1 GiB stored

export function fmtRatio(fsUsed: number, physUsed: number): string | null {
  if (fsUsed < RATIO_FLOOR || physUsed <= 0) return null
  const r = fsUsed / physUsed
  return `${r >= 10 ? r.toFixed(0) : r.toFixed(1)}:1`
}

export function fmtGen(g?: string): string {
  // 20260703T181858Z -> 2026-07-03 18:18Z
  if (!g || g.length < 16) return g ?? '—'
  return `${g.slice(0, 4)}-${g.slice(4, 6)}-${g.slice(6, 8)} ${g.slice(9, 11)}:${g.slice(11, 13)}Z`
}

export function fmtTime(ts: string): string {
  return new Date(ts).toLocaleTimeString()
}
