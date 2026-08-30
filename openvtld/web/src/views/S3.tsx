import { useEffect, useState } from 'react'
import { api } from '../api'
import { Badge, Button, Field, Panel, inputCls, notify } from '../components/ui'
import type { BucketObject, CatalogEntry, LibraryRow, Pool, RebuildResult, Remote } from '../types'
import { fmtBytes, fmtGen } from '../types'

export default function S3({ goto }: { goto: (v: string) => void }) {
  const [remotes, setRemotes] = useState<Remote[]>([])
  const [adding, setAdding] = useState(false)

  const reload = () => api.remotes().then(setRemotes).catch((e: Error) => notify(e.message, true))
  useEffect(() => { reload() }, [])

  return (
    <div className="space-y-6">
      <Panel
        title="S3 remotes"
        right={<Button kind="primary" needsAdmin onClick={() => setAdding(!adding)}>{adding ? 'Close' : 'Add remote'}</Button>}
      >
        {adding && <RemoteForm onDone={() => { setAdding(false); reload() }} />}
        {remotes.length === 0 && !adding && (
          <p className="text-sm text-zinc-600">No remotes yet. Add one to keep exported cartridges offsite.</p>
        )}
        <div className="space-y-3">
          {remotes.map((r) => <RemoteCard key={r.id} r={r} reload={reload} goto={goto} />)}
        </div>
      </Panel>
    </div>
  )
}

function RemoteCard({ r, reload, goto }: { r: Remote; reload: () => void; goto: (v: string) => void }) {
  const [editing, setEditing] = useState(false)
  const [testing, setTesting] = useState(false)
  const [showCatalog, setShowCatalog] = useState(false)
  const [showRaw, setShowRaw] = useState(false)

  const test = () => {
    setTesting(true)
    api.testRemote(r.id)
      .then((res) => notify(res.ok ? `✓ ${res.detail}` : res.detail, !res.ok))
      .catch((e: Error) => notify(e.message, true))
      .finally(() => { setTesting(false); reload() })
  }
  const del = () => {
    if (!window.confirm(`Delete remote "${r.name}"? Nothing in the bucket is touched.`)) return
    api.deleteRemote(r.id).then(reload).catch((e: Error) => notify(e.message, true))
  }

  return (
    <div className="rounded-lg border border-zinc-800 bg-zinc-900/60 p-4">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <div className="flex items-center gap-3">
          <span className="text-sm font-semibold text-zinc-100">{r.name}</span>
          <span className="font-mono text-xs text-zinc-500">s3://{r.bucket}{r.prefix ? `/${r.prefix}` : ''} · {r.region || r.endpoint}</span>
          {r.last_test_ok !== undefined && r.last_test_at && (
            <Badge ok={!!r.last_test_ok}
              label={r.last_test_ok ? 'reachable' : 'unreachable'}
              title={`${r.last_test_detail} (${new Date(r.last_test_at).toLocaleString()})`} />
          )}
        </div>
        <div className="flex gap-2">
          <Button needsAdmin onClick={test} disabled={testing}>{testing ? 'testing…' : 'Test connection'}</Button>
          <Button onClick={() => setShowCatalog(!showCatalog)}>{showCatalog ? 'Hide catalog' : 'Catalog'}</Button>
          <Button onClick={() => setShowRaw(!showRaw)}>{showRaw ? 'Hide raw' : 'Raw'}</Button>
          <Button needsAdmin onClick={() => setEditing(!editing)}>{editing ? 'Close' : 'Edit'}</Button>
          <Button kind="danger" needsAdmin onClick={del}>Delete</Button>
        </div>
      </div>
      {editing && <RemoteForm existing={r} onDone={() => { setEditing(false); reload() }} />}
      {showCatalog && <CatalogBrowser remote={r} goto={goto} />}
      {showRaw && <RawBrowser remote={r} />}
    </div>
  )
}

