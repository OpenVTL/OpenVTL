import { useEffect, useState } from 'react'
import { useAuthCtx } from '../useAuth'
import type { JobState } from '../types'

// modelLabel renders a library's SCSI product id as the canonical
// human-facing model name — "TS3100/TS3200 (3573)" / "TS3500 (3584)",
// matching the type WRKHDWRSC shows host-side (3573-040 / 3584-040).
// The raw product id stays a wire/technical identifier (device.conf,
// status.json, conformance docs).
export const modelLabel = (product: string): string =>
  product === '3573-TL' ? 'TS3100/TS3200 (3573)'
    : product.startsWith('03584') ? 'TS3500 (3584)'
      : product

export function Badge({ ok, label, title }: { ok: boolean; label: string; title?: string }) {
  return (
    <span
      title={title}
      className={`inline-flex items-center gap-1.5 rounded-full px-2.5 py-1 font-medium ${
        ok ? 'bg-emerald-500/10 text-emerald-400' : 'bg-red-500/10 text-red-400'
      }`}
    >
      <span className={`h-1.5 w-1.5 rounded-full ${ok ? 'bg-emerald-400' : 'bg-red-400 animate-pulse'}`} />
      {label}
    </span>
  )
}

export function Panel({ title, right, children, className = '' }: {
  title?: string; right?: React.ReactNode; children: React.ReactNode; className?: string
}) {
  return (
    <div className={`rounded-lg border border-zinc-800 bg-zinc-900/40 p-4 ${className}`}>
      {(title || right) && (
        <div className="mb-3 flex items-center justify-between">
          {title && <h2 className="text-sm font-medium text-zinc-300">{title}</h2>}
          {right}
        </div>
      )}
      {children}
    </div>
  )
}

export function Stat({ label, value, sub, bar, accent, warn, crit }: {
  label: string; value: string; sub?: string; bar?: number; accent?: boolean; warn?: boolean; crit?: boolean
}) {
  const barColor = crit ? 'bg-red-500' : warn ? 'bg-amber-500' : 'bg-sky-600'
  return (
    <div className="rounded-lg border border-zinc-800 bg-zinc-900/60 px-4 py-3">
      <div className="text-[11px] uppercase tracking-wider text-zinc-500">{label}</div>
      <div className={`mt-1 text-lg font-semibold ${accent ? 'text-emerald-400' : 'text-zinc-100'}`}>{value}</div>
      {sub && <div className="mt-0.5 font-mono text-[10px] text-zinc-500">{sub}</div>}
      {bar !== undefined && (
        <div className="mt-2 h-1 overflow-hidden rounded bg-zinc-800">
          <div className={`h-full ${barColor}`} style={{ width: `${Math.min(100, bar)}%` }} />
        </div>
      )}
    </div>
  )
}

// Sparkline: one polyline scaled to its own max — shape over absolutes.
export function Spark({ points, color = '#34d399', height = 28 }: {
  points: number[]; color?: string; height?: number
}) {
  if (points.length < 2) {
    return <div style={{ height }} className="flex items-center text-[10px] text-zinc-700">no samples</div>
  }
  const w = 160
  const max = Math.max(...points, 1)
  const step = w / (points.length - 1)
  const pts = points.map((v, i) => `${(i * step).toFixed(1)},${(height - (v / max) * (height - 2) - 1).toFixed(1)}`).join(' ')
  return (
    <svg width="100%" height={height} viewBox={`0 0 ${w} ${height}`} preserveAspectRatio="none" className="block">
      <polyline points={pts} fill="none" stroke={color} strokeWidth="1.5" vectorEffect="non-scaling-stroke" />
    </svg>
  )
}

// Area chart with two series (physical / logical) and a y max — the
// capacity trend. Hand-rolled: two <path> fills, no chart dependency.
export function AreaChart({ series, height = 120, yMax, fmtY }: {
  series: { name: string; color: string; points: { t: number; v: number }[] }[]
  height?: number
  yMax?: number
  fmtY?: (v: number) => string
}) {
  const all = series.flatMap((s) => s.points)
  if (all.length < 2) {
    return <div style={{ height }} className="flex items-center justify-center text-xs text-zinc-600">collecting samples…</div>
  }
  const w = 600
  const t0 = Math.min(...all.map((p) => p.t))
  const t1 = Math.max(...all.map((p) => p.t))
  const max = (yMax ?? Math.max(...all.map((p) => p.v)) * 1.15) || 1
  const x = (t: number) => (t1 === t0 ? 0 : ((t - t0) / (t1 - t0)) * w)
  const y = (v: number) => height - (v / max) * (height - 8) - 2
  return (
    <div>
      <svg width="100%" height={height} viewBox={`0 0 ${w} ${height}`} preserveAspectRatio="none" className="block">
        {series.map((s) => {
          if (s.points.length < 2) return null
          const line = s.points.map((p) => `${x(p.t).toFixed(1)},${y(p.v).toFixed(1)}`).join(' L')
          const area = `M${x(s.points[0].t).toFixed(1)},${height} L${line} L${x(s.points[s.points.length - 1].t).toFixed(1)},${height} Z`
          return (
            <g key={s.name}>
              <path d={area} fill={s.color} opacity="0.12" />
              <path d={`M${line}`} fill="none" stroke={s.color} strokeWidth="1.5" vectorEffect="non-scaling-stroke" />
            </g>
          )
        })}
      </svg>
      <div className="mt-1 flex justify-between text-[10px] text-zinc-600">
        <span>{new Date(t0).toLocaleTimeString()}</span>
        <span className="flex gap-3">
          {series.map((s) => (
            <span key={s.name} className="inline-flex items-center gap-1">
              <span className="inline-block h-1.5 w-1.5 rounded-full" style={{ background: s.color }} />
              {s.name}{s.points.length ? ` ${fmtY ? fmtY(s.points[s.points.length - 1].v) : ''}` : ''}
            </span>
          ))}
        </span>
        <span>{new Date(t1).toLocaleTimeString()}</span>
      </div>
    </div>
  )
}

