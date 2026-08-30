import { useEffect, useState } from 'react'
import { api } from '../api'
import { AreaChart, Button, Panel, Spark, Stat, StateBadge, Progress, modelLabel, notify } from '../components/ui'
import type { VTL, PoolSample as LivePoolSample } from '../useVTL'
import type { Drive, LibrarySnapshot, LibraryRow, LoggedEvent, Pool, PoolStats, Settings, SystemStatus } from '../types'
import { fmtBytes, fmtRatio, fmtTime, isActive } from '../types'

// Dashboard (v0.7): system-wide stats in the top
// strip, everything library-owned inside per-library cards (the paired
// pool's stats live there — 1:1 model). Trend + activity stay broad.
export default function Dashboard({ vtl, goto }: { vtl: VTL; goto: (v: string) => void }) {
  const { snap, events, jobs, ticks } = vtl
  const [libRows, setLibRows] = useState<LibraryRow[]>([])
  const [poolRows, setPoolRows] = useState<Pool[]>([])
  const [system, setSystem] = useState<SystemStatus | null>(null)

  // Keep the top-strip system storage (zpool alloc/size + global dedupe)
  // fresh: these come from /api/devices, not the SSE-driven snapshot, so
  // without a periodic reload they'd freeze at mount while data is
  // written. 10s matches the snapshot poll so all the numbers move
  // together. Library/pool rows change rarely but are cheap to refresh.
  useEffect(() => {
    const load = () => {
      api.libraryRows().then(setLibRows).catch(() => {})
      api.pools().then(setPoolRows).catch(() => {})
      api.devices().then((r) => setSystem(r.system)).catch(() => {})
    }
    load()
    const t = window.setInterval(load, 10_000)
    return () => window.clearInterval(t)
  }, [])

  if (!snap) return null
  const active = jobs.filter(isActive)

  // library id -> its pool's live stats (home_pool -> pool name -> snapshot stats)
  const statsByName = new Map(snap.pools.map((p) => [p.name, p]))
  const poolNameById = new Map(poolRows.map((p) => [p.id, p.name]))
  const poolOf = (libID: number): PoolStats | undefined => {
    const row = libRows.find((r) => r.id === libID)
    if (!row) return undefined
    const name = poolNameById.get(row.home_pool)
    return name ? statsByName.get(name) : undefined
  }

  // System-wide storage: physical is the zpool allocation (dedup is
  // global, so per-pool physical isn't meaningful); logical is the sum of
  // datasets' pre-dedup/compress data. Overall saving = logical : physical.
  const physUsed = system?.alloc_bytes ?? 0
  const physTotal = system?.size_bytes ?? 0
  const logicalTotal = snap.pools.reduce((a, p) => a + p.logical_bytes, 0)
  const physPct = physTotal > 0 ? (100 * physUsed) / physTotal : 0
  const saving = fmtRatio(logicalTotal, physUsed)
  const dedup = system?.dedup_ratio ?? 0
  const sessions = snap.fabrics?.fc.sessions ?? 0

  return (
    <div className="space-y-6">
      <OffsiteNag goto={goto} />

      <div className="grid grid-cols-2 gap-3 md:grid-cols-4">
        <Stat label="storage used"
          value={`${fmtBytes(physUsed)} / ${fmtBytes(physTotal)}`}
          bar={physPct} warn={physPct > 70} crit={physPct > 85} />
        <Stat label="space saving (logical : physical)"
          value={saving && logicalTotal > physUsed ? saving : '—'}
          sub={dedup ? `dedupe ${dedup.toFixed(2)}× · ${fmtBytes(logicalTotal)} logical` : undefined} accent />
        <Stat label="active jobs" value={String(active.length)} />
        <Stat label="host sessions" value={String(sessions)} />
      </div>

      {active.length > 0 && (
        <Panel title="Active jobs" right={
          <button onClick={() => goto('jobs')} className="text-xs text-sky-400 hover:text-sky-300">all jobs →</button>
        }>
          <div className="space-y-3">
            {active.map((j) => (
              <div key={j.id} className="rounded border border-zinc-800 bg-zinc-900/60 px-3 py-2">
                <div className="flex items-center justify-between text-sm">
                  <span className="font-mono text-zinc-200">
                    #{j.id} {j.kind} <span className="text-amber-300">{j.cart_label}</span>
                  </span>
                  <StateBadge state={j.state} />
                </div>
                <div className="mt-2 flex items-center gap-3">
                  <div className="flex-1"><Progress done={j.bytes_done} total={j.bytes_total} /></div>
                  <span className="shrink-0 font-mono text-[11px] text-zinc-500">
                    {fmtBytes(j.bytes_done)} / {fmtBytes(j.bytes_total)}
                    {j.chunks_total > 0 && ` · chunk ${j.chunks_done}/${j.chunks_total}`}
                  </span>
                </div>
              </div>
            ))}
          </div>
        </Panel>
      )}

      <div className="grid grid-cols-1 gap-4 xl:grid-cols-2">
        {snap.libraries.map((l) => (
          <LibraryCard key={l.library.id} l={l} pool={poolOf(l.library.id)}
            evicted={snap.cartridges.filter((c) => c.library === l.library.id && c.local_state === 'evicted').length}
            ticks={ticks} goto={goto} />
        ))}
      </div>

      <TrendPanel pools={snap.pools} poolHist={vtl.poolHist} />

      <ActivityPanel events={events} />
    </div>
  )
}

