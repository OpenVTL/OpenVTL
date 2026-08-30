import { useEffect, useMemo, useState } from 'react'
import { api } from '../api'
import { Button, Field, Panel, inputCls, modelLabel, notify } from '../components/ui'
import { useAuthCtx } from '../useAuth'
import type { VTL } from '../useVTL'
import type {
  Cart, CatalogEntry, DriveModel, LibraryModel, LibraryRow, LibrarySnapshot,
  LoggedEvent, Pool, Remote,
} from '../types'
import { fmtBytes, fmtGen } from '../types'

export default function Library({ vtl, goto }: { vtl: VTL; goto: (v: string) => void }) {
  const { snap } = vtl
  const { admin } = useAuthCtx()
  const [selected, setSelected] = useState<string | null>(null)
  const [libID, setLibID] = useState<number | null>(null)
  const [remotes, setRemotes] = useState<Remote[]>([])
  const [rows, setRows] = useState<LibraryRow[]>([])
  const [showNew, setShowNew] = useState(false)

  const reloadRows = () => api.libraryRows().then(setRows).catch(() => {})
  useEffect(() => { api.remotes().then(setRemotes).catch(() => {}) }, [])
  // Keep the DB library list in step with the live snapshot. Fetched only
  // once on mount it goes stale after a delete+reboot: the row is gone
  // server-side, but the client still shows it — producing a phantom
  // "deletion left unfinished" orphan (and a 404 "unknown library" if the
  // operator clicks Finish) that only a manual refresh clears. Re-fetch
  // whenever the snapshot's set of libraries changes.
  const libSig = (snap?.libraries ?? []).map((l) => l.library.id).sort((a, b) => a - b).join(',')
  useEffect(() => { reloadRows() }, [libSig])

  if (!snap) return null
  // A registered library missing from the live snapshot = an
  // interrupted deletion; it keeps its pool reserved until finished.
  const orphans = rows.filter((r) => !snap.libraries.some((l) => l.library.id === r.id))
  const lib = snap.libraries.find((l) => l.library.id === libID)
    ?? snap.libraries.find((l) => l.library.live)
    ?? snap.libraries[0]
  // Zero libraries = fresh install or mid re-foundation: the wizard is
  // the whole view.
  if (!lib) {
    return (
      <div className="space-y-4">
        <OrphanLibraries rows={orphans} onDone={reloadRows} />
        <Panel title="Library">
          <p className="text-sm text-zinc-600">
            No libraries configured. Provision a pool in Storage, then declare the
            first library here.
          </p>
        </Panel>
        {admin && <NewLibraryPanel onCreated={() => {}} />}
      </div>
    )
  }
  const selCart = snap.cartridges.find((c) => c.label === selected && c.library === lib.library.id) ?? null
  const pending = snap.libraries.filter((l) => !l.library.live)

  return (
    <div className="space-y-4">
      <OrphanLibraries rows={orphans} onDone={reloadRows} />
      {pending.length > 0 && <ApplyBanner pending={pending} />}
      <div className="flex items-center justify-between border-b border-zinc-800 pb-px text-sm">
        <div className="flex gap-1">
          {snap.libraries.length > 1 && snap.libraries.map((l) => (
            <button key={l.library.id}
              onClick={() => { setLibID(l.library.id); setSelected(null) }}
              title={l.library.serial}
              className={`rounded-t px-3 py-1.5 font-medium transition-colors ${
                l.library.id === lib.library.id
                  ? 'bg-zinc-800/80 text-zinc-100'
                  : 'text-zinc-500 hover:bg-zinc-900 hover:text-zinc-300'
              }`}>
              {l.library.name}
              <span className="ml-1.5 text-[10px] text-zinc-600">{modelLabel(l.library.product)} · {l.library.serial}</span>
              {!l.library.live && (
                <span className="ml-1.5 rounded bg-amber-500/20 px-1.5 py-0.5 font-mono text-[10px] text-amber-300"
                  title="declared in device.conf but not served — awaiting the mhVTL maintenance window">
                  awaiting activation
                </span>
              )}
            </button>
          ))}
        </div>
        {admin && (
          <button onClick={() => setShowNew((v) => !v)}
            className="mb-1 rounded border border-zinc-700 bg-zinc-900 px-2.5 py-1 text-xs text-zinc-300 hover:bg-zinc-800">
            {showNew ? 'Close' : 'New library…'}
          </button>
        )}
      </div>
      {showNew && <NewLibraryPanel onCreated={() => setShowNew(false)} />}
      <div className="grid grid-cols-1 gap-6 lg:grid-cols-[1fr_360px]">
        <LibraryGrid vtl={vtl} lib={lib} selected={selected} onSelect={setSelected} />
        <div className="space-y-6">
          {selCart
            ? <CartDetail cart={selCart} lib={lib} remotes={remotes} vtl={vtl} goto={goto} onClose={() => setSelected(null)} />
            : (
              <Panel title="Cartridge">
                <p className="text-sm text-zinc-600">Select a cartridge for details, history and vault actions.</p>
              </Panel>
            )}
          <MintSection lib={lib} />
        </div>
      </div>
    </div>
  )
}

