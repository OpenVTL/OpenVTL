import { useEffect, useRef, useState } from 'react'
import { api, broadcastAuthRequired } from './api'
import type { Job, Snapshot, VTLEvent } from './types'

// One throughput sample per drive_activity flush (the daemon emits
// every 2s while a drive moves data).
export interface TickSample {
  t: number // epoch ms
  w: number // write ops in the window
  r: number // read ops
}

// One capacity sample per pool_stats event (10s cadence).
export interface PoolSample {
  t: number
  phys: number // real disk share (used ÷ global dedupratio since v0.9)
  logical: number // pre-dedup/compress (zfs `logicalused`)
}

const TICK_WINDOW = 90 // samples kept per drive (~3 min at 2s)
const POOL_WINDOW = 360 // ~1 h at 10s

// A maintenance-window operation in flight (Activate, delete a live
// library, data-plane restart) — streamed step-by-step over SSE so the
// UI shows a live checklist instead of a button that "just sits".
export interface MaintState {
  label: string
  steps: string[]
  // 'rebooting' = the op finished by rebooting the appliance; the SSE
  // stream is about to drop and the overlay holds a "reconnecting" state
  // until the daemon returns (see the connection watcher below).
  status: 'running' | 'rebooting' | 'done' | 'failed'
  detail?: string
}