const stateStyle: Record<string, string> = {
  done: 'bg-emerald-500/10 text-emerald-400',
  failed: 'bg-red-500/10 text-red-400',
  cancelled: 'bg-zinc-500/10 text-zinc-400',
  uploading: 'bg-sky-500/10 text-sky-300',
  fetching: 'bg-sky-500/10 text-sky-300',
  chunking: 'bg-amber-500/10 text-amber-300',
  verifying: 'bg-violet-500/10 text-violet-300',
}

export function StateBadge({ state }: { state: JobState | string }) {
  const cls = stateStyle[state] ?? 'bg-amber-500/10 text-amber-300'
  const active = !['done', 'failed', 'cancelled'].includes(state)
  return (
    <span className={`inline-flex items-center gap-1.5 rounded px-2 py-0.5 font-mono text-[11px] ${cls}`}>
      {active && <span className="h-1.5 w-1.5 animate-pulse rounded-full bg-current" />}
      {state}
    </span>
  )
}

export function Progress({ done, total }: { done: number; total: number }) {
  const pct = total > 0 ? Math.min(100, (done / total) * 100) : 0
  return (
    <div className="h-1.5 w-full overflow-hidden rounded bg-zinc-800">
      <div className="h-full bg-sky-500 transition-[width] duration-500" style={{ width: `${pct}%` }} />
    </div>
  )
}

// Tiny toast: views call notify(); the host renders <Toasts/> once.
type Toast = { id: number; msg: string; err: boolean }
const listeners = new Set<(t: Toast) => void>()
let nextId = 1

export function notify(msg: string, err = false) {
  for (const l of listeners) l({ id: nextId++, msg, err })
}

export function Toasts() {
  const [toasts, setToasts] = useState<Toast[]>([])
  useEffect(() => {
    const on = (t: Toast) => {
      setToasts((prev) => [...prev, t])
      window.setTimeout(() => setToasts((prev) => prev.filter((x) => x.id !== t.id)), 5000)
    }
    listeners.add(on)
    return () => { listeners.delete(on) }
  }, [])
  return (
    <div className="pointer-events-none fixed bottom-4 right-4 z-50 space-y-2">
      {toasts.map((t) => (
        <div key={t.id}
          className={`pointer-events-auto max-w-md rounded-lg border px-4 py-2.5 text-sm shadow-lg ${
            t.err ? 'border-red-500/40 bg-red-950/90 text-red-200' : 'border-zinc-700 bg-zinc-900/95 text-zinc-200'
          }`}>
          {t.msg}
        </div>
      ))}
    </div>
  )
}

// needsAdmin: mutating controls set it; for read-only users the button
// renders disabled with an explanatory tooltip instead of letting the
// API refuse (the v0.4 lesson: never offer an action that will be
// rejected without saying why).
export function Button({ children, onClick, kind = 'default', disabled, title, needsAdmin }: {
  children: React.ReactNode; onClick?: () => void
  kind?: 'default' | 'primary' | 'danger'; disabled?: boolean; title?: string
  needsAdmin?: boolean
}) {
  const { admin } = useAuthCtx()
  const roleBlocked = Boolean(needsAdmin) && !admin
  const styles = {
    default: 'border-zinc-700 bg-zinc-800/80 text-zinc-200 hover:bg-zinc-700',
    primary: 'border-sky-700 bg-sky-600/20 text-sky-300 hover:bg-sky-600/35',
    danger: 'border-red-800 bg-red-600/10 text-red-300 hover:bg-red-600/25',
  }
  return (
    <button onClick={onClick} disabled={disabled || roleBlocked}
      title={roleBlocked ? 'admin role required' : title}
      className={`rounded border px-2.5 py-1 text-xs font-medium transition-colors disabled:cursor-not-allowed disabled:opacity-40 ${styles[kind]}`}>
      {children}
    </button>
  )
}

export function Field({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <label className="block text-xs">
      <span className="mb-1 block text-zinc-500">{label}</span>
      {children}
    </label>
  )
}

export const inputCls =
  'w-full rounded border border-zinc-700 bg-zinc-900 px-2 py-1.5 text-sm text-zinc-200 outline-none focus:border-sky-600'