// LibraryCard — one library, self-contained: its pool, its drives, its
// occupancy. Serial stays visible as subtext (the host-facing identity).
function LibraryCard({ l, pool, evicted, ticks, goto }: {
  l: LibrarySnapshot; pool?: PoolStats; evicted: number
  ticks: VTL['ticks']; goto: (v: string) => void
}) {
  const lib = l.library
  const storage = (l.slots ?? []).filter((s) => s.kind === 'storage')
  const occupied = storage.filter((s) => s.label).length

  return (
    <div className="rounded-lg border border-zinc-800 bg-zinc-900/40 p-4">
      <div className="flex items-baseline justify-between">
        <button onClick={() => goto('library')} className="text-left">
          <span className="text-sm font-semibold text-zinc-100 hover:text-sky-300">{lib.name}</span>
          <span className="ml-2 text-xs text-zinc-500">{modelLabel(lib.product)} · {lib.serial}</span>
        </button>
        {!lib.live && (
          <span className="rounded bg-amber-500/20 px-1.5 py-0.5 font-mono text-[10px] text-amber-300">
            awaiting activation
          </span>
        )}
      </div>

      {pool && (
        <div className="mt-3 rounded border border-zinc-800 bg-zinc-950/40 px-3 py-2">
          <div className="flex items-center justify-between text-xs">
            <span className="text-zinc-500">pool {pool.name}</span>
            <span className="font-mono text-zinc-400">{fmtBytes(pool.logical_bytes)} logical</span>
          </div>
          <div className="mt-1.5 flex gap-4 font-mono text-[11px] text-zinc-500">
            <span>compression <span className="text-emerald-400">{pool.compress_ratio ? `${pool.compress_ratio.toFixed(2)}×` : '—'}</span></span>
            <span>dedupe <span className="text-emerald-400">{pool.dedup_ratio ? `${pool.dedup_ratio.toFixed(2)}×` : '—'}</span></span>
          </div>
        </div>
      )}

      <div className="mt-3 grid grid-cols-1 gap-2 md:grid-cols-2">
        {(l.drives ?? []).map((d) => (
          <DriveTile key={d.index} d={d}
            samples={(ticks[`${d.library}:${d.index}`] ?? []).map((s) => s.w + s.r)} />
        ))}
      </div>

      {lib.live && (
        <div className="mt-3 flex gap-4 text-[11px] text-zinc-500">
          <span>{occupied}/{storage.length} slots occupied</span>
          {evicted > 0 && (
            <span className="text-red-400"
              title="evicted stubs — mounting one reads as blank on the host">
              {evicted} evicted stub{evicted === 1 ? '' : 's'}
            </span>
          )}
        </div>
      )}
    </div>
  )
}

function DriveTile({ d, samples }: { d: Drive; samples: number[] }) {
  // ops/s over the daemon's 2s flush windows; honest units — mhVTL
  // journal counts operations, not bytes.
  const last = samples.length ? samples[samples.length - 1] / 2 : 0
  return (
    <div className={`rounded border border-zinc-800 bg-zinc-900/60 px-3 py-2 ${
      d.activity === 'writing' ? 'drive-writing' : d.activity === 'reading' ? 'drive-reading' : ''
    }`}>
      <div className="flex items-center justify-between">
        <div className="text-xs font-medium text-zinc-300">
          Drive {d.index} <span className="text-zinc-600">· {d.serial}</span>
        </div>
        <span className={`text-[10px] font-semibold uppercase tracking-wide ${
          d.activity === 'writing' ? 'text-emerald-400'
            : d.activity === 'reading' ? 'text-sky-400' : 'text-zinc-600'
        }`}>{d.activity}</span>
      </div>
      <div className="mt-1.5 flex items-center justify-between text-xs">
        {d.loaded
          ? <span className="rounded bg-zinc-800 px-1.5 py-0.5 font-mono text-amber-300">{d.loaded}</span>
          : <span className="text-zinc-600">empty</span>}
        <span className="font-mono text-[10px] text-zinc-500">
          {last > 0 ? `${last.toFixed(0)} ops/s` : 'idle'}
        </span>
      </div>
      <div className="mt-2">
        <Spark points={samples} color={d.activity === 'reading' ? '#60a5fa' : '#34d399'} height={20} />
      </div>
    </div>
  )
}