// OrphanLibraries — registered in the DB but absent from the live
// snapshot: a deletion whose restart was interrupted (e.g. the browser
// gave up, or the box was rebooted mid-apply). The config is already
// gone; finishing just erases the media + rows and frees the pool.
function OrphanLibraries({ rows, onDone }: { rows: LibraryRow[]; onDone: () => void }) {
  const { admin } = useAuthCtx()
  const [busy, setBusy] = useState(false)
  if (rows.length === 0) return null

  const finish = (row: LibraryRow) => {
    const typed = window.prompt(
      `Finish deleting library ${row.name}? Its removal was interrupted — the config ` +
      `is already gone, but its cartridges and records remain on this appliance.\n\n` +
      `Type the library name (${row.name}) to continue:`)
    if (typed === null) return
    if (typed.trim() !== row.name) { notify('name mismatch — cancelled', true); return }
    const ack = window.prompt(
      `You're erasing every cartridge on ${row.name} from this appliance. Copies already ` +
      `in S3 are kept.\n\nType "I understand" to continue:`)
    if (ack === null) return
    if (ack.trim() !== 'I understand') { notify('acknowledgement mismatch — cancelled', true); return }
    setBusy(true)
    api.deleteLibrary(row.id, row.name)
      .then((r) => { notify(`library ${row.name} deleted (${r.cartridges_deleted} cartridge(s) erased)`); onDone() })
      .catch((e: Error) => {
        // Already gone: a completed delete+reboot cleared the DB row, so
        // this banner was a stale client artifact. Clear it (reload rows)
        // instead of surfacing a confusing "unknown library" error.
        if (/unknown library/i.test(e.message)) {
          notify(`library ${row.name} was already deleted`)
          onDone()
        } else {
          notify(e.message, true)
        }
      })
      .finally(() => setBusy(false))
  }

  return (
    <div className="rounded-lg border border-amber-500/40 bg-amber-500/5 px-4 py-3">
      <div className="text-sm font-medium text-amber-300">
        {rows.length} library deletion{rows.length === 1 ? '' : 's'} left unfinished
        ({rows.map((r) => r.name).join(', ')})
      </div>
      <p className="mt-1 text-xs text-amber-200/70">
        The mhVTL config was removed but the media and records weren't cleaned up —
        the restart didn't complete. Finish to erase them and free the pool.
      </p>
      <div className="mt-2 flex flex-wrap gap-2">
        {rows.map((r) => (
          <Button key={r.id} kind="danger" needsAdmin disabled={busy || !admin} onClick={() => finish(r)}>
            {busy ? 'deleting…' : `Finish deleting ${r.name}`}
          </Button>
        ))}
      </div>
    </div>
  )
}

// ApplyBanner — the daemon half of the maintenance window. Loud on
// purpose: the FC rebuild drops every host session, and the operator
// owns vary off/on + the new MLB device description afterwards.
function ApplyBanner({ pending }: { pending: LibrarySnapshot[] }) {
  const [confirm, setConfirm] = useState('')
  const [busy, setBusy] = useState(false)
  const [steps, setSteps] = useState<string[]>([])

  const apply = () => {
    setBusy(true)
    setSteps([])
    api.applyLibraries()
      .then((r) => { setSteps(r.steps); notify('libraries activated — operator steps remain (vary off/on, MLB description)') })
      .catch((e: Error) => notify(e.message, true))
      .finally(() => { setBusy(false); setConfirm('') })
  }

  return (
    <div className="rounded-lg border border-amber-500/40 bg-amber-500/5 px-4 py-3">
      <div className="text-sm font-medium text-amber-300">
        {pending.length} librar{pending.length === 1 ? 'y' : 'ies'} awaiting activation
        ({pending.map((l) => l.library.name).join(', ')})
      </div>
      <p className="mt-1 text-xs text-amber-200/70">
        Activating restarts the tape daemons and rebuilds the targets — host connections
        drop briefly. Run during a maintenance window, then vary the host tape devices
        off/on (a new library also needs its MLB description).
      </p>
      <div className="mt-2 flex items-end gap-3">
        <Field label={'type "apply" to confirm'}>
          <input className={inputCls} value={confirm} placeholder="apply"
            onChange={(e) => setConfirm(e.target.value)} />
        </Field>
        <Button kind="danger" needsAdmin disabled={busy || confirm !== 'apply'} onClick={apply}>
          {busy ? 'activating… (restarting mhVTL)' : 'Activate — maintenance window'}
        </Button>
      </div>
      {steps.length > 0 && (
        <div className="mt-3 space-y-0.5 border-t border-amber-500/20 pt-2 font-mono text-[11px] text-amber-200/80">
          {steps.map((s, i) => <div key={i}>✓ {s}</div>)}
        </div>
      )}
    </div>
  )
}