function RemoteForm({ existing, onDone }: { existing?: Remote; onDone: () => void }) {
  const [f, setF] = useState({
    name: existing?.name ?? '',
    endpoint: existing?.endpoint ?? 's3.amazonaws.com',
    region: existing?.region ?? '',
    bucket: existing?.bucket ?? '',
    prefix: existing?.prefix ?? '',
    access_key: existing?.access_key ?? '',
    secret_key: '',
    use_ssl: existing?.use_ssl ?? true,
    path_style: existing?.path_style ?? false,
  })
  const [busy, setBusy] = useState(false)
  const set = (k: string, v: string | boolean) => setF((prev) => ({ ...prev, [k]: v }))

  const save = () => {
    setBusy(true)
    const p = existing ? api.updateRemote(existing.id, f) : api.createRemote(f)
    p.then(() => { notify(existing ? 'Remote updated' : 'Remote created'); onDone() })
      .catch((e: Error) => notify(e.message, true))
      .finally(() => setBusy(false))
  }

  return (
    <div className="mt-3 grid grid-cols-2 gap-3 rounded border border-zinc-800 bg-zinc-950/60 p-4 md:grid-cols-3">
      <Field label="name"><input className={inputCls} value={f.name} onChange={(e) => set('name', e.target.value)} /></Field>
      <Field label="bucket"><input className={inputCls} value={f.bucket} onChange={(e) => set('bucket', e.target.value)} /></Field>
      <Field label="region"><input className={inputCls} value={f.region} placeholder="us-east-2" onChange={(e) => set('region', e.target.value)} /></Field>
      <Field label="endpoint"><input className={inputCls} value={f.endpoint} onChange={(e) => set('endpoint', e.target.value)} /></Field>
      <Field label="key prefix (optional)"><input className={inputCls} value={f.prefix} onChange={(e) => set('prefix', e.target.value)} /></Field>
      <div className="flex items-end gap-4 pb-1 text-xs text-zinc-400">
        <label className="flex items-center gap-1.5">
          <input type="checkbox" checked={f.use_ssl} onChange={(e) => set('use_ssl', e.target.checked)} /> TLS
        </label>
        <label className="flex items-center gap-1.5">
          <input type="checkbox" checked={f.path_style} onChange={(e) => set('path_style', e.target.checked)} /> path-style
        </label>
      </div>
      <Field label="access key"><input className={inputCls} value={f.access_key} onChange={(e) => set('access_key', e.target.value)} /></Field>
      <Field label={existing ? 'secret key (blank = keep current)' : 'secret key'}>
        <input className={inputCls} type="password" value={f.secret_key} autoComplete="new-password"
          onChange={(e) => set('secret_key', e.target.value)} />
      </Field>
      <div className="flex items-end justify-end gap-2">
        <Button kind="primary" needsAdmin disabled={busy} onClick={save}>{existing ? 'Save' : 'Create'}</Button>
      </div>
    </div>
  )
}

