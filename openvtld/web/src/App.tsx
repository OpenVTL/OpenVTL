import { useEffect, useState } from 'react'
import { useVTL } from './useVTL'
import { AuthContext, useAuth, useAuthCtx } from './useAuth'
import { Badge, Toasts, modelLabel } from './components/ui'
import { isActive } from './types'
import type { Fabrics } from './types'
import Dashboard from './views/Dashboard'
import Library from './views/Library'
import Jobs from './views/Jobs'
import Login from './views/Login'
import S3 from './views/S3'
import Settings from './views/Settings'
import Storage from './views/Storage'
import Targets from './views/Targets'

// Hash routes keep their historical ids (#/library, #/targets, #/s3
// bookmarks stay valid); LABELS carries the v0.7 display names.
const VIEWS = ['dashboard', 'storage', 'library', 'targets', 's3', 'jobs', 'settings'] as const
type View = (typeof VIEWS)[number]

const LABELS: Record<View, string> = {
  dashboard: 'Dashboard',
  storage: 'Storage',
  library: 'Libraries',
  targets: 'Access',
  s3: 'Offsite',
  jobs: 'Jobs',
  settings: 'Settings',
}

function viewFromHash(): View {
  const h = window.location.hash.replace(/^#\/?/, '')
  return (VIEWS as readonly string[]).includes(h) ? (h as View) : 'dashboard'
}

// App = auth gate; Shell (below) is the actual UI and only mounts once
// authenticated, so SSE/status polling never runs anonymous.
export default function App() {
  const auth = useAuth()
  switch (auth.state.phase) {
    case 'loading':
      return <div className="flex h-screen items-center justify-center text-zinc-500">connecting to openvtld…</div>
    case 'setup':
      return <Login mode="setup" onSignedIn={auth.signedIn} />
    case 'anon':
      return <Login mode="login" onSignedIn={auth.signedIn} />
    case 'authed':
      return (
        <AuthContext.Provider value={{
          user: auth.state.user,
          admin: auth.state.user.role === 'admin',
          logout: auth.logout,
        }}>
          <Shell />
        </AuthContext.Provider>
      )
  }
}

function Shell() {
  const vtl = useVTL()
  const { user, logout } = useAuthCtx()
  const [view, setView] = useState<View>(viewFromHash)

  useEffect(() => {
    const on = () => setView(viewFromHash())
    window.addEventListener('hashchange', on)
    return () => window.removeEventListener('hashchange', on)
  }, [])
  const goto = (v: string) => { window.location.hash = `/${v}` }

  if (!vtl.snap) {
    return (
      <div className="flex h-screen items-center justify-center text-zinc-500">
        connecting to openvtld…
      </div>
    )
  }

  const activeJobs = vtl.jobs.filter(isActive).length
  const evictedCount = vtl.snap.cartridges.filter((c) => c.local_state === 'evicted').length

  return (
    <div className="mx-auto max-w-7xl px-6 py-5">
      <header className="flex flex-wrap items-center justify-between gap-3 border-b border-zinc-800 pb-4">
        <div className="flex items-center gap-3">
          <h1 className="flex items-center gap-3">
            <img src="/brand/icon.svg" alt="" className="h-16 w-auto" />
            <img src="/brand/wordmark.svg" alt="OpenVTL" className="h-8 w-auto" />
          </h1>
          <span className="text-sm text-zinc-500">
            {vtl.snap.libraries.length === 1
              ? `${vtl.snap.libraries[0].library.name} · ${modelLabel(vtl.snap.libraries[0].library.product)}`
              : `${vtl.snap.libraries.length} libraries · ${vtl.snap.pools.length} pool${vtl.snap.pools.length === 1 ? '' : 's'}`}
          </span>
        </div>
        <div className="flex items-center gap-4 text-xs">
          <FabricChip fabrics={vtl.snap.fabrics} onClick={() => goto('targets')} />
          <Badge ok={vtl.connected} label={vtl.connected ? 'live' : 'reconnecting…'} />
          <span className="flex items-center gap-2 border-l border-zinc-800 pl-4 text-zinc-400">
            <span title={user?.role === 'admin' ? 'admin' : 'read-only'}>
              {user?.username}
              <span className="ml-1.5 rounded bg-zinc-800 px-1.5 py-0.5 font-mono text-[10px] text-zinc-500">
                {user?.role === 'admin' ? 'admin' : 'ro'}
              </span>
            </span>
            <button onClick={logout}
              className="rounded px-1.5 py-0.5 text-zinc-500 transition-colors hover:bg-zinc-800 hover:text-zinc-300"
              title="sign out">
              Sign out
            </button>
          </span>
        </div>
      </header>

      <nav className="mt-4 flex gap-1 border-b border-zinc-800 pb-px text-sm">
        {VIEWS.map((v) => (
          <button key={v} onClick={() => goto(v)}
            className={`relative rounded-t px-3.5 py-2 font-medium transition-colors ${
              view === v
                ? 'bg-zinc-800/80 text-zinc-100'
                : 'text-zinc-500 hover:bg-zinc-900 hover:text-zinc-300'
            }`}>
            {LABELS[v]}
            {v === 'jobs' && activeJobs > 0 && (
              <span className="ml-1.5 rounded-full bg-sky-500/20 px-1.5 py-0.5 font-mono text-[10px] text-sky-300">
                {activeJobs}
              </span>
            )}
            {v === 'library' && evictedCount > 0 && (
              <span className="ml-1.5 rounded-full bg-red-500/20 px-1.5 py-0.5 font-mono text-[10px] text-red-300"
                title={`${evictedCount} evicted stub${evictedCount === 1 ? '' : 's'} — mounting one reads as blank`}>
                {evictedCount}
              </span>
            )}
          </button>
        ))}
      </nav>

      <main className="mt-6">
        {view === 'dashboard' && <Dashboard vtl={vtl} goto={goto} />}
        {view === 'library' && <Library vtl={vtl} goto={goto} />}
        {view === 'storage' && <Storage vtl={vtl} goto={goto} />}
        {view === 'jobs' && <Jobs vtl={vtl} />}
        {view === 's3' && <S3 goto={goto} />}
        {view === 'targets' && <Targets />}
        {view === 'settings' && <Settings />}
      </main>

      <MaintenanceOverlay vtl={vtl} />
      <Toasts />
    </div>
  )
}

// MaintenanceOverlay — a floating live checklist for the multi-minute
// maintenance operations (Activate, delete a serving library, data-
// plane restart, reboot). Driven entirely by SSE, so it shows progress
// even if the operator navigates away from the button they clicked.
//
// It never vanishes mid-maintenance: the window stays until the operator
// dismisses a completed/failed op, and while the appliance is (or is
// about to be) unreachable it holds a live "reconnecting" state rather
// than disappearing and leaving the operator on a dead page.
function MaintenanceOverlay({ vtl }: { vtl: ReturnType<typeof useVTL> }) {
  const m = vtl.maint
  if (!m) return null
  const connected = vtl.connected
  const failed = m.status === 'failed'
  const done = m.status === 'done'
  // Waiting = the appliance is, or is about to be, gone: an explicit
  // reboot, or a running op whose SSE stream has already dropped. The
  // connection watcher resolves it to `done` once the stream returns.
  const waiting = m.status === 'rebooting' || (m.status === 'running' && !connected)
  const running = m.status === 'running' && connected
  const spinning = running || waiting

  let subtitle = m.detail ?? ''
  if (waiting) subtitle = 'Waiting for the appliance to come back — reconnecting…'
  else if (running) subtitle = 'Host connections drop during this — it can take a couple of minutes.'

  return (
    <div className="fixed bottom-4 left-4 z-50 w-96 max-w-[calc(100vw-2rem)] rounded-lg border border-zinc-700 bg-zinc-900/95 p-4 shadow-xl">
      <div className="flex items-center justify-between">
        <span className="flex items-center gap-2 text-sm font-medium text-zinc-100">
          {spinning && <span className="h-3 w-3 animate-spin rounded-full border-2 border-zinc-600 border-t-sky-400" />}
          {done && <span className="text-emerald-400">✓</span>}
          {failed && <span className="text-red-400">✕</span>}
          {m.label}
        </span>
        {(done || failed) && (
          <button onClick={vtl.dismissMaint} className="text-zinc-500 hover:text-zinc-200">✕</button>
        )}
      </div>
      <p className="mt-1 text-[11px] text-zinc-500">{subtitle}</p>
      <div className="mt-2 max-h-48 space-y-0.5 overflow-y-auto border-t border-zinc-800 pt-2 font-mono text-[11px]">
        {m.steps.length === 0 && spinning && <div className="text-zinc-600">starting…</div>}
        {m.steps.map((s, i) => (
          <div key={i} className={failed && i === m.steps.length - 1 ? 'text-amber-300' : 'text-emerald-300/80'}>
            ✓ {s}
          </div>
        ))}
        {waiting && <div className="animate-pulse text-sky-300/80">· reconnecting…</div>}
        {failed && <div className="mt-1 text-red-300">{m.detail}</div>}
      </div>
    </div>
  )
}

// FabricChip — ONE aggregate FC health signal (v0.7; FC-only since
// 2026-08-24): green serving + session count, red
// fault, gray when no HBA is present. Clicking jumps to Access.
function FabricChip({ fabrics, onClick }: { fabrics?: Fabrics; onClick: () => void }) {
  const fc = fabrics?.fc
  const fcOn = !!fc?.present

  const summary = fcOn
    ? `FC ${fc!.verified ? '✓' : 'FAULT'} · ${fc!.sessions} session${fc!.sessions === 1 ? '' : 's'}`
    : 'FC: no HBA'
  // On a fault, guide the operator instead of dumping internals (the
  // raw detail stays in /api/status + the journal for diagnosis).
  const fault = fcOn && !fc!.verified
  const title = fault
    ? `${summary}\nIf the host lost its tape connection: try an IOP Reset/IPL, or Concurrent Maintenance (vary domain power off/on), then vary the tape devices back on.`
    : summary

  let cls = 'bg-zinc-800 text-zinc-400'
  let dot = 'bg-zinc-500'
  let label = 'no HBA'
  if (fcOn && !fc!.verified) {
    cls = 'bg-red-500/10 text-red-400'
    dot = 'bg-red-400 animate-pulse'
    label = 'target FAULT'
  } else if (fcOn) {
    const n = fc?.sessions ?? 0
    cls = 'bg-emerald-500/10 text-emerald-400'
    dot = 'bg-emerald-400'
    label = `serving · ${n} session${n === 1 ? '' : 's'}`
  }
  return (
    <button onClick={onClick} title={title}
      className={`inline-flex items-center gap-1.5 rounded-full px-2.5 py-1 font-medium transition-colors hover:brightness-125 ${cls}`}>
      <span className={`h-1.5 w-1.5 rounded-full ${dot}`} />
      {label}
    </button>
  )
}
