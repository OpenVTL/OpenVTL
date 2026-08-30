import { useCallback, useEffect, useState } from 'react'
import { api } from '../api'
import { Button, Field, Panel, inputCls, notify } from '../components/ui'
import type { VTL } from '../useVTL'
import type { BlockDevice, LibraryRow, Pool, PoolStats, SystemStatus } from '../types'
import { fmtBytes } from '../types'

// Storage view (v0.7, ZFS): one system pool (data disks + one SSD dedupe
// device) holds every pool as a dataset, so deduplication is global. Set
// up the system pool once, then create pools by name — no per-pool disks,
// no cache slice. Eligibility is decided server-side; there is no force
// path past a disk that holds data.

// What the kernel's rotational flag is worth. Real HBAs report it honestly;
// virtio does not — a hypervisor that never sets the SSD hint (IBM Cloud VPC,
// EC2, GCE) shows rotational=1 on all-flash volumes, so say "virtio" rather
// than assert a spindle that isn't there.
const diskClass = (d: BlockDevice) =>
  !d.rotational ? 'ssd' : d.transport === 'virtio' ? 'virtio' : 'hdd'

export default function Storage({ vtl }: { vtl: VTL; goto: (v: string) => void }) {
  const [devices, setDevices] = useState<BlockDevice[]>([])
  const [system, setSystem] = useState<SystemStatus | null>(null)
  const [pools, setPools] = useState<Pool[]>([])
  const [libs, setLibs] = useState<LibraryRow[]>([])
  const [scanning, setScanning] = useState(false)

  const reload = useCallback(() => {
    api.devices().then((r) => { setDevices(r.devices); setSystem(r.system) }).catch(() => {})
    api.pools().then(setPools).catch(() => {})
    api.libraryRows().then(setLibs).catch(() => {})
  }, [])

  const scan = () => {
    setScanning(true)
    api.rescanDevices()
      .then((r) => {
        setDevices(r.devices); setSystem(r.system)
        notify(`scanned ${r.scsi_hosts} SCSI host(s) — ${r.devices.length} block device(s) enumerated`)
      })
      .catch((e: Error) => notify(e.message, true))
      .finally(() => setScanning(false))
  }
  useEffect(() => {
    reload()
    const t = window.setInterval(reload, 10_000)
    return () => window.clearInterval(t)
  }, [reload])

  const liveStats = new Map(vtl.snap?.pools.map((p) => [p.name, p]) ?? [])

  return (
    <div className="space-y-6">
      <Panel title="Storage pools" right={
        <span className="text-xs text-zinc-600">{pools.length || 'no'} pool{pools.length === 1 ? '' : 's'}</span>
      }>
        {pools.length === 0 ? (
          <p className="text-sm text-zinc-600">
            {system?.ready
              ? 'No pools yet. Create your first pool below — it becomes a deduplicated dataset on the system storage.'
              : 'Set up system storage below, then create your first pool.'}
          </p>
        ) : (
          <div className="grid grid-cols-1 gap-3 md:grid-cols-2">
            {pools.map((p) => (
              <PoolCard key={p.id} pool={p} live={liveStats.get(p.name)}
                pairedLib={libs.find((l) => l.home_pool === p.id)}
                onChanged={reload} />
            ))}
          </div>
        )}
      </Panel>

      <SystemStoragePanel devices={devices} system={system} canTeardown={pools.length === 0} onChanged={reload} />
      {system?.ready && <ProvisionPanel onCreated={reload} />}
      <DevicesPanel devices={devices} scanning={scanning} onScan={scan} />
    </div>
  )
}

function KV({ k, v }: { k: string; v: string }) {
  return (
    <div className="flex justify-between gap-4">
      <dt className="text-zinc-500">{k}</dt>
      <dd className="text-right font-mono text-zinc-300">{v}</dd>
    </div>
  )
}