// NewLibraryPanel — the catalog-driven wizard (operator decision:
// dropdown of what the installed mhVTL emulates, filtered to what the
// IBM i will attach). Pool pairing is 1:1 and mandatory.
function NewLibraryPanel({ onCreated }: { onCreated: () => void }) {
  const [models, setModels] = useState<{ libraries: LibraryModel[]; drives: DriveModel[] } | null>(null)
  const [pools, setPools] = useState<Pool[]>([])
  const [takenPools, setTakenPools] = useState<Set<number>>(new Set())
  const [name, setName] = useState('')
  const [product, setProduct] = useState('')
  const [driveProduct, setDriveProduct] = useState('')
  const [numDrives, setNumDrives] = useState('2')
  const [numSlots, setNumSlots] = useState('100')
  const [numMAP, setNumMAP] = useState('4')
  const [prefix, setPrefix] = useState('')
  const [poolID, setPoolID] = useState('')
  const [busy, setBusy] = useState(false)

  useEffect(() => {
    api.models().then(setModels).catch(() => {})
    api.pools().then(setPools).catch(() => {})
    api.libraryRows().then((rows) => setTakenPools(new Set(rows.map((r) => r.home_pool)))).catch(() => {})
  }, [])

  // Creatable is variant-level (e.g. of the TS3500 frames only the
  // spec-validated L32 may be created); each option carries its model's
  // drive cap (3573-TL: 4, 3584: 12 per real frame).
  const variants = (models?.libraries ?? []).filter((m) => m.creatable)
    .flatMap((m) => (m.variants ?? []).filter((v) => v.creatable)
      .map((v) => ({ ...v, parent: m.display, maxDrives: m.max_drives || 4 })))
  const selected = variants.find((v) => v.product === product)
  const family = selected?.family
  const maxDrives = selected?.maxDrives ?? 4
  const drives = (models?.drives ?? []).filter((d) => d.ibmi_compatible && (!family || d.family === family))
  const freePools = pools.filter((p) => p.state === 'active' && !takenPools.has(p.id))

  const slotsN = Number(numSlots) || 0
  const mapN = Number(numMAP) || 0
  const drivesN = Number(numDrives) || 0
  const slotsOK = slotsN >= 1 && slotsN <= 400
  const mapOK = mapN >= 1 && mapN <= 32
  const drivesOK = drivesN >= 1 && drivesN <= maxDrives

  const create = () => {
    setBusy(true)
    api.createLibrary({
      name: name.trim() || undefined,
      product, drive_product: driveProduct, num_drives: Number(numDrives) || 1,
      num_slots: slotsN, num_map: mapN,
      label_prefix: prefix.toUpperCase(), pool_id: Number(poolID),
    })
      .then((r) => { notify(`library ${name.trim() || r.serial} declared — ${r.state}`); onCreated() })
      .catch((e: Error) => notify(e.message, true))
      .finally(() => setBusy(false))
  }

  return (
    <Panel title="New library">
      <div className="grid grid-cols-1 gap-3 md:grid-cols-3">
        <Field label="name (optional — blank uses the serial)">
          <input className={inputCls} value={name} placeholder="PROD-VTL"
            onChange={(e) => setName(e.target.value)} />
        </Field>
        <Field label="library model">
          <select className={inputCls} value={product}
            onChange={(e) => { setProduct(e.target.value); setDriveProduct('') }}>
            <option value="">select…</option>
            {variants.map((v) => (
              // The model display alone ("TS3100/TS3200 (3573)") is the whole
              // label while each model has one creatable variant; the variant
              // suffix returns only if a second frame of the same model ever
              // becomes creatable and the options need disambiguating.
              <option key={v.product} value={v.product}>
                {variants.filter((x) => x.parent === v.parent).length > 1
                  ? `${v.parent} — ${v.display}` : v.parent}
              </option>
            ))}
          </select>
        </Field>
        <Field label="drive model">
          <select className={inputCls} value={driveProduct} disabled={!product}
            onChange={(e) => setDriveProduct(e.target.value)}>
            <option value="">select…</option>
            {drives.map((d) => (
              <option key={d.product} value={d.product} title={d.note}>{d.display}</option>
            ))}
          </select>
        </Field>
        <Field label={`drives (1-${maxDrives})`}>
          <input className={inputCls} inputMode="numeric" value={numDrives}
            onChange={(e) => setNumDrives(e.target.value.replace(/[^0-9]/g, ''))} />
        </Field>
        <Field label="storage slots (1-400)">
          <input className={inputCls} inputMode="numeric" value={numSlots}
            onChange={(e) => setNumSlots(e.target.value.replace(/[^0-9]/g, ''))} />
        </Field>
        <Field label="import/export slots (1-32)">
          <input className={inputCls} inputMode="numeric" value={numMAP}
            onChange={(e) => setNumMAP(e.target.value.replace(/[^0-9]/g, ''))} />
        </Field>
        <Field label="cart label prefix (3 chars, e.g. OVB)">
          <input className={inputCls} maxLength={3} value={prefix} placeholder="OVB"
            onChange={(e) => setPrefix(e.target.value.toUpperCase())} />
        </Field>
        <Field label="storage pool (one library per pool)">
          <select className={inputCls} value={poolID} onChange={(e) => setPoolID(e.target.value)}>
            <option value="">{freePools.length ? 'select…' : 'no unpaired active pool — provision one in Storage'}</option>
            {freePools.map((p) => (
              <option key={p.id} value={p.id}>{p.name} · {p.mountpoint}</option>
            ))}
          </select>
        </Field>
        <div className="flex items-end">
          <Button kind="primary" needsAdmin
            disabled={busy || !product || !driveProduct || !prefix.match(/^[A-Z0-9]{3}$/) || !poolID || !slotsOK || !mapOK || !drivesOK}
            onClick={create}>
            {busy ? 'creating…' : 'Declare library'}
          </Button>
        </div>
      </div>
      <p className="mt-2 text-xs text-zinc-600">
        Declaring stages the library — it starts serving after Activate (maintenance
        window). The name is permanent.
      </p>
    </Panel>
  )
}

