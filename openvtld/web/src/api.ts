// Thin REST helpers for the mutating v0.4 endpoints. Errors surface as
// thrown Error with the server's message so views can toast them.
import type {
  APIKey, AuditEntry, BlockDevice, BucketObject, CatalogEntry, DriveModel, Job, JobEvent, ExportChunk,
  LibraryModel, LibraryRow, LicenseInfo, LoggedEvent, Pool, RebuildResult, Remote, Role,
  Settings, SystemInfo, SystemStatus, TargetsView, UpdateLaunched, UpdateStatus, User,
} from './types'

// A 401 mid-session means the session expired (or was revoked): tell
// the auth layer so the login screen comes back. Login itself handles
// its own 401s.
export function broadcastAuthRequired() {
  window.dispatchEvent(new Event('auth:required'))
}

async function req<T>(url: string, init?: RequestInit): Promise<T> {
  const r = await fetch(url, {
    // FormData must set its own multipart boundary — never force JSON on it.
    headers: init?.body && !(init.body instanceof FormData) ? { 'Content-Type': 'application/json' } : undefined,
    ...init,
  })
  const body = await r.json().catch(() => ({}))
  if (r.status === 401 && url !== '/api/auth/login') broadcastAuthRequired()
  if (!r.ok) throw new Error((body as { error?: string }).error ?? `${r.status} ${r.statusText}`)
  return body as T
}

