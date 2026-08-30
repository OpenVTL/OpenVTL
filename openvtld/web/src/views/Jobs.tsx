import { useEffect, useState } from 'react'
import { api } from '../api'
import { Button, Panel, Progress, StateBadge, notify } from '../components/ui'
import type { VTL } from '../useVTL'
import type { ExportChunk, Job, JobEvent } from '../types'
import { fmtBytes, fmtGen, isActive } from '../types'

export default function Jobs({ vtl }: { vtl: VTL }) {
  const { jobs } = vtl
  const [open, setOpen] = useState<number | null>(null)
  const [q, setQ] = useState('')
  const [deep, setDeep] = useState<Job[] | null>(null) // full-history results, or null for the loaded/in-view list
  const [searching, setSearching] = useState(false)
  const LIMIT = 500

  const needle = q.trim().toLowerCase()
  const matches = (j: Job) =>
    needle === '' ||
    String(j.id).includes(needle) ||
    j.kind.toLowerCase().includes(needle) ||
    j.state.toLowerCase().includes(needle) ||
    (j.cart_label ?? '').toLowerCase().includes(needle) ||
    (j.trigger ?? '').toLowerCase().includes(needle) ||
    (j.generation ?? '').toLowerCase().includes(needle) ||
    (j.system_name ?? '').toLowerCase().includes(needle) ||
    (j.error ?? '').toLowerCase().includes(needle)
  const inView = jobs.filter(matches)

  const runDeep = () => {
    if (needle === '') { setDeep(null); return }
    setSearching(true)
    api.searchJobs(q.trim(), LIMIT)
      .then(setDeep)
      .catch((e: Error) => { notify(e.message, true); setDeep(null) })
      .finally(() => setSearching(false))
  }
  // Editing the query drops back to the in-view feed until a fresh deep search.
  const onQ = (v: string) => { setQ(v); setDeep(null) }
  const shown = deep ?? inView

  if (jobs.length === 0) {
    return (
      <Panel title="Jobs">
        <p className="text-sm text-zinc-600">
          No jobs yet. Start one from a cartridge in Libraries, or import from an Offsite catalog.
        </p>
      </Panel>
    )
  }

  return (
    <Panel title="Jobs" right={<span className="text-xs text-zinc-600">{jobs.filter(isActive).length} active</span>}>
      <div className="mb-2 flex items-center gap-2 text-xs">
        <input
          className="flex-1 rounded border border-zinc-700 bg-zinc-900 px-2 py-1 text-zinc-200 placeholder:text-zinc-600"
          placeholder="search jobs — label, kind, state, trigger, error…"
          value={q}
          onChange={(e) => onQ(e.target.value)}
          onKeyDown={(e) => { if (e.key === 'Enter') runDeep() }}
        />
        <button onClick={runDeep} disabled={searching || needle === ''}
          title="search every job, not just the recent ones loaded here"
          className="shrink-0 rounded border border-zinc-700 bg-zinc-900 px-2 py-1 text-zinc-300 hover:text-zinc-100 disabled:opacity-40">
          {searching ? 'searching…' : 'Search all jobs'}
        </button>
      </div>
      {deep && (
        <div className="mb-2 flex items-center justify-between text-[11px] text-zinc-500">
          <span>{deep.length}{deep.length >= LIMIT ? '+' : ''} match{deep.length === 1 ? '' : 'es'} across all jobs</span>
          <button onClick={() => setDeep(null)} className="text-sky-400 hover:text-sky-300">back to recent</button>
        </div>
      )}
      <table className="w-full text-left text-sm">
        <thead>
          <tr className="border-b border-zinc-800 text-[11px] uppercase tracking-wider text-zinc-500">
            <th className="py-2 pr-3 font-medium">#</th>
            <th className="py-2 pr-3 font-medium">kind</th>
            <th className="py-2 pr-3 font-medium">cartridge</th>
            <th className="py-2 pr-3 font-medium">generation</th>
            <th className="py-2 pr-3 font-medium">state</th>
            <th className="py-2 pr-3 font-medium">progress</th>
            <th className="py-2 pr-3 font-medium">trigger</th>
            <th className="py-2 font-medium"></th>
          </tr>
        </thead>
        <tbody>
          {shown.length === 0 && (
            <tr><td colSpan={8} className="py-3 text-xs text-zinc-600">no matching jobs</td></tr>
          )}
          {shown.map((j) => (
            <JobRow key={j.id} j={j} open={open === j.id} toggle={() => setOpen(open === j.id ? null : j.id)} />
          ))}
        </tbody>
      </table>
    </Panel>
  )
}