// CatalogBrowser: what the bucket says exists — the catalog of record —
// grouped by cartridge, with import + rebuild actions.
function CatalogBrowser({ remote, goto }: { remote: Remote; goto: (v: string) => void }) {
  const [entries, setEntries] = useState<CatalogEntry[]>([])
  const [rebuild, setRebuild] = useState<RebuildResult | null>(null)
  const [busy, setBusy] = useState(false)
  const [libs, setLibs] = useState<LibraryRow[]>([]) // all libraries (serial match + import target)
  const [pools, setPools] = useState<Pool[]>([])
  const [target, setTarget] = useState(0)

  const load = () => api.catalog(remote.id).then(setEntries).catch((e: Error) => notify(e.message, true))
  const loadLibs = () =>
    api.libraryRows()
      .then((rows) => {
        setLibs(rows)
        const active = rows.filter((l) => l.state === 'active')
        setTarget((t) => (t && active.some((l) => l.id === t) ? t : active[0]?.id ?? 0))
      })
      .catch((e: Error) => notify(e.message, true))
  useEffect(() => { load() }, [remote.id]) // eslint-disable-line react-hooks/exhaustive-deps
  useEffect(() => {
    loadLibs()
    api.pools().then(setPools).catch((e: Error) => notify(e.message, true))
  }, [remote.id]) // eslint-disable-line react-hooks/exhaustive-deps
  const activeLibs = libs.filter((l) => l.state === 'active')

  const doRebuild = () => {
    setBusy(true)
    api.rebuildCatalog(remote.id)
      .then((res) => {
        setRebuild(res)
        notify(`Bucket catalog refreshed: ${res.complete} manifests${res.incomplete.length ? `, ${res.incomplete.length} incomplete` : ''}`)
        load()
      })
      .catch((e: Error) => notify(e.message, true))
      .finally(() => setBusy(false))
  }

  const doImport = (e: CatalogEntry) => {
    if (!target) { notify('No active library to import into — create and activate a library first', true); return }
    const lib = activeLibs.find((l) => l.id === target)
    if (!window.confirm(`Import ${e.cart_label} (generation ${fmtGen(e.generation)}) into library ${lib?.name ?? target}? The cartridge keeps its name; if ${e.cart_label} already exists here the import is refused.`)) return
    api.importCart(e.cart_label, remote.id, e.generation, e.system_name, target)
      .then(() => { notify(`Import job started for ${e.cart_label}`); goto('jobs') })
      .catch((err: Error) => notify(err.message, true))
  }

  // Recover a whole library from its topology: recreate it (original serial)
  // on a free pool, then import all its carts. One-click DR (Phase B).
  const doRecover = (system: string, serial: string, cartCount: number) => {
    const free = pools.filter((p) => p.state === 'active' && !libs.some((l) => l.home_pool === p.id))
    if (free.length === 0) { notify('No free active pool to recover onto — create or free a pool first (Storage)', true); return }
    let pool = free[0]
    if (free.length > 1) {
      const pick = window.prompt(`Recover onto which pool? Available: ${free.map((p) => p.name).join(', ')}`, free[0].name)
      if (pick === null) return
      const found = free.find((p) => p.name === pick.trim())
      if (!found) { notify(`No free pool named "${pick}"`, true); return }
      pool = found
    }
    if (!window.confirm(`Recover library ${serial} onto pool ${pool.name}? This recreates the library and imports ${cartCount} cartridge(s), each keeping its name. Applying restarts mhVTL (any host sessions drop briefly); re-point the IBM i (RSC/DEVD) at the recovered library afterward.`)) return
    api.recoverLibrary({ remote_id: remote.id, system_name: system, library_serial: serial, pool_id: pool.id })
      .then((res) => { notify(`Recovering ${res.name}: library created, importing ${res.carts} cartridge(s)`); loadLibs(); goto('jobs') })
      .catch((err: Error) => notify(err.message, true))
  }

  // Group by system, then by cart — a shared bucket reads as
  // "<system> · <library> · <tape>" (v0.7 S3 namespacing).
  const bySystem = new Map<string, CatalogEntry[]>()
  for (const e of entries) {
    const k = e.system_name || '(unnamed)'
    bySystem.set(k, [...(bySystem.get(k) ?? []), e])
  }

  return (
    <div className="mt-3 rounded border border-zinc-800 bg-zinc-950/60 p-4">
      <div className="mb-3 flex flex-wrap items-center justify-between gap-2">
        <span className="text-xs text-zinc-500">
          The bucket is the source of truth — Refresh bucket catalog re-reads it from scratch. Import restores a cartridge into the chosen library, keeping its name.
        </span>
        <div className="flex items-center gap-2">
          {activeLibs.length > 0 && (
            <label className="flex items-center gap-1 text-xs text-zinc-400">
              Import into
              <select className={inputCls} value={target} onChange={(ev) => setTarget(Number(ev.target.value))}>
                {activeLibs.map((l) => <option key={l.id} value={l.id}>{l.name}</option>)}
              </select>
            </label>
          )}
          <Button needsAdmin onClick={doRebuild} disabled={busy}>{busy ? 'refreshing…' : 'Refresh bucket catalog'}</Button>
        </div>
      </div>
      {entries.length > 0 && activeLibs.length === 0 && (
        <div className="mb-3 rounded border border-sky-800/50 bg-sky-950/30 px-3 py-2 text-xs text-sky-300">
          These cartridges are stored in the bucket but there's no active library here. Use <span className="font-semibold">Recover library</span> below to recreate a library from its saved layout and pull its cartridges back — or create one manually (Libraries), then Import. Refresh bucket catalog only re-reads this list.
        </div>
      )}
      {rebuild && rebuild.incomplete && rebuild.incomplete.length > 0 && (
        <div className="mb-3 rounded border border-amber-700/50 bg-amber-500/10 px-3 py-2 text-xs text-amber-300">
          Incomplete exports (no manifest — aborted uploads):{' '}
          {rebuild.incomplete.map((i) => `${i.system}/${i.library}/${i.label}/${i.generation}`).join(', ')}
        </div>
      )}
      {rebuild?.errors && rebuild.errors.length > 0 && (
        <div className="mb-3 rounded border border-red-800/50 bg-red-950/40 px-3 py-2 text-xs text-red-300">
          {rebuild.errors.join('; ')}
        </div>
      )}
      {bySystem.size === 0 && <p className="text-xs text-zinc-600">No exports in this bucket yet.</p>}
      <div className="space-y-4">
        {Array.from(bySystem.entries()).map(([system, sysEntries]) => {
          const byLib = new Map<string, CatalogEntry[]>()
          for (const e of sysEntries) byLib.set(e.library_serial || '—', [...(byLib.get(e.library_serial || '—') ?? []), e])
          return (
            <div key={system}>
              <div className="mb-1 text-[11px] uppercase tracking-wide text-sky-400">system · {system}</div>
              <div className="space-y-3 border-l border-zinc-800 pl-3">
                {Array.from(byLib.entries()).map(([serial, libEntries]) => {
                  const byCart = new Map<string, CatalogEntry[]>()
                  for (const e of libEntries) byCart.set(e.cart_label, [...(byCart.get(e.cart_label) ?? []), e])
                  const present = libs.some((l) => l.serial === serial)
                  return (
                    <div key={serial}>
                      <div className="mb-1 flex flex-wrap items-center gap-2">
                        <span className="text-[11px] uppercase tracking-wide text-emerald-400">library · {serial}</span>
                        {present
                          ? <span className="text-[10px] text-zinc-600">on this appliance</span>
                          : <Button needsAdmin onClick={() => doRecover(system, serial, byCart.size)}>Recover library</Button>}
                      </div>
                      <div className="space-y-3 border-l border-zinc-800 pl-3">
                        {Array.from(byCart.entries()).map(([label, list]) => (
                          <div key={label}>
                            <div className="mb-1 font-mono text-sm text-amber-300">{label}</div>
                            <div className="space-y-1">
                              {list.map((e) => (
                                <div key={e.generation}
                                  className="flex flex-wrap items-center justify-between gap-2 rounded bg-zinc-900/70 px-3 py-1.5 text-xs">
                                  <span className="font-mono text-zinc-300">{fmtGen(e.generation)}</span>
                                  <span className="font-mono text-zinc-500">
                                    {fmtBytes(e.logical_bytes)} logical → {fmtBytes(e.stored_bytes)} stored · {e.chunk_count} chunk{e.chunk_count === 1 ? '' : 's'}
                                  </span>
                                  <Button needsAdmin onClick={() => doImport(e)}>Import</Button>
                                </div>
                              ))}
                            </div>
                          </div>
                        ))}
                      </div>
                    </div>
                  )
                })}
              </div>
            </div>
          )
        })}
      </div>
    </div>
  )
}