// TrendPanel — ONE system-wide chart with a pool selector (design
// decision: cards stay compact). Persisted seed is fetched per pool on
// first selection and merged with the live SSE series.
function TrendPanel({ pools, poolHist }: { pools: PoolStats[]; poolHist: Record<string, LivePoolSample[]> }) {
  const [sel, setSel] = useState(pools[0]?.name ?? '')
  const [seeds, setSeeds] = useState<Record<string, LivePoolSample[]>>({})
  const pool = sel || (pools[0]?.name ?? '')

  useEffect(() => {
    if (!pool || seeds[pool]) return
    fetch(`/api/pool/history?hours=24&pool=${encodeURIComponent(pool)}`)
      .then((r) => (r.ok ? r.json() : []))
      .then((samples: { ts: string; vdo_used_bytes: number; logical_bytes: number }[]) => {
        if (!Array.isArray(samples)) return
        setSeeds((prev) => ({
          ...prev,
          [pool]: samples.map((s) => ({ t: Date.parse(s.ts), phys: s.vdo_used_bytes, logical: s.logical_bytes })),
        }))
      })
      .catch(() => {})
  }, [pool, seeds])

  const seed = seeds[pool] ?? []
  const cutoff = seed.length ? seed[seed.length - 1].t : 0
  const trend = [...seed, ...(poolHist[pool] ?? []).filter((p) => p.t > cutoff)]

  return (
    <Panel title="Capacity trend (24 h)" right={
      pools.length > 1 ? (
        <div className="flex overflow-hidden rounded border border-zinc-700 text-xs">
          {pools.map((p) => (
            <button key={p.name} onClick={() => setSel(p.name)}
              className={`px-2 py-0.5 ${pool === p.name ? 'bg-zinc-700 text-zinc-100' : 'bg-zinc-900 text-zinc-500 hover:text-zinc-300'}`}>
              {p.name}
            </button>
          ))}
        </div>
      ) : undefined
    }>
      <AreaChart
        series={[
          { name: 'physical', color: '#38bdf8', points: trend.map((s) => ({ t: s.t, v: s.phys })) },
          { name: 'logical', color: '#34d399', points: trend.map((s) => ({ t: s.t, v: s.logical })) },
        ]}
        fmtY={fmtBytes}
      />
    </Panel>
  )
}

// OffsiteNag: S3 is optional, but a site with no remote has no offsite
// copy — losing this box loses the backups on it. Dismissal is a
// persisted (and audited) setting; deleting the last remote re-arms it
// server-side.
const K_NAG = 'nag.no_offsite_dismissed'

function OffsiteNag({ goto }: { goto: (v: string) => void }) {
  const [show, setShow] = useState(false)
  const [settings, setSettings] = useState<Settings>({})

  useEffect(() => {
    Promise.all([api.remotes(), api.settings()])
      .then(([remotes, s]) => {
        setSettings(s)
        setShow(remotes.length === 0 && s[K_NAG] !== '1')
      })
      .catch(() => {})
  }, [])

  const dismiss = () =>
    api.saveSettings({ ...settings, [K_NAG]: '1' })
      .then(() => setShow(false))
      .catch((e: Error) => notify(e.message, true))

  if (!show) return null
  return (
    <div className="flex flex-wrap items-center justify-between gap-3 rounded border border-amber-700/50 bg-amber-950/30 px-4 py-3 text-sm text-amber-200">
      <span>
        <span className="font-semibold">No offsite export configured.</span>{' '}
        If this OpenVTL instance is lost, the backups stored on it are lost with it.
        Add an S3 remote to keep vaulted media offsite.
      </span>
      <span className="flex shrink-0 items-center gap-2">
        <Button kind="primary" onClick={() => goto('s3')}>Configure S3</Button>
        <Button needsAdmin onClick={dismiss} title="hide until the last remote is removed again">Dismiss</Button>
      </span>
    </div>
  )
}