// fmtCapacity — a drive's decimal-MB capacity to a friendly size string.
function fmtCapacity(mb: number): string {
  if (!mb || mb <= 0) return ''
  return mb >= 1_000_000 ? `${mb / 1_000_000} TB` : `${Math.round(mb / 1000)} GB`
}

// MintSection — cart creation collapsed behind a toggle (v0.7 UI:
// detail-first rail; creation is deliberate, not ambient).
function MintSection({ lib }: { lib: LibrarySnapshot }) {
  const { admin } = useAuthCtx()
  const [open, setOpen] = useState(false)
  if (!admin) return null
  if (!open) {
    return (
      <button onClick={() => setOpen(true)}
        className="w-full rounded border border-zinc-800 bg-zinc-900/40 px-3 py-2 text-left text-xs text-zinc-500 hover:bg-zinc-900 hover:text-zinc-300">
        New cartridges…
      </button>
    )
  }
  return <MintPanel lib={lib} onClose={() => setOpen(false)} />
}

// MintPanel — zero-restart cart creation into the active library. The
// daemon runs the cart through the MAP (IE watcher suppressed) into a
// free slot; the file mhVTL reads at startup is rewritten so restarts
// agree.
function MintPanel({ lib, onClose }: { lib: LibrarySnapshot; onClose: () => void }) {
  const [label, setLabel] = useState('')
  const [capacity, setCapacity] = useState('')
  const [count, setCount] = useState('1')
  const [busy, setBusy] = useState(false)
  const [prefix, setPrefix] = useState('OVT')
  const [next, setNext] = useState('')
  const n = Number(count) || 1
  const batch = n > 1
  // The batch can't exceed the library's free storage slots — each cart
  // parks in one. Reactive: it drops as carts are minted (SSE snapshot).
  const avail = lib.slots.filter((s) => s.kind === 'storage' && !s.label).length

  // Cart size is not chosen — it follows the library's drive (LTO type).
  // Look it up from the model catalog for a read-only display.
  useEffect(() => {
    Promise.all([api.libraryRows(), api.models()])
      .then(([rows, models]) => {
        const row = rows.find((r) => r.id === lib.library.id)
        if (row?.label_prefix) setPrefix(row.label_prefix)
        const dm = row && models.drives.find((d) => d.product === row.drive_model)
        if (dm) setCapacity(fmtCapacity(dm.capacity_mb))
      })
      .catch(() => {})
  }, [lib.library.id])

  // The next auto-sequenced label, refreshed whenever this library's carts
  // change (like the free-slot count) so a batch shows its real starting
  // label. Keyed on the cart labels so it re-fetches after each mint.
  const cartsSig = (lib.cartridges ?? []).map((c) => c.label).sort().join(',')
  useEffect(() => {
    api.nextLabel(lib.library.id).then((r) => setNext(r.label)).catch(() => {})
  }, [lib.library.id, cartsSig])

  const create = () => {
    setBusy(true)
    api.mintCart(lib.library.id, batch ? '' : label.trim().toUpperCase(), n)
      .then((r) => {
        const labels = r.created.map((c) => c.label)
        notify(r.created.length === 1
          ? `cartridge ${labels[0]} created in slot ${r.created[0].slot}`
          : `${r.created.length} cartridges created: ${labels[0]}…${labels[labels.length - 1]}`)
        setLabel('')
        setCount('1')
      })
      .catch((e: Error) => notify(e.message, true))
      .finally(() => setBusy(false))
  }

  return (
    <Panel title={`New cartridges · ${lib.library.name}`} right={
      <button onClick={onClose} className="text-xs text-zinc-500 hover:text-zinc-200">Close</button>
    }>
      <div className="grid grid-cols-1 items-end gap-3 md:grid-cols-[1fr_80px_80px_auto]">
        <Field label={batch ? 'labels auto-sequence for a batch' : 'label (blank = next in sequence)'}>
          <input className={inputCls} placeholder={next || `${prefix}001…`} disabled={batch}
            value={batch ? next : label} onChange={(e) => setLabel(e.target.value)} />
        </Field>
        <Field label="size (from drive)">
          <div className={`${inputCls} flex items-center text-zinc-400`} title="cart capacity is set by the library's drive type">
            {capacity || '—'}
          </div>
        </Field>
        <Field label={avail > 0 ? `count (1-${avail})` : 'count'}>
          <input className={inputCls} inputMode="numeric" value={count}
            onChange={(e) => {
              const v = e.target.value.replace(/[^0-9]/g, '')
              setCount(avail > 0 && Number(v) > avail ? String(avail) : v)
            }} />
        </Field>
        <Button kind="primary" needsAdmin disabled={busy || !lib.library.live || avail < 1 || n < 1 || n > avail} onClick={create}>
          {busy ? `creating${batch ? ` ${n}` : ''}…` : batch ? `Create ${n}` : 'Create'}
        </Button>
      </div>
      <p className="mt-2 text-xs text-zinc-600">
        {avail < 1
          ? 'Every storage slot is occupied — free a slot (delete or export a cart) before creating more.'
          : `Takes effect immediately — no restart needed. Cart capacity follows the library's drive
             (its LTO type) and is declared capacity, not space used. Up to ${avail} fit the free
             slots; batch labels number themselves in sequence.`}
      </p>
    </Panel>
  )
}