// --- Raw bucket browser (folder-level delete for admins) ---

interface TreeNode {
  name: string
  path: string // relative prefix; folders end with '/', files are the full key
  size: number
  count: number
  isFolder: boolean
  children: Map<string, TreeNode>
}

function buildTree(objects: BucketObject[]): TreeNode {
  const root: TreeNode = { name: '', path: '', size: 0, count: 0, isFolder: true, children: new Map() }
  for (const o of objects) {
    const parts = o.key.split('/')
    let node = root
    let prefix = ''
    parts.forEach((part, i) => {
      const leaf = i === parts.length - 1
      prefix += leaf ? part : part + '/'
      let child = node.children.get(part)
      if (!child) {
        child = { name: part, path: prefix, size: 0, count: 0, isFolder: !leaf, children: new Map() }
        node.children.set(part, child)
      }
      if (leaf) { child.size = o.size; child.count = 1; child.isFolder = false }
      node = child
    })
  }
  const rollup = (n: TreeNode): [number, number] => {
    if (!n.isFolder) return [n.size, 1]
    let s = 0, c = 0
    n.children.forEach((ch) => { const [cs, cc] = rollup(ch); s += cs; c += cc })
    n.size = s; n.count = c
    return [s, c]
  }
  rollup(root)
  return root
}