// Live VTL state: full snapshot from REST, kept fresh by SSE-triggered
// debounced refetches; SSE also feeds the activity ticker, the per-
// drive throughput series, the capacity trend and the live job table.
export function useVTL() {
  const [snap, setSnap] = useState<Snapshot | null>(null)
  const [events, setEvents] = useState<VTLEvent[]>([])
  const [jobs, setJobs] = useState<Job[]>([])
  // per-drive throughput keyed "<library>:<index>"
  const [ticks, setTicks] = useState<Record<string, TickSample[]>>({})
  // capacity trend per pool name
  const [poolHist, setPoolHist] = useState<Record<string, PoolSample[]>>({})
  const [connected, setConnected] = useState(false)
  const [maint, setMaint] = useState<MaintState | null>(null)
  const refetchTimer = useRef<number | undefined>(undefined)
  const maintClear = useRef<number | undefined>(undefined)
  // Latest maint state + whether the SSE stream dropped while a
  // maintenance op was in flight — read by the connection watcher, which
  // must not re-subscribe SSE, so they live in refs.
  const maintRef = useRef<MaintState | null>(null)
  const maintDropped = useRef(false)
  maintRef.current = maint

  useEffect(() => {
    let stop = false

    const refetch = async () => {
      try {
        const r = await fetch('/api/status')
        if (r.status === 401) broadcastAuthRequired() // session expired mid-view
        if (r.ok && !stop) setSnap(await r.json())
      } catch { /* next event or interval retries */ }
    }
    const refetchJobs = async () => {
      try {
        if (!stop) setJobs(await api.jobs())
      } catch { /* transient */ }
    }
    const debouncedRefetch = () => {
      window.clearTimeout(refetchTimer.current)
      refetchTimer.current = window.setTimeout(refetch, 250)
    }

    refetch()
    refetchJobs()
    // Seed the capacity trend from persisted samples (dedupe_stats) so
    // the chart doesn't start empty after a daemon or page restart.
    // Rows carry their pool name; the default query serves the first
    // pool, per-pool seeds come with the Storage view.
    fetch('/api/pool/history?hours=24')
      .then((r) => (r.ok ? r.json() : []))
      .then((samples: { ts: string; pool?: string; vdo_used_bytes: number; logical_bytes: number }[]) => {
        if (stop || !Array.isArray(samples) || samples.length === 0) return
        const pool = samples[0].pool ?? ''
        setPoolHist((prev) => {
          const live = prev[pool] ?? []
          const seeded = samples.map((s) => ({
            t: Date.parse(s.ts), phys: s.vdo_used_bytes, logical: s.logical_bytes,
          }))
          const cutoff = seeded[seeded.length - 1].t
          return { ...prev, [pool]: [...seeded, ...live.filter((p) => p.t > cutoff)].slice(-POOL_WINDOW) }
        })
      })
      .catch(() => {})
    const poll = window.setInterval(refetch, 10_000) // SSE-loss safety net

    const es = new EventSource('/api/events')
    es.onopen = () => setConnected(true)
    es.onerror = () => setConnected(false)

    const feedKinds = [
      'cart_moved', 'drive_loaded', 'drive_unloaded',
      'fc_state', 'mhvtl_load', 'mhvtl_unload', 'mhvtl_move',
      'mhvtl_filemarks', 'mhvtl_pr',
      'export_done', 'import_done', 'evict_done', 'vault_detected', 'policy_evict',
    ]
    for (const k of feedKinds) {
      es.addEventListener(k, (m) => {
        const ev = JSON.parse((m as MessageEvent).data) as VTLEvent
        setEvents((prev) => [ev, ...prev].slice(0, 80))
        debouncedRefetch()
      })
    }

    es.addEventListener('drive_activity', (m) => {
      const ev = JSON.parse((m as MessageEvent).data) as VTLEvent
      // subject is "drive:<library>:<index>"
      const key = String(ev.subject).replace('drive:', '')
      const w = Number(ev.data?.writes_delta ?? 0)
      const r = Number(ev.data?.reads_delta ?? 0)
      setTicks((prev) => {
        const s = [...(prev[key] ?? []), { t: Date.parse(ev.ts), w, r }]
        return { ...prev, [key]: s.slice(-TICK_WINDOW) }
      })
      debouncedRefetch()
    })

    es.addEventListener('pool_stats', (m) => {
      const ev = JSON.parse((m as MessageEvent).data) as VTLEvent
      const pool = String(ev.subject)
      setPoolHist((prev) => ({
        ...prev,
        [pool]: [...(prev[pool] ?? []), {
          t: Date.parse(ev.ts),
          // phys_est since the v0.9 fix (real disk share incl. global
          // dedup); vdo_used kept as fallback for an old daemon.
          phys: Number(ev.data?.phys_est ?? ev.data?.vdo_used ?? 0),
          logical: Number(ev.data?.logical ?? 0),
        }].slice(-POOL_WINDOW),
      }))
      debouncedRefetch()
    })

    // Maintenance-window progress: the daemon streams one maint_step
    // per completed step (the first carries step:"started"), then a
    // maint_done. The panel shows the live checklist and self-clears a
    // few seconds after completion.
    es.addEventListener('maint_step', (m) => {
      const ev = JSON.parse((m as MessageEvent).data) as VTLEvent
      const label = String(ev.data?.label ?? ev.subject)
      const step = String(ev.data?.step ?? '')
      window.clearTimeout(maintClear.current)
      setMaint((prev) => {
        // Every op begins with a 'started' step (see maintStep on the
        // daemon), which takes over the window. Any other step whose label
        // isn't the one on screen belongs to a superseded or already-
        // finished op — two maintenance actions overlapping — so ignore it
        // rather than let the window flip-flop between them.
        if (step === 'started') {
          return { label, steps: [], status: 'running' }
        }
        if (!prev || prev.label !== label) {
          return prev
        }
        return { ...prev, steps: [...prev.steps, step] }
      })
    })
    es.addEventListener('maint_done', (m) => {
      const ev = JSON.parse((m as MessageEvent).data) as VTLEvent
      const label = String(ev.data?.label ?? ev.subject)
      const ok = ev.data?.ok !== false
      const rebooting = ev.data?.rebooting === true
      window.clearTimeout(maintClear.current)
      setMaint((prev) => {
        if (!prev || prev.label !== label) return prev
        const detail = String(ev.data?.detail ?? '')
        if (!ok) return { ...prev, status: 'failed', detail }
        return { ...prev, status: rebooting ? 'rebooting' : 'done', detail }
      })
      debouncedRefetch()
      // No auto-clear: the window stays until the operator dismisses it (a
      // plain success or failure) or until the appliance reconnects (a
      // reboot, resolved by the connection watcher). Either way it never
      // vanishes mid-maintenance and leaves the operator on a dead page.
    })

    es.addEventListener('job_update', (m) => {
      const j = (JSON.parse((m as MessageEvent).data) as VTLEvent).data as unknown as Job
      if (!j || typeof j.id !== 'number') return
      setJobs((prev) => {
        const i = prev.findIndex((p) => p.id === j.id)
        if (i === -1) return [j, ...prev]
        const next = [...prev]
        next[i] = j
        return next
      })
    })

    return () => {
      stop = true
      es.close()
      window.clearInterval(poll)
      window.clearTimeout(refetchTimer.current)
      window.clearTimeout(maintClear.current)
    }
  }, [])

  // Connection watcher — keeps the maintenance window alive across the
  // disruption. While a running/rebooting op is in flight and the SSE
  // stream drops, the appliance is (re)starting: the overlay renders a
  // live "reconnecting" state off `connected`. When the stream comes back
  // we resolve the op to "back online" so the operator sees it finish
  // instead of a stale spinner (a reboot never replays its maint_done).
  useEffect(() => {
    const m = maintRef.current
    if (!m) { maintDropped.current = false; return }
    const active = m.status === 'running' || m.status === 'rebooting'
    if (!connected) {
      if (active) maintDropped.current = true
      return
    }
    if (maintDropped.current && active) {
      maintDropped.current = false
      setMaint({ ...m, status: 'done', detail: 'Appliance is back online.' })
    }
  }, [connected])

  return { snap, events, jobs, ticks, poolHist, connected, maint, dismissMaint: () => setMaint(null) }
}

export type VTL = ReturnType<typeof useVTL>