// ActivityPanel: live session feed by default; flips to the persisted
// journal (event_log) with a kind filter and search — the journal viewer.
// Search works in two tiers: typing filters the loaded recent rows in place
// (instant), and "Search all history" queries the whole event_log server
// side so matches older than the recent window still surface.
function ActivityPanel({ events }: { events: VTL['events'] }) {
  const [mode, setMode] = useState<'live' | 'journal'>('live')
  const [journal, setJournal] = useState<LoggedEvent[]>([])
  const [kind, setKind] = useState('all')
  const [q, setQ] = useState('')
  const [deep, setDeep] = useState<LoggedEvent[] | null>(null) // full-history results, or null for the live/in-view feed
  const [searching, setSearching] = useState(false)
  const LIMIT = 500

  useEffect(() => {
    if (mode !== 'journal') return
    api.journal().then(setJournal).catch(() => setJournal([]))
    const t = window.setInterval(() => api.journal().then(setJournal).catch(() => {}), 15_000)
    return () => window.clearInterval(t)
  }, [mode])

  const kinds = ['all', ...Array.from(new Set(journal.map((e) => e.kind))).sort()]
  const needle = q.trim().toLowerCase()

  // In-view search: filter the loaded recent rows by kind + free text.
  const inView = journal.filter((e) =>
    (kind === 'all' || e.kind === kind) &&
    (needle === '' ||
      e.kind.toLowerCase().includes(needle) ||
      e.subject.toLowerCase().includes(needle) ||
      (e.detail ?? '').toLowerCase().includes(needle)))

  const runDeep = () => {
    if (needle === '') { setDeep(null); return }
    setSearching(true)
    api.searchJournal(q.trim(), kind, LIMIT)
      .then(setDeep)
      .catch((e: Error) => { notify(e.message, true); setDeep(null) })
      .finally(() => setSearching(false))
  }
  // Editing the query or kind drops back to the in-view feed until the user
  // runs a fresh full-history search.
  const onQ = (v: string) => { setQ(v); setDeep(null) }
  const onKind = (v: string) => { setKind(v); setDeep(null) }

  const shown = deep ?? inView

  return (
    <Panel
      title="Activity"
      right={
        <div className="flex items-center gap-2 text-xs">
          {mode === 'journal' && (
            <select value={kind} onChange={(e) => onKind(e.target.value)}
              className="rounded border border-zinc-700 bg-zinc-900 px-1.5 py-0.5 text-zinc-300">
              {kinds.map((k) => <option key={k}>{k}</option>)}
            </select>
          )}
          <div className="flex overflow-hidden rounded border border-zinc-700">
            {(['live', 'journal'] as const).map((m) => (
              <button key={m} onClick={() => setMode(m)}
                className={`px-2 py-0.5 ${mode === m ? 'bg-zinc-700 text-zinc-100' : 'bg-zinc-900 text-zinc-500 hover:text-zinc-300'}`}>
                {m}
              </button>
            ))}
          </div>
        </div>
      }
    >
      {mode === 'journal' && (
        <div className="mb-2 flex items-center gap-2 text-xs">
          <input
            className="flex-1 rounded border border-zinc-700 bg-zinc-900 px-2 py-1 text-zinc-200 placeholder:text-zinc-600"
            placeholder="search the log…"
            value={q}
            onChange={(e) => onQ(e.target.value)}
            onKeyDown={(e) => { if (e.key === 'Enter') runDeep() }}
          />
          <button onClick={runDeep} disabled={searching || needle === ''}
            title="search the whole log, not just the recent rows"
            className="shrink-0 rounded border border-zinc-700 bg-zinc-900 px-2 py-1 text-zinc-300 hover:text-zinc-100 disabled:opacity-40">
            {searching ? 'searching…' : 'Search all history'}
          </button>
        </div>
      )}
      {mode === 'journal' && deep && (
        <div className="mb-2 flex items-center justify-between text-[11px] text-zinc-500">
          <span>{deep.length}{deep.length >= LIMIT ? '+' : ''} match{deep.length === 1 ? '' : 'es'} across the full log</span>
          <button onClick={() => setDeep(null)} className="text-sky-400 hover:text-sky-300">back to recent</button>
        </div>
      )}
      <div className="max-h-[420px] space-y-1.5 overflow-y-auto text-xs">
        {mode === 'live' && events.length === 0 &&
          <div className="text-zinc-600">no events yet this session</div>}
        {mode === 'live' && events.map((e, i) => (
          <FeedRow key={i} ts={e.ts} kind={e.kind} text={String(e.data?.detail ?? e.subject)} subject={e.subject} />
        ))}
        {mode === 'journal' && shown.length === 0 &&
          <div className="text-zinc-600">{needle ? 'no matching events' : 'no events logged yet'}</div>}
        {mode === 'journal' && shown.map((e) => (
          <FeedRow key={e.id} ts={e.ts} kind={e.kind} text={e.detail || e.subject} subject={e.subject} />
        ))}
      </div>
    </Panel>
  )
}

function FeedRow({ ts, kind, text, subject }: { ts: string; kind: string; text: string; subject: string }) {
  return (
    <div className="flex gap-2 border-b border-zinc-800/60 pb-1.5">
      <span className="shrink-0 font-mono text-zinc-600">{fmtTime(ts)}</span>
      <span className="shrink-0 rounded bg-zinc-800 px-1.5 font-mono text-[10px] leading-5 text-sky-300">
        {kind.replace('mhvtl_', '')}
      </span>
      <span className="truncate text-zinc-400" title={`${subject}: ${text}`}>{text}</span>
    </div>
  )
}