function JobRow({ j, open, toggle }: { j: Job; open: boolean; toggle: () => void }) {
  return (
    <>
      <tr onClick={toggle} className="cursor-pointer border-b border-zinc-800/60 hover:bg-zinc-800/30">
        <td className="py-2 pr-3 font-mono text-zinc-500">{j.id}</td>
        <td className="py-2 pr-3">{j.kind}</td>
        <td className="py-2 pr-3 font-mono text-amber-300">{j.cart_label}</td>
        <td className="py-2 pr-3 font-mono text-xs text-zinc-400">{j.generation ? fmtGen(j.generation) : '—'}</td>
        <td className="py-2 pr-3"><StateBadge state={j.state} /></td>
        <td className="py-2 pr-3">
          <div className="flex items-center gap-2">
            <div className="w-28"><Progress done={j.bytes_done} total={j.bytes_total} /></div>
            <span className="font-mono text-[10px] text-zinc-500">
              {j.chunks_total > 0 ? `${j.chunks_done}/${j.chunks_total}` : ''}
            </span>
          </div>
        </td>
        <td className="py-2 pr-3 text-xs text-zinc-500">{j.trigger}</td>
        <td className="py-2 text-right text-xs text-zinc-600">{open ? '▲' : '▼'}</td>
      </tr>
      {open && (
        <tr className="border-b border-zinc-800/60 bg-zinc-950/60">
          <td colSpan={8} className="px-2 py-3">
            <JobDetail j={j} />
          </td>
        </tr>
      )}
    </>
  )
}

function JobDetail({ j }: { j: Job }) {
  const [events, setEvents] = useState<JobEvent[]>([])
  const [chunks, setChunks] = useState<ExportChunk[]>([])
  const [busy, setBusy] = useState(false)

  useEffect(() => {
    let stop = false
    const load = () =>
      api.job(j.id).then((d) => {
        if (stop) return
        setEvents(d.events)
        setChunks(d.chunks ?? [])
      }).catch(() => {})
    load()
    const t = window.setInterval(load, isActive(j) ? 3000 : 30000)
    return () => { stop = true; window.clearInterval(t) }
  }, [j.id, j.state]) // eslint-disable-line react-hooks/exhaustive-deps

  const act = (fn: () => Promise<unknown>, verb: string) => {
    setBusy(true)
    fn().then(() => notify(`${verb} job #${j.id}`)).catch((e: Error) => notify(e.message, true)).finally(() => setBusy(false))
  }

  return (
    <div className="grid grid-cols-1 gap-4 lg:grid-cols-[1fr_320px]">
      <div>
        <div className="mb-2 flex items-center justify-between">
          <span className="text-xs font-medium text-zinc-400">State transitions</span>
          <div className="flex gap-2">
            {(j.state === 'failed' || j.state === 'cancelled') && j.kind !== 'pool_create' && j.kind !== 'pool_remove' && (
              <Button kind="primary" needsAdmin disabled={busy} onClick={() => act(() => api.retryJob(j.id), 'Retried')}>Retry</Button>
            )}
            {isActive(j) && (
              <Button kind="danger" needsAdmin disabled={busy} onClick={() => act(() => api.cancelJob(j.id), 'Cancelled')}>Cancel</Button>
            )}
          </div>
        </div>
        {j.error && (
          <div className="mb-2 rounded border border-red-800/50 bg-red-950/40 px-3 py-2 font-mono text-xs text-red-300">
            {j.error}
          </div>
        )}
        <ol className="relative ml-2 space-y-2 border-l border-zinc-800 pl-4 text-xs">
          {events.map((e) => (
            <li key={e.id} className="relative">
              <span className="absolute -left-[21.5px] top-1 h-2 w-2 rounded-full bg-zinc-600" />
              <div className="flex items-baseline gap-2">
                <span className="font-mono text-zinc-600">{new Date(e.ts).toLocaleTimeString()}</span>
                <StateBadge state={e.to_state} />
              </div>
              {e.detail && <p className="mt-0.5 text-zinc-500">{e.detail}</p>}
            </li>
          ))}
        </ol>
      </div>
      {chunks.length > 0 && (
        <div>
          <div className="mb-2 text-xs font-medium text-zinc-400">Chunks</div>
          <div className="max-h-64 space-y-1 overflow-y-auto font-mono text-[11px]">
            {chunks.map((c) => (
              <div key={c.idx} className="flex justify-between rounded bg-zinc-900/70 px-2 py-1">
                <span className="text-zinc-400">#{c.idx}</span>
                <span className="text-zinc-500">{fmtBytes(c.raw_bytes)} → {fmtBytes(c.stored_bytes)}</span>
                <span className={c.uploaded_at ? 'text-emerald-400' : 'text-zinc-600'}>
                  {c.uploaded_at ? '✓' : '…'}
                </span>
              </div>
            ))}
          </div>
        </div>
      )}
    </div>
  )
}