function LibraryGrid({ vtl, lib, selected, onSelect }: {
  vtl: VTL; lib: LibrarySnapshot; selected: string | null; onSelect: (l: string | null) => void
}) {
  const { admin } = useAuthCtx()
  const snap = vtl.snap!
  // A pending library has never been polled — its slots/drives may be
  // null from the API. Guard BEFORE any access.
  if (!lib.library.live) {
    return (
      <Panel title={`Library · ${lib.library.name}`}
        right={<span className="font-mono text-xs text-zinc-500" title="host-facing library serial — also its folder in S3">{lib.library.serial}</span>}>
        <p className="text-sm text-amber-300">
          Declared but not serving yet — activate it from the banner above during a
          maintenance window.
        </p>
        <DeleteLibraryRow lib={lib} />
      </Panel>
    )
  }
  const byLabel = new Map(snap.cartridges.filter((c) => c.library === lib.library.id).map((c) => [c.label, c]))
  const storage = (lib.slots ?? []).filter((s) => s.kind === 'storage')
  const ie = (lib.slots ?? []).filter((s) => s.kind === 'ie')
  const drives = lib.drives ?? []

  const cell = (label: string, num: number, kind: string) => {
    const cart = label ? byLabel.get(label) : undefined
    const hasData = (cart?.size_bytes ?? 0) > 1 << 20
    const evicted = cart?.local_state === 'evicted'
    const sel = selected === label
    return (
      <button
        key={`${kind}${num}`}
        title={label
          ? `${label} · ${fmtBytes(cart?.size_bytes ?? 0)}${evicted ? ' · EVICTED STUB' : ''}`
          : `${kind} ${num} · empty`}
        onClick={() => onSelect(label || null)}
        className={`aspect-[2/1] rounded-sm border text-[9px] font-mono leading-none transition-colors ${
          sel ? 'border-amber-400 bg-amber-400/20 text-amber-300'
            : evicted
              ? 'border-red-800/70 bg-red-500/10 text-red-300 hover:bg-red-500/25'
              : label
                ? hasData
                  ? 'border-emerald-700/60 bg-emerald-500/15 text-emerald-300 hover:bg-emerald-500/30'
                  : 'border-zinc-700 bg-zinc-800/80 text-zinc-400 hover:bg-zinc-700'
                : 'border-zinc-800 bg-zinc-900/40 text-zinc-700'
        }`}
      >
        {label ? label.replace(/(L[1-9]|J[AB])$/, '') : num}
      </button>
    )
  }

  return (
    <Panel
      title={`Library · ${lib.library.name}`}
      right={
        <span className="flex items-center gap-3 text-xs text-zinc-600">
          <span className="font-mono text-zinc-500" title="host-facing library serial — also its folder in S3">{lib.library.serial}</span>
          <span>{storage.filter((s) => s.label).length}/{storage.length} slots occupied</span>
        </span>
      }
    >
      <div className="mb-2 text-xs font-medium text-zinc-500">Drives</div>
      <div className="mb-4 grid grid-cols-2 gap-1">
        {drives.map((d) => (
          <div key={d.index}
            className={`flex items-center justify-between rounded-sm border px-2 py-1 font-mono text-[10px] ${
              d.loaded ? 'border-amber-700/60 bg-amber-500/10 text-amber-300'
                : 'border-zinc-800 bg-zinc-900/40 text-zinc-700'
            }`}>
            <button onClick={() => onSelect(d.loaded || null)}
              className={d.loaded ? 'hover:text-amber-200' : 'cursor-default'}>
              drv{d.index} {d.loaded || '· empty'}
              {d.activity !== 'idle' && <span className="ml-1 text-emerald-400">({d.activity})</span>}
            </button>
            {d.loaded && (
              <button
                title={!admin ? 'admin role required'
                  : d.activity !== 'idle' ? `drive is ${d.activity}` : `unload ${d.loaded} to a storage slot`}
                disabled={d.activity !== 'idle' || !admin}
                onClick={() =>
                  api.unloadDrive(lib.library.id, d.index)
                    .then((r) => notify(r.detail))
                    .catch((e: Error) => notify(e.message, true))
                }
                className="rounded border border-zinc-700 bg-zinc-800/80 px-1.5 py-0.5 text-[9px] text-zinc-300 hover:bg-zinc-700 disabled:cursor-not-allowed disabled:opacity-40">
                Unload
              </button>
            )}
          </div>
        ))}
      </div>
      <div className="mb-2 text-xs font-medium text-zinc-500">Storage slots</div>
      <div className="grid grid-cols-10 gap-1">
        {storage.map((s) => cell(s.label, s.num, 'slot'))}
      </div>
      <div className="mt-4 mb-2 text-xs font-medium text-zinc-500">Import/Export</div>
      <div className="grid grid-cols-10 gap-1">
        {ie.map((s) => cell(s.label, s.num, 'ie'))}
      </div>
      <div className="mt-4 flex gap-4 text-[10px] text-zinc-600">
        <LegendDot cls="bg-emerald-500/15 border-emerald-700/60" label="data" />
        <LegendDot cls="bg-zinc-800/80 border-zinc-700" label="label only" />
        <LegendDot cls="bg-red-500/10 border-red-800/70" label="evicted stub" />
        <LegendDot cls="bg-zinc-900/40 border-zinc-800" label="empty" />
      </div>
      <DeleteLibraryRow lib={lib} />
    </Panel>
  )
}