// PoolCard shows one pool's dedupe/compression and its guarded removal
// control. A paired library must be deleted first (1:1 model).
function PoolCard({ pool: p, live, pairedLib, onChanged }: {
  pool: Pool; live?: PoolStats; pairedLib?: LibraryRow; onChanged: () => void
}) {
  const [confirm, setConfirm] = useState('')
  const [open, setOpen] = useState(false)
  const [busy, setBusy] = useState(false)
  const busyState = p.state === 'creating' || p.state === 'removing'
  const blockReason = pairedLib
    ? `Delete the library "${pairedLib.name}" first — it lives on this pool.`
    : busyState ? 'Pool is busy.' : ''

  const remove = () => {
    setBusy(true)
    api.removePool(p.id, confirm)
      .then(() => {
        notify(`pool ${p.name} removed`)
        setOpen(false); setConfirm(''); onChanged()
      })
      .catch((e: Error) => notify(e.message, true))
      .finally(() => setBusy(false))
  }

  return (
    <div className={`rounded-lg border px-4 py-3 ${
      p.state === 'error' ? 'border-red-800/70 bg-red-950/20'
        : busyState ? 'border-amber-700/60 bg-amber-500/5'
          : 'border-zinc-800 bg-zinc-900/60'
    }`}>
      <div className="flex items-center justify-between">
        <span className="font-mono text-sm text-zinc-200">{p.name}</span>
        <span className={`text-xs font-semibold uppercase ${
          p.state === 'active' ? 'text-emerald-400' : p.state === 'error' ? 'text-red-400' : 'text-amber-300'
        }`}>{p.state}</span>
      </div>
      <div className="mt-1 font-mono text-[11px] text-zinc-500">{p.mountpoint}</div>
      {p.state === 'error' && p.detail && (
        <div className="mt-2 text-xs text-red-300">{p.detail}</div>
      )}
      <dl className="mt-2 space-y-1 text-xs">
        {live && <KV k="logical stored" v={fmtBytes(live.logical_bytes)} />}
        {live && (
          <div className="flex justify-between gap-4" title="estimate: dataset used ÷ global dedupe ratio — dedupe savings are pool-wide, so an exact per-pool figure doesn't exist">
            <dt className="text-zinc-500">actual on disk (est.)</dt>
            <dd className="text-right font-mono text-zinc-300">{fmtBytes(live.phys_est_bytes ?? live.vdo_used_bytes)}</dd>
          </div>
        )}
        {live && <KV k="compression" v={live.compress_ratio ? `${live.compress_ratio.toFixed(2)}×` : '—'} />}
        {live && <KV k="dedupe (global)" v={live.dedup_ratio ? `${live.dedup_ratio.toFixed(2)}×` : '—'} />}
        {live && (live.record_bytes ?? 0) > 0 && <KV k="dedupe granularity" v={fmtBytes(live.record_bytes!)} />}
      </dl>

      <div className="mt-3 border-t border-zinc-800/70 pt-2">
        {!open ? (
          <div className="flex items-center justify-end gap-2">
            {blockReason && <span className="text-[11px] text-zinc-600">{blockReason}</span>}
            <Button needsAdmin disabled={!!pairedLib || busyState}
              title={blockReason || 'Remove this pool'} onClick={() => setOpen(true)}>
              Remove pool
            </Button>
          </div>
        ) : (
          <div className="space-y-2">
            <p className="text-xs text-zinc-400">
              This deletes the deduplicated data on <span className="font-mono text-zinc-300">{p.name}</span>.
              {' '}The system storage and its disks are untouched — to free the disks, use “Tear down storage”
              once every pool is removed. This cannot be undone.
            </p>
            <div className="flex items-end gap-2">
              <Field label={`type the pool name to confirm (${p.name})`}>
                <input className={inputCls} value={confirm} placeholder={p.name} autoFocus
                  onChange={(e) => setConfirm(e.target.value)} />
              </Field>
              <Button kind="danger" needsAdmin disabled={busy || confirm !== p.name} onClick={remove}>
                {busy ? 'removing…' : 'Remove'}
              </Button>
              <Button disabled={busy} onClick={() => { setOpen(false); setConfirm('') }}>Cancel</Button>
            </div>
          </div>
        )}
      </div>
    </div>
  )
}