export const api = {
  jobs: (limit = 100) => req<Job[]>(`/api/jobs?limit=${limit}`),
  searchJobs: (q: string, limit = 500) =>
    req<Job[]>(`/api/jobs/search?q=${encodeURIComponent(q)}&limit=${limit}`),
  job: (id: number) =>
    req<{ job: Job; events: JobEvent[]; chunks: ExportChunk[] | null }>(`/api/jobs/${id}`),
  retryJob: (id: number) => req<Job>(`/api/jobs/${id}/retry`, { method: 'POST' }),
  cancelJob: (id: number) => req<{ ok: boolean }>(`/api/jobs/${id}/cancel`, { method: 'POST' }),

  exportCart: (label: string, remoteId: number) =>
    req<Job>(`/api/cartridges/${label}/export`, { method: 'POST', body: JSON.stringify({ remote_id: remoteId }) }),
  evictCart: (label: string, remoteId: number, generation?: string) =>
    req<Job>(`/api/cartridges/${label}/evict`, { method: 'POST', body: JSON.stringify({ remote_id: remoteId, generation }) }),
  importCart: (label: string, remoteId: number, generation: string, systemName?: string, targetLibrary?: number) =>
    req<Job>(`/api/cartridges/${label}/import`, {
      method: 'POST',
      body: JSON.stringify({ remote_id: remoteId, generation, system_name: systemName, target_library: targetLibrary }),
    }),

  mintCart: (library: number, label: string, count = 1) =>
    req<{ library: number; size_gb: number; created: { label: string; slot: number }[] }>('/api/cartridges', {
      method: 'POST', body: JSON.stringify({ library, label, count }),
    }),
  deleteCart: (label: string) =>
    req<{ ok: boolean; label: string }>(`/api/cartridges/${label}`, {
      method: 'DELETE', body: JSON.stringify({ confirm: label }),
    }),

  loadDrive: (library: number, index: number, label: string) =>
    req<{ ok: boolean; detail: string }>(`/api/libraries/${library}/drives/${index}/load`, { method: 'POST', body: JSON.stringify({ label }) }),
  unloadDrive: (library: number, index: number) =>
    req<{ ok: boolean; detail: string; slot: number }>(`/api/libraries/${library}/drives/${index}/unload`, { method: 'POST', body: JSON.stringify({}) }),

  remotes: () => req<Remote[]>('/api/remotes'),
  createRemote: (r: Record<string, unknown>) =>
    req<Remote>('/api/remotes', { method: 'POST', body: JSON.stringify(r) }),
  updateRemote: (id: number, r: Record<string, unknown>) =>
    req<Remote>(`/api/remotes/${id}`, { method: 'PUT', body: JSON.stringify(r) }),
  deleteRemote: (id: number) => req<{ ok: boolean }>(`/api/remotes/${id}`, { method: 'DELETE' }),
  testRemote: (id: number) =>
    req<{ ok: boolean; detail: string }>(`/api/remotes/${id}/test`, { method: 'POST' }),

  catalog: (remoteId: number) => req<CatalogEntry[]>(`/api/catalog?remote_id=${remoteId}`),
  rebuildCatalog: (remoteId: number) =>
    req<RebuildResult>('/api/catalog/rebuild', { method: 'POST', body: JSON.stringify({ remote_id: remoteId }) }),
  bucketObjects: (remoteId: number) =>
    req<{ objects: BucketObject[] }>(`/api/remotes/${remoteId}/objects`),
  deleteBucketPrefix: (remoteId: number, prefix: string) =>
    req<{ deleted: number }>(`/api/remotes/${remoteId}/objects`, { method: 'DELETE', body: JSON.stringify({ prefix }) }),

  settings: () => req<Settings>('/api/settings'),
  saveSettings: (s: Settings) => req<Settings>('/api/settings', { method: 'PUT', body: JSON.stringify(s) }),

  audit: () => req<AuditEntry[]>('/api/audit'),
  journal: () => req<LoggedEvent[]>('/api/events/recent'),
  searchJournal: (q: string, kind = 'all', limit = 500) =>
    req<LoggedEvent[]>(`/api/events/search?q=${encodeURIComponent(q)}&kind=${encodeURIComponent(kind)}&limit=${limit}`),

  // v0.5 auth + users
  me: () => req<{ user: User | null; setup_required?: boolean }>('/api/auth/me'),
  login: (username: string, password: string) =>
    req<{ user: User }>('/api/auth/login', { method: 'POST', body: JSON.stringify({ username, password }) }),
  logout: () => req<{ ok: boolean }>('/api/auth/logout', { method: 'POST' }),
  setup: (username: string, password: string) =>
    req<{ user: User }>('/api/auth/setup', { method: 'POST', body: JSON.stringify({ username, password }) }),

  // v0.6 libraries
  models: () =>
    req<{ libraries: LibraryModel[]; drives: DriveModel[] }>('/api/models'),
  libraryRows: () => req<LibraryRow[]>('/api/libraries'),
  nextLabel: (id: number) =>
    req<{ label: string; prefix: string; suffix: string }>(`/api/libraries/${id}/next-label`),
  createLibrary: (p: { name?: string; product: string; drive_product: string; num_drives: number; num_slots?: number; num_map?: number; label_prefix: string; pool_id: number }) =>
    req<{ library: number; serial: string; state: string; note: string }>('/api/libraries', {
      method: 'POST', body: JSON.stringify(p),
    }),
  deleteLibrary: (id: number, confirm: string) =>
    req<{ ok: boolean; cartridges_deleted: number; rebooting: boolean }>(`/api/libraries/${id}`, {
      method: 'DELETE', body: JSON.stringify({ confirm, acknowledge: 'I understand' }),
    }),
  applyLibraries: () =>
    req<{ steps: string[]; fc: { OK: boolean; Detail: string } }>('/api/libraries/apply', {
      method: 'POST', body: JSON.stringify({ confirm: 'apply' }),
    }),

  // v0.7 ZFS storage
  devices: () =>
    req<{ devices: BlockDevice[]; system: SystemStatus }>('/api/devices'),
  pools: () => req<Pool[]>('/api/pools'),
  recoverLibrary: (p: { remote_id: number; system_name: string; library_serial: string; pool_id: number; name?: string }) =>
    req<{ library: number; serial: string; name: string; carts: number; import_jobs: number[] }>(
      '/api/libraries/recover', { method: 'POST', body: JSON.stringify(p) }),
  createPool: (name: string) =>
    req<{ pool_id: number }>('/api/pools', { method: 'POST', body: JSON.stringify({ name }) }),
  removePool: (id: number, confirm: string) =>
    req<{ ok: boolean; pool_id: number }>(`/api/pools/${id}`, { method: 'DELETE', body: JSON.stringify({ confirm }) }),
  teardownStorage: (confirm: string) =>
    req<SystemStatus>('/api/storage/teardown', { method: 'POST', body: JSON.stringify({ confirm }) }),
  growStorage: () =>
    req<{ before_bytes: number; after_bytes: number; grew: boolean; system: SystemStatus }>(
      '/api/storage/grow', { method: 'POST', body: '{}' }),
  rescanDevices: () =>
    req<{ scsi_hosts: number; devices: BlockDevice[]; system: SystemStatus }>(
      '/api/storage/rescan', { method: 'POST', body: '{}' }),
  setupStorage: (dataDevs: string[], dedupDev: string, confirm: string) =>
    req<SystemStatus>('/api/storage/setup', {
      method: 'POST', body: JSON.stringify({ data_devs: dataDevs, dedup_dev: dedupDev, confirm }),
    }),

  // v0.9 — in-UI updates (upload + detached apply; UI rides out the restart)
  updateStatus: () => req<UpdateStatus>('/api/system/update-status'),
  uploadUpdate: (file: File, force = false) => {
    const fd = new FormData()
    fd.append('bundle', file)
    if (force) fd.append('force', '1')
    return req<UpdateLaunched>('/api/system/update', { method: 'POST', body: fd })
  },
  rollbackUpdate: () =>
    req<UpdateLaunched>('/api/system/rollback', { method: 'POST', body: JSON.stringify({ confirm: 'rollback' }) }),

  targets: () => req<TargetsView>('/api/targets'),
  addACL: (p: { wwpn: string; alias: string; ports?: string[]; libraries?: number[] }) =>
    req<TargetsView>('/api/targets/acls', { method: 'POST', body: JSON.stringify(p) }),
  // ports/libraries: null = all (including future ones); [] = none.
  updateACL: (wwpn: string, p: { alias: string; ports: string[] | null; libraries: number[] | null }) =>
    req<TargetsView>(`/api/targets/acls/${encodeURIComponent(wwpn)}`,
      { method: 'PUT', body: JSON.stringify(p) }),
  removeACL: (wwpn: string, force = false) =>
    req<TargetsView>(
      `/api/targets/acls/${encodeURIComponent(wwpn)}${force ? '?force=1' : ''}`,
      { method: 'DELETE' }),
  setPortServing: (wwn: string, serving: boolean) =>
    req<TargetsView>('/api/targets/ports', { method: 'PUT', body: JSON.stringify({ wwn, serving }) }),

  // v0.7 system control + API keys
  system: () => req<SystemInfo>('/api/system'),
  license: () => req<LicenseInfo>('/api/license'),
  ackLicense: () => req<{ ok: boolean; fingerprint: string }>('/api/license/ack', { method: 'POST' }),
  // Support bundle is a binary download, not JSON — fetch → blob → save.
  downloadSupportBundle: async () => {
    const r = await fetch('/api/system/support-bundle')
    if (!r.ok) {
      const b = await r.json().catch(() => ({}))
      throw new Error((b as { error?: string }).error ?? `${r.status} ${r.statusText}`)
    }
    const cd = r.headers.get('Content-Disposition') || ''
    const m = cd.match(/filename="([^"]+)"/)
    const name = m ? m[1] : 'openvtl-support.tar.gz'
    const url = URL.createObjectURL(await r.blob())
    const a = document.createElement('a')
    a.href = url; a.download = name
    document.body.appendChild(a); a.click(); a.remove()
    URL.revokeObjectURL(url)
  },
  restart: (mode: 'graceful' | 'immediate') =>
    req<{ ok: boolean; detail?: string; draining?: boolean; active_jobs?: number }>('/api/system/restart', {
      method: 'POST', body: JSON.stringify({ mode, confirm: 'restart' }),
    }),
  dataplaneRestart: () =>
    req<{ steps: string[]; fc: { OK: boolean; Detail: string } }>('/api/system/dataplane-restart', {
      method: 'POST', body: JSON.stringify({ confirm: 'restart' }),
    }),
  rebootHost: () =>
    req<{ ok: boolean; detail: string }>('/api/system/reboot', {
      method: 'POST', body: JSON.stringify({ confirm: 'reboot' }),
    }),
  apiKeys: () => req<APIKey[]>('/api/apikeys'),
  createAPIKey: (name: string, role: Role) =>
    req<{ id: number; name: string; role: Role; token: string }>('/api/apikeys', {
      method: 'POST', body: JSON.stringify({ name, role }),
    }),
  deleteAPIKey: (id: number) => req<{ ok: boolean }>(`/api/apikeys/${id}`, { method: 'DELETE' }),

  users: () => req<User[]>('/api/users'),
  createUser: (username: string, password: string, role: Role) =>
    req<User>('/api/users', { method: 'POST', body: JSON.stringify({ username, password, role }) }),
  updateUser: (id: number, patch: { role?: Role; disabled?: boolean }) =>
    req<User>(`/api/users/${id}`, { method: 'PUT', body: JSON.stringify(patch) }),
  setPassword: (id: number, password: string) =>
    req<{ ok: boolean }>(`/api/users/${id}/password`, { method: 'PUT', body: JSON.stringify({ password }) }),
  deleteUser: (id: number) => req<{ ok: boolean }>(`/api/users/${id}`, { method: 'DELETE' }),
}