// DeleteLibraryRow — removes the library, its drives AND every
// cartridge on it (cascade; drives never leave independently). Double
// acknowledgement: the name, then the literal "I understand". A
// serving library needs the restart, so host connections drop.
function DeleteLibraryRow({ lib }: { lib: LibrarySnapshot }) {
  const { admin } = useAuthCtx()
  const [busy, setBusy] = useState(false)
  if (!admin) return null

  const del = () => {
    const name = lib.library.name
    const nCarts = (lib.cartridges ?? []).length
    const typed = window.prompt(
      `Delete library ${name} and its ${lib.library.num_drives} drive(s)?\n\n` +
      (lib.library.live
        ? 'The appliance will REBOOT to complete this — all host connections drop (maintenance window).\n\n'
        : '') +
      `Type the library name (${name}) to continue:`)
    if (typed === null) return
    if (typed.trim() !== name) { notify('name mismatch — cancelled', true); return }
    const ack = window.prompt(
      `You're deleting ${name}${nCarts > 0
        ? ` — all ${nCarts} cartridge(s) on it will be erased from this appliance. Copies already in S3 are kept.`
        : '.'}` +
      (lib.library.live ? ' The appliance reboots to finish.' : '') +
      `\n\nType "I understand" to continue:`)
    if (ack === null) return
    if (ack.trim() !== 'I understand') { notify('acknowledgement mismatch — cancelled', true); return }
    setBusy(true)
    api.deleteLibrary(lib.library.id, name)
      .then((r) => notify(r.rebooting
        ? `library ${name} deleted — rebooting to finish (back in a minute or two)`
        : `library ${name} deleted (${r.cartridges_deleted} cartridge(s) erased)`))
      .catch((e: Error) => notify(e.message, true))
      .finally(() => setBusy(false))
  }

  return (
    <div className="mt-4 border-t border-zinc-800 pt-3">
      <Button kind="danger" needsAdmin disabled={busy} onClick={del}>
        {busy ? 'deleting…' : 'Delete library…'}
      </Button>
    </div>
  )
}