// SystemStoragePanel — the one-time storage foundation: data disk(s) for
// bulk capacity + one SSD to hold the global dedupe table. Every pool
// is a dataset on it.
function SystemStoragePanel({ devices, system, canTeardown, onChanged }: {
  devices: BlockDevice[]; system: SystemStatus | null; canTeardown: boolean; onChanged: () => void
}) {
  const [dataDevs, setDataDevs] = useState<string[]>([])
  const [dedup, setDedup] = useState('')
  const [confirm, setConfirm] = useState('')
  const [busy, setBusy] = useState(false)
  const [tdOpen, setTdOpen] = useState(false)
  const [tdConfirm, setTdConfirm] = useState('')
  const [growing, setGrowing] = useState(false)
  const eligible = devices.filter((d) => d.eligible && d.path !== '')
  // The dedupe device wants to be an SSD, but the kernel's rotational flag is
  // only a hint: virtualised transports (virtio on IBM Cloud/EC2/GCE) report
  // rotational=1 for every volume even when the backing store is all-flash,
  // which used to leave this picker empty and storage unbuildable. Offer every
  // eligible disk, flash-first, and label what we actually know.
  const dedupCandidates = [...eligible].sort((a, b) => Number(a.rotational) - Number(b.rotational))
  // The dedupe device is permanent: after the first setup it's remembered
  // across a teardown and can't be changed, so the picker is locked to it.
  const lockedDedup = system?.dedup_fixed ? system.dedup_dev : ''
  const dedupDev = lockedDedup || dedup

  const toggle = (path: string) =>
    setDataDevs((cur) => cur.includes(path) ? cur.filter((p) => p !== path) : [...cur, path])

  const setup = () => {
    setBusy(true)
    api.setupStorage(dataDevs.filter((d) => d !== dedupDev), dedupDev, confirm)
      .then(() => { notify('system storage created'); setDataDevs([]); setDedup(''); setConfirm(''); onChanged() })
      .catch((e: Error) => notify(e.message, true))
      .finally(() => setBusy(false))
  }

  const teardown = () => {
    setBusy(true)
    api.teardownStorage(tdConfirm)
      .then(() => { notify('system storage torn down — data disk(s) released to available'); setTdOpen(false); setTdConfirm(''); onChanged() })
      .catch((e: Error) => notify(e.message, true))
      .finally(() => setBusy(false))
  }

  const grow = () => {
    setGrowing(true)
    api.growStorage()
      .then((r) => {
        notify(r.grew
          ? `storage grown: ${fmtBytes(r.before_bytes)} → ${fmtBytes(r.after_bytes)}`
          : 'no new space to claim — enlarge a data or dedupe disk at the hypervisor first, then grow')
        onChanged()
      })
      .catch((e: Error) => notify(e.message, true))
      .finally(() => setGrowing(false))
  }

  if (system?.ready) {
    const cap = (alloc: number, size: number) => {
      const pct = size > 0 ? (100 * alloc) / size : 0
      return `${fmtBytes(alloc)} / ${fmtBytes(size)} (${pct.toFixed(1)}%)`
    }
    return (
      <Panel title="System storage">
        <dl className="space-y-1 text-xs">
          <KV k="pool" v={system.zpool} />
          <KV k="data disks" v={system.data_devs.join(', ') || '—'} />
          <KV k="dedupe device" v={system.dedup_dev || '—'} />
          <KV k="data capacity" v={cap(system.data_alloc_bytes, system.data_size_bytes)} />
          <KV k="dedupe capacity" v={cap(system.dedup_alloc_bytes, system.dedup_size_bytes)} />
          <KV k="dedupe ratio (global)" v={system.dedup_ratio ? `${system.dedup_ratio.toFixed(2)}×` : '—'} />
          {system.compression && <KV k="compression" v={system.compression} />}
        </dl>
        <p className="mt-2 text-xs text-zinc-600">
          Every pool is a deduplicated dataset on this storage. The dedupe table lives on the SSD, so lookups stay fast.
        </p>
        <div className="mt-3 border-t border-zinc-800/70 pt-2">
          {!tdOpen ? (
            <div className="flex items-center justify-between gap-2">
              <Button needsAdmin disabled={busy || growing} onClick={grow}
                title="After enlarging a data or dedupe disk at the hypervisor, extend the pool onto the new space">
                {growing ? 'growing…' : 'Grow storage'}
              </Button>
              <div className="flex items-center gap-2">
                {!canTeardown && <span className="text-[11px] text-zinc-600">remove all pools first</span>}
                <Button needsAdmin disabled={!canTeardown || busy || growing}
                  title={canTeardown ? 'Destroy the system storage and free its disks' : 'Remove all pools first'}
                  onClick={() => setTdOpen(true)}>
                  Tear down storage
                </Button>
              </div>
            </div>
          ) : (
            <div className="space-y-2">
              <p className="text-xs text-zinc-400">
                This destroys the system storage and erases its data disk(s), returning them to available. The dedupe
                SSD stays reserved as this system’s permanent metadata device. This cannot be undone.
              </p>
              <div className="flex items-end gap-2">
                <Field label={'confirm — type "teardown"'}>
                  <input className={inputCls} value={tdConfirm} placeholder="teardown" autoFocus
                    onChange={(e) => setTdConfirm(e.target.value)} />
                </Field>
                <Button kind="danger" needsAdmin disabled={busy || tdConfirm !== 'teardown'} onClick={teardown}>
                  {busy ? 'tearing down…' : 'Tear down'}
                </Button>
                <Button disabled={busy} onClick={() => { setTdOpen(false); setTdConfirm('') }}>Cancel</Button>
              </div>
            </div>
          )}
        </div>
      </Panel>
    )
  }

  return (
    <Panel title="Set up system storage">
      <p className="text-xs text-zinc-500">
        Pick disk(s) for bulk data and one fast SSD to hold the dedupe metadata. Selected disks are erased.
        {lockedDedup
          ? ' The dedupe device is fixed for this system — pick a data disk to bring storage back up.'
          : ' The dedupe device is chosen once and can’t be changed later without tearing down all storage.'}
      </p>
      <div className="mt-3 space-y-3">
        <div>
          <div className="mb-1 text-xs text-zinc-500">data disks</div>
          <div className="grid grid-cols-1 gap-1 md:grid-cols-2">
            {eligible.map((d) => (
              <label key={d.path} className={`flex cursor-pointer items-center gap-2 rounded border px-2 py-1 text-xs ${
                dataDevs.includes(d.path) ? 'border-sky-700 bg-sky-600/10' : 'border-zinc-800 bg-zinc-900/40'
              }`}>
                <input type="checkbox" checked={dataDevs.includes(d.path)} onChange={() => toggle(d.path)} />
                <span className="font-mono text-zinc-300">{d.path}</span>
                <span className="text-zinc-500">{fmtBytes(d.size_bytes)} · {diskClass(d)}</span>
              </label>
            ))}
            {eligible.length === 0 && <span className="text-xs text-zinc-600">no available disks</span>}
          </div>
        </div>
        <div className="grid grid-cols-1 items-end gap-3 md:grid-cols-[1fr_1fr_auto]">
          <Field label={lockedDedup ? 'dedupe metadata disk (fixed)' : 'dedupe metadata disk (flash)'}>
            {lockedDedup ? (
              <div className={`${inputCls} flex items-center text-zinc-400`}>
                <span className="font-mono text-zinc-300">{lockedDedup}</span>
                <span className="ml-2 text-[11px] text-zinc-600">· permanent</span>
              </div>
            ) : (
              <select className={inputCls} value={dedup} onChange={(e) => setDedup(e.target.value)}>
                <option value="">select a disk…</option>
                {dedupCandidates.map((d) => (
                  <option key={d.path} value={d.path}>{d.path} · {fmtBytes(d.size_bytes)} · {diskClass(d)}</option>
                ))}
              </select>
            )}
          </Field>
          <Field label={'confirm — type "create"'}>
            <input className={inputCls} value={confirm} placeholder="create"
              onChange={(e) => setConfirm(e.target.value)} />
          </Field>
          <Button kind="danger" needsAdmin
            disabled={busy || dataDevs.filter((d) => d !== dedupDev).length === 0 || !dedupDev || confirm !== 'create'}
            onClick={setup}>
            {busy ? 'creating…' : 'Create storage'}
          </Button>
        </div>
      </div>
    </Panel>
  )
}