function sortNodes(a: TreeNode, b: TreeNode) {
  if (a.isFolder !== b.isFolder) return a.isFolder ? -1 : 1
  return a.name.localeCompare(b.name)
}

function BucketNode({ node, depth, onDelete, busy }: {
  node: TreeNode; depth: number; onDelete: (path: string, count: number) => void; busy: boolean
}) {
  const [open, setOpen] = useState(depth < 1)
  const pad = { paddingLeft: depth * 14 }
  if (!node.isFolder) {
    return (
      <div className="flex items-center gap-2 py-0.5 text-zinc-500" style={pad}>
        <span>{node.name}</span>
        <span className="text-zinc-700">{fmtBytes(node.size)}</span>
      </div>
    )
  }
  const kids = Array.from(node.children.values()).sort(sortNodes)
  return (
    <div>
      <div className="flex items-center gap-2 py-0.5" style={pad}>
        <button onClick={() => setOpen(!open)} className="text-zinc-300 hover:text-zinc-100">
          {open ? '▾' : '▸'} {node.name}/
        </button>
        <span className="text-zinc-600">{node.count} obj · {fmtBytes(node.size)}</span>
        <Button kind="danger" needsAdmin disabled={busy}
          title="delete this folder and everything under it" onClick={() => onDelete(node.path, node.count)}>
          Delete
        </Button>
      </div>
      {open && kids.map((n) => <BucketNode key={n.path} node={n} depth={depth + 1} onDelete={onDelete} busy={busy} />)}
    </div>
  )
}

// RawBrowser: the actual objects in the bucket, as a folder tree. Admins
// can delete whole folders (system / library / tape / generation); a lone
// chunk or manifest can't be removed here — that gets dirty fast.
function RawBrowser({ remote }: { remote: Remote }) {
  const [objects, setObjects] = useState<BucketObject[]>([])
  const [busy, setBusy] = useState(false)

  const load = () =>
    api.bucketObjects(remote.id).then((res) => setObjects(res.objects ?? [])).catch((e: Error) => notify(e.message, true))
  useEffect(() => { load() }, [remote.id]) // eslint-disable-line react-hooks/exhaustive-deps

  const del = (path: string, count: number) => {
    if (!window.confirm(`Delete folder "${path}" and all ${count} object(s) under it from s3://${remote.bucket}? This cannot be undone.`)) return
    setBusy(true)
    api.deleteBucketPrefix(remote.id, path)
      .then((res) => { notify(`deleted ${res.deleted} object(s)`); load() })
      .catch((e: Error) => notify(e.message, true))
      .finally(() => setBusy(false))
  }

  const top = Array.from(buildTree(objects).children.values()).sort(sortNodes)
  return (
    <div className="mt-3 rounded border border-zinc-800 bg-zinc-950/60 p-4">
      <div className="mb-2 flex items-center justify-between">
        <span className="text-xs text-zinc-500">
          Raw bucket contents. Admins can delete whole folders; individual chunks and manifests can’t be removed here.
        </span>
        <Button onClick={load} disabled={busy}>Refresh</Button>
      </div>
      {top.length === 0
        ? <p className="text-xs text-zinc-600">Nothing in the bucket under this remote’s prefix.</p>
        : <div className="font-mono text-xs">{top.map((n) => <BucketNode key={n.path} node={n} depth={0} onDelete={del} busy={busy} />)}</div>}
    </div>
  )
}