function LegendDot({ cls, label }: { cls: string; label: string }) {
  return (
    <span className="inline-flex items-center gap-1.5">
      <span className={`inline-block h-2.5 w-4 rounded-sm border ${cls}`} />
      {label}
    </span>
  )
}

function CartDetail({ cart, lib, remotes, vtl, goto, onClose }: {
  cart: Cart; lib: LibrarySnapshot; remotes: Remote[]; vtl: VTL; goto: (v: string) => void; onClose: () => void
}) {
  const [remoteId, setRemoteId] = useState<number>(remotes[0]?.id ?? 0)
  const [history, setHistory] = useState<LoggedEvent[]>([])
  const [exportsList, setExportsList] = useState<CatalogEntry[]>([])
  const [busy, setBusy] = useState(false)

  useEffect(() => { if (remotes.length && !remoteId) setRemoteId(remotes[0].id) }, [remotes, remoteId])

  useEffect(() => {
    api.journal()
      .then((evs) => setHistory(evs.filter((e) => e.subject === cart.label).slice(0, 40)))
      .catch(() => {})
    Promise.all(remotes.map((r) => api.catalog(r.id).catch(() => [] as CatalogEntry[])))
      .then((all) => setExportsList(all.flat().filter((e) => e.cart_label === cart.label)))
  }, [cart.label, remotes])

  const evicted = cart.local_state === 'evicted'
  const inDrive = cart.location.startsWith('drive:')
  const emptyDrives = lib.drives.filter((d) => !d.loaded)
  const activeJob = vtl.jobs.find((j) => j.cart_label === cart.label && !['done', 'failed', 'cancelled'].includes(j.state))

  const run = (fn: () => Promise<unknown>, verb: string) => {
    setBusy(true)
    fn()
      .then(() => { notify(`${verb} job started for ${cart.label}`); goto('jobs') })
      .catch((e: Error) => notify(e.message, true))
      .finally(() => setBusy(false))
  }

  const loadInto = (driveIdx: number) => {
    if (evicted && !window.confirm(
      `${cart.label} is an EVICTED STUB — the host will raise a media error (unlabelled volume). Load it anyway?`)) return
    setBusy(true)
    api.loadDrive(lib.library.id, driveIdx, cart.label)
      .then((r) => notify(r.detail))
      .catch((e: Error) => notify(e.message, true))
      .finally(() => setBusy(false))
  }

  return (
    <div className={`rounded-lg border p-4 ${evicted ? 'border-red-500/50 bg-red-950/20' : 'border-amber-500/30 bg-zinc-900/80'}`}>
      <div className="flex items-center justify-between">
        <h3 className="font-mono text-base text-amber-300">{cart.label}</h3>
        <button onClick={onClose} className="text-zinc-500 hover:text-zinc-200">✕</button>
      </div>

      {evicted && (
        <div className="mt-3 rounded border border-red-500/50 bg-red-500/10 px-3 py-2 text-xs text-red-300">
          <div className="font-semibold uppercase tracking-wide">Evicted stub — no data on disk</div>
          <p className="mt-1 text-red-300/80">
            Mounting this cartridge raises a media error on the host (volume
            reads as unlabelled, IBM i shows *N). Import generation{' '}
            {fmtGen(cart.last_export_gen)} before any restore.
          </p>
        </div>
      )}

      <dl className="mt-3 space-y-1.5 text-sm">
        <Row k="library" v={lib.library.name} />
        <Row k="location" v={cart.location} />
        <Row k="data on cart" v={fmtBytes(cart.size_bytes)} />
        <Row k="actual on disk" v={cart.phys_bytes != null ? fmtBytes(cart.phys_bytes) : '—'} />
        <Row k="local state" v={cart.local_state ?? 'resident'} />
        <Row k="last export" v={cart.last_export_gen ? fmtGen(cart.last_export_gen) : '—'} />
        <Row k="last modified" v={cart.modified ? new Date(cart.modified).toLocaleString() : '—'} />
      </dl>

      <div className="mt-4 border-t border-zinc-800 pt-3">
        <div className="mb-2 flex items-center justify-between text-xs text-zinc-500">
          <span>Vault actions</span>
          {remotes.length > 0 && (
            <select value={remoteId} onChange={(e) => setRemoteId(Number(e.target.value))}
              className="rounded border border-zinc-700 bg-zinc-900 px-1.5 py-0.5 text-zinc-300">
              {remotes.map((r) => <option key={r.id} value={r.id}>{r.name}</option>)}
            </select>
          )}
        </div>
        {remotes.length === 0 ? (
          <p className="text-xs text-zinc-600">
            No S3 remotes configured — <button className="text-sky-400" onClick={() => goto('s3')}>add one</button>.
          </p>
        ) : activeJob ? (
          <p className="text-xs text-zinc-500">
            job #{activeJob.id} ({activeJob.kind}) is {activeJob.state} —{' '}
            <button className="text-sky-400" onClick={() => goto('jobs')}>watch</button>
          </p>
        ) : (
          <div className="flex flex-wrap gap-2">
            <Button kind="primary" needsAdmin disabled={busy || inDrive || evicted}
              title={inDrive ? 'cart is loaded in a drive' : undefined}
              onClick={() => run(() => api.exportCart(cart.label, remoteId), 'Export')}>
              Export now
            </Button>
            <Button kind="danger" needsAdmin disabled={busy || inDrive || evicted || !cart.last_export_gen}
              title={!cart.last_export_gen ? 'no completed export to back a stub' : undefined}
              onClick={() => {
                if (window.confirm(`Evict ${cart.label}? Local data is deleted; a labelled stub remains. Restore requires importing generation ${fmtGen(cart.last_export_gen)}.`))
                  run(() => api.evictCart(cart.label, remoteId), 'Evict')
              }}>
              Evict local data
            </Button>
            {evicted && cart.last_export_gen && (
              <Button kind="primary" needsAdmin disabled={busy || inDrive}
                onClick={() => run(() => api.importCart(cart.label, remoteId, cart.last_export_gen!), 'Import')}>
                Import {fmtGen(cart.last_export_gen)}
              </Button>
            )}
          </div>
        )}
      </div>

      {!inDrive && emptyDrives.length > 0 && (
        <div className="mt-3 border-t border-zinc-800 pt-3">
          <div className="mb-2 text-xs text-zinc-500">Drive control</div>
          <div className="flex flex-wrap gap-2">
            {emptyDrives.map((d) => (
              <Button key={d.index} needsAdmin disabled={busy} onClick={() => loadInto(d.index)}>
                Load into drive {d.index}
              </Button>
            ))}
          </div>
        </div>
      )}

      {exportsList.length > 0 && (
        <div className="mt-4 border-t border-zinc-800 pt-3">
          <div className="mb-2 text-xs text-zinc-500">Exports in S3</div>
          <div className="space-y-1 text-xs">
            {exportsList.map((e) => (
              <div key={`${e.remote_id}:${e.generation}`} className="flex justify-between font-mono">
                <span className="text-zinc-300">{fmtGen(e.generation)}</span>
                <span className="text-zinc-500">{fmtBytes(e.stored_bytes)} · {e.chunk_count} chunks</span>
              </div>
            ))}
          </div>
        </div>
      )}

      <div className="mt-4 border-t border-zinc-800 pt-3">
        <div className="mb-2 text-xs text-zinc-500">History (journal)</div>
        <div className="max-h-56 space-y-1 overflow-y-auto text-xs">
          {history.length === 0 && <div className="text-zinc-700">no recorded events for this cart</div>}
          {history.map((e) => (
            <div key={e.id} className="flex gap-2">
              <span className="shrink-0 font-mono text-zinc-600">{new Date(e.ts).toLocaleString()}</span>
              <span className="truncate text-zinc-400" title={e.detail}>{e.kind}: {e.detail || '—'}</span>
            </div>
          ))}
        </div>
      </div>

      <div className="mt-4 border-t border-zinc-800 pt-3">
        <Button kind="danger" needsAdmin disabled={busy || inDrive || !!activeJob}
          title={inDrive ? 'cart is loaded in a drive' : activeJob ? 'a job is running on this cart' : undefined}
          onClick={() => {
            const warn = cart.size_bytes > 1 << 20
              ? `${cart.label} holds ${fmtBytes(cart.size_bytes)} of data. `
              : ''
            const typed = window.prompt(
              `${warn}Deleting removes it from the library and erases its local files. ` +
              `Copies already in S3 are kept.\n\n` +
              `Type the label (${cart.label}) to confirm:`)
            if (typed === null) return
            if (typed.trim().toUpperCase() !== cart.label) {
              notify('label mismatch — deletion cancelled', true)
              return
            }
            setBusy(true)
            api.deleteCart(cart.label)
              .then(() => { notify(`cartridge ${cart.label} deleted`); onClose() })
              .catch((e: Error) => notify(e.message, true))
              .finally(() => setBusy(false))
          }}>
          Delete cartridge…
        </Button>
      </div>
    </div>
  )
}

function Row({ k, v }: { k: string; v: string }) {
  return (
    <div className="flex justify-between gap-4">
      <dt className="text-zinc-500">{k}</dt>
      <dd className="text-right font-mono text-zinc-200">{v}</dd>
    </div>
  )
}

export function useRemotes() {
  const [remotes, setRemotes] = useState<Remote[]>([])
  const reload = useMemo(() => () => { api.remotes().then(setRemotes).catch(() => {}) }, [])
  useEffect(reload, [reload])
  return { remotes, reload }
}