function ProvisionPanel({ onCreated }: { onCreated: () => void }) {
  const [name, setName] = useState('')
  const [busy, setBusy] = useState(false)

  const create = () => {
    setBusy(true)
    api.createPool(name.trim())
      .then(() => { notify(`pool ${name.trim()} created`); setName(''); onCreated() })
      .catch((e: Error) => notify(e.message, true))
      .finally(() => setBusy(false))
  }

  return (
    <Panel title="Create a pool">
      <div className="grid grid-cols-1 items-end gap-3 md:grid-cols-[1fr_auto]">
        <Field label="pool name (a-z, 0-9, _ — no dashes)">
          <input className={inputCls} placeholder="pool1" value={name}
            onChange={(e) => setName(e.target.value.toLowerCase())} />
        </Field>
        <Button kind="primary" needsAdmin disabled={busy || !name} onClick={create}>
          {busy ? 'creating…' : 'Create pool'}
        </Button>
      </div>
      <p className="mt-2 text-xs text-zinc-600">
        Creates a deduplicated, compressed dataset on the system storage. Pair a library to it in Libraries.
      </p>
    </Panel>
  )
}

function DevicesPanel({ devices, scanning, onScan }: {
  devices: BlockDevice[]; scanning: boolean; onScan: () => void
}) {
  return (
    <Panel title="Block devices" right={
      <Button needsAdmin disabled={scanning} onClick={onScan}
        title="if your disk isn't showing shortly, rescan here — no interruptions">
        {scanning ? 'scanning…' : 'Scan for new disks'}
      </Button>
    }>
      <table className="w-full text-left text-xs">
        <thead>
          <tr className="text-zinc-600">
            <th className="py-1 pr-4 font-medium">device</th>
            <th className="py-1 pr-4 font-medium">size</th>
            <th className="py-1 pr-4 font-medium">model</th>
            <th className="py-1 pr-4 font-medium">type</th>
            <th className="py-1 font-medium">status</th>
          </tr>
        </thead>
        <tbody>
          {devices.map((d) => (
            <tr key={d.path} className="border-t border-zinc-800/60">
              <td className="py-1.5 pr-4 font-mono text-zinc-300">{d.path}</td>
              <td className="py-1.5 pr-4 font-mono text-zinc-400">{fmtBytes(d.size_bytes)}</td>
              <td className="py-1.5 pr-4 text-zinc-500">{d.model || '—'}</td>
              <td className="py-1.5 pr-4 text-zinc-500">
                {[d.transport, diskClass(d)].filter(Boolean).join(' · ')}
              </td>
              <td className="py-1.5">
                {d.eligible
                  ? <span className="rounded bg-emerald-500/15 px-1.5 py-0.5 text-emerald-300">available</span>
                  : <span className="text-zinc-500" title={d.reason}>{d.role ? d.role : d.reason}</span>}
              </td>
            </tr>
          ))}
          {devices.length === 0 && (
            <tr><td colSpan={5} className="py-2 text-zinc-600">no devices enumerated</td></tr>
          )}
        </tbody>
      </table>
    </Panel>
  )
}
