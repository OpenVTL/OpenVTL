import { useEffect, useState } from 'react'
import { api } from '../api'
import { Badge, Button, Field, Panel, inputCls, notify } from '../components/ui'
import { useAuthCtx } from '../useAuth'
import type { APIKey, AuditEntry, LicenseInfo, Remote, Role, Settings as SettingsMap, SystemInfo, UpdateStatus, User } from '../types'

const K_IE = 'export.ie_watcher'
const K_REMOTE = 'export.default_remote'
const K_EVICT = 'evict.threshold_pct'
const K_MINT = 'minting.enabled'
const K_APIKEYS = 'apikeys.enabled'
const K_SYSNAME = 'system.name'

export default function Settings() {
  const { admin } = useAuthCtx()
  const [s, setS] = useState<SettingsMap>({})
  const [remotes, setRemotes] = useState<Remote[]>([])
  const [audit, setAudit] = useState<AuditEntry[]>([])
  const [busy, setBusy] = useState(false)

  useEffect(() => {
    // apikeys.enabled is owned by the API-keys panel (immediate saves);
    // keeping it out of this map stops the policy Save from writing a
    // stale value back over a panel toggle.
    api.settings().then((m) => { const { [K_APIKEYS]: _drop, ...rest } = m; setS(rest) }).catch(() => {})
    api.remotes().then(setRemotes).catch(() => {})
    api.audit().then(setAudit).catch(() => {})
  }, [])

  const set = (k: string, v: string) => setS((prev) => ({ ...prev, [k]: v }))
  const save = () => {
    setBusy(true)
    api.saveSettings(s)
      .then((next) => { setS(next); notify('Settings saved'); api.audit().then(setAudit).catch(() => {}) })
      .catch((e: Error) => notify(e.message, true))
      .finally(() => setBusy(false))
  }

  return (
    <div className="space-y-6">
      <Panel title="Export & eviction policy" right={<Button kind="primary" needsAdmin disabled={busy} onClick={save}>Save</Button>}>
        <div className="grid grid-cols-1 gap-5 md:grid-cols-2">
          <div>
            <Field label="IE vault watcher">
              <select className={inputCls} value={s[K_IE] || 'off'} onChange={(e) => set(K_IE, e.target.value)}>
                <option value="off">off — manual “Export now” only</option>
                <option value="on">on — auto-export carts moved to I/E elements</option>
              </select>
            </Field>
            <p className="mt-1.5 text-xs text-zinc-600">
              When the host ejects a cartridge (a vault move), it is exported automatically
              and returned to its slot once the upload is verified.
            </p>
          </div>
          <div>
            <Field label="default S3 remote (for automatic exports)">
              <select className={inputCls} value={s[K_REMOTE] || ''} onChange={(e) => set(K_REMOTE, e.target.value)}>
                <option value="">— none —</option>
                {remotes.map((r) => <option key={r.id} value={String(r.id)}>{r.name}</option>)}
              </select>
            </Field>
          </div>
          <div>
            <Field label="eviction pressure threshold (% of physical storage used)">
              <input className={inputCls} inputMode="numeric" placeholder="empty = never evict automatically"
                value={s[K_EVICT] || ''} onChange={(e) => set(K_EVICT, e.target.value.replace(/[^0-9.]/g, ''))} />
            </Field>
            <p className="mt-1.5 text-xs text-zinc-600">
              Above this, the oldest exported cartridge is evicted automatically to free
              space. Manual eviction is always available per cartridge.
            </p>
          </div>
          <div>
            <Field label="automatic spare cartridges">
              <select className={inputCls} value={s[K_MINT] || 'off'} onChange={(e) => set(K_MINT, e.target.value)} disabled>
                <option value="off">off — pre-provision scratch instead</option>
              </select>
            </Field>
            <p className="mt-1.5 text-xs text-zinc-600">
              Reserved for a future release — pre-provision scratch cartridges instead.
            </p>
          </div>
        </div>
      </Panel>

      <SystemPanel />
      <SupportPanel />
      <UpdatesPanel />
      {admin && <APIKeysPanel />}
      {admin && <UsersPanel />}
      <PasswordPanel />

      <Panel title="Audit log" right={<span className="text-xs text-zinc-600">who did what, when</span>}>
        <div className="max-h-[420px] overflow-y-auto">
          <table className="w-full text-left text-xs">
            <thead>
              <tr className="border-b border-zinc-800 text-[10px] uppercase tracking-wider text-zinc-500">
                <th className="py-1.5 pr-3 font-medium">time</th>
                <th className="py-1.5 pr-3 font-medium">actor</th>
                <th className="py-1.5 pr-3 font-medium">from</th>
                <th className="py-1.5 pr-3 font-medium">action</th>
                <th className="py-1.5 pr-3 font-medium">subject</th>
                <th className="py-1.5 font-medium">params</th>
              </tr>
            </thead>
            <tbody>
              {audit.length === 0 && (
                <tr><td colSpan={6} className="py-3 text-zinc-600">no mutations recorded yet</td></tr>
              )}
              {audit.map((a) => (
                <tr key={a.id} className="border-b border-zinc-800/60">
                  <td className="py-1.5 pr-3 font-mono text-zinc-500">{new Date(a.ts).toLocaleString()}</td>
                  <td className="py-1.5 pr-3 font-mono text-zinc-400">{a.actor}</td>
                  <td className="py-1.5 pr-3 font-mono text-zinc-600">{a.remote_addr || '—'}</td>
                  <td className="py-1.5 pr-3 font-mono text-sky-300">{a.action}</td>
                  <td className="py-1.5 pr-3 font-mono text-amber-300">{a.subject || '—'}</td>
                  <td className="max-w-[260px] truncate py-1.5 font-mono text-zinc-500" title={a.params}>{a.params || '—'}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </Panel>
    </div>
  )
}

// SystemPanel — daemon status + restart. Restart touches the
// control plane only: mhVTL, LIO, and host I/O never notice. Graceful
// drains jobs first (new jobs blocked meanwhile); immediate restarts
// now and interrupted jobs become retryable.
function SystemPanel() {
  const [info, setInfo] = useState<SystemInfo | null>(null)
  const [busy, setBusy] = useState(false)
  const [name, setName] = useState('')
  const [nameDirty, setNameDirty] = useState(false)

  const load = () => api.system().then(setInfo).catch(() => {})
  useEffect(() => {
    load()
    const t = window.setInterval(load, 10_000)
    return () => window.clearInterval(t)
  }, [])
  // Seed the editable name from the server until the operator edits it.
  useEffect(() => { if (info && !nameDirty) setName(info.system_name) }, [info, nameDirty])

  const saveName = () => {
    api.saveSettings({ [K_SYSNAME]: name.trim() })
      .then(() => { notify('system name saved'); setNameDirty(false); load() })
      .catch((e: Error) => notify(e.message, true))
  }

  const restart = (mode: 'graceful' | 'immediate') => {
    const n = info?.active_jobs ?? 0
    const msg = mode === 'graceful'
      ? n > 0
        ? `Restart openvtld after the ${n} active job(s) finish? Tape serving is not affected.`
        : 'Restart openvtld now? Tape serving is not affected.'
      : `Restart openvtld immediately?${n > 0 ? ` ${n} active job(s) will be interrupted (they can be retried).` : ''} Tape serving is not affected.`
    if (!window.confirm(msg)) return
    setBusy(true)
    api.restart(mode)
      .then((r) => notify(r.detail ?? (r.draining ? `draining ${r.active_jobs} job(s), then restarting` : 'restarting')))
      .catch((e: Error) => notify(e.message, true))
      .finally(() => { setBusy(false); window.setTimeout(load, 1500) })
  }

  const uptime = (sec: number) => {
    const d = Math.floor(sec / 86400), h = Math.floor((sec % 86400) / 3600), m = Math.floor((sec % 3600) / 60)
    return d > 0 ? `${d}d ${h}h ${m}m` : h > 0 ? `${h}h ${m}m` : `${m}m`
  }

  // Tier 2: mhVTL restart + fabric rebuild — the Activate sequence with
  // no new libraries. Every host session drops; typed confirm.
  const [steps, setSteps] = useState<string[]>([])
  const dataplane = () => {
    const n = info?.active_jobs ?? 0
    if (n > 0) { notify(`${n} active job(s) — wait or cancel first`, true); return }
    const typed = window.prompt(
      'Restart the data plane (tape daemons + target rebuild)?\n\n' +
      'Host connections drop — run during a maintenance window, with drives empty ' +
      'and idle. Afterwards vary the host tape devices off/on.\n\nType "restart" to confirm:')
    if (typed === null) return
    if (typed.trim() !== 'restart') { notify('confirmation mismatch — cancelled', true); return }
    setBusy(true)
    setSteps([])
    api.dataplaneRestart()
      .then((r) => { setSteps(r.steps); notify('data plane restarted — operator steps remain (vary off/on)') })
      .catch((e: Error) => notify(e.message, true))
      .finally(() => { setBusy(false); load() })
  }

  // Tier 3: the recovery hammer. Boot orchestration restores targets.
  const reboot = () => {
    const n = info?.active_jobs ?? 0
    const typed = window.prompt(
      'Reboot the appliance?\n\n' +
      'Host connections drop; everything comes back automatically after boot.' +
      `${n > 0 ? ` ${n} active job(s) will be interrupted (they can be retried).` : ''}` +
      '\n\nType "reboot" to confirm:')
    if (typed === null) return
    if (typed.trim() !== 'reboot') { notify('confirmation mismatch — cancelled', true); return }
    setBusy(true)
    api.rebootHost()
      .then((r) => notify(r.detail))
      .catch((e: Error) => notify(e.message, true))
      .finally(() => setBusy(false))
  }

  return (
    <Panel title="System" right={
      <div className="flex gap-2">
        <Button needsAdmin disabled={busy || info?.draining} onClick={() => restart('graceful')}>
          Restart (graceful)
        </Button>
        <Button kind="danger" needsAdmin disabled={busy} onClick={() => restart('immediate')}>
          Restart (immediate)
        </Button>
      </div>
    }>
      {!info ? <p className="text-xs text-zinc-600">loading…</p> : (
        <div className="flex flex-wrap gap-x-8 gap-y-2 text-xs">
          <span className="text-zinc-500">version <span className="ml-1 font-mono text-zinc-300">{info.version}</span></span>
          <span className="text-zinc-500">uptime <span className="ml-1 font-mono text-zinc-300">{uptime(info.uptime_sec)}</span></span>
          <span className="text-zinc-500">active jobs <span className="ml-1 font-mono text-zinc-300">{info.active_jobs}</span></span>
          <span className="text-zinc-500">plaintext listener{' '}
            <span className="ml-1 font-mono text-zinc-300">{info.plain_listener || 'disabled'}</span>
            <span className="ml-1 text-zinc-600">(metrics/health only; flag-controlled via site.conf)</span>
          </span>
          {info.draining && (
            <span className="rounded bg-amber-500/10 px-1.5 py-0.5 text-amber-300">
              draining jobs — restart pending
            </span>
          )}
        </div>
      )}
      {info && (
        <div className="mt-3 flex flex-wrap items-end gap-2 border-t border-zinc-800 pt-3">
          <Field label="system name — this instance's folder in S3 (System › Library › Tape)">
            <input className={inputCls} value={name} placeholder="e.g. hq-prod"
              onChange={(e) => { setName(e.target.value.toLowerCase()); setNameDirty(true) }} />
          </Field>
          <Button needsAdmin disabled={!nameDirty || name.trim() === ''} onClick={saveName}>Save name</Button>
          <span className="pb-1.5 font-mono text-[11px] text-zinc-600">id {info.system_uuid.slice(0, 8)}</span>
        </div>
      )}
      <p className="mt-2 text-xs text-zinc-600">
        Safe during operation — tape serving is not affected. The header badge briefly
        shows reconnecting…
      </p>

      <div className="mt-4 border-t border-zinc-800 pt-3">
        <div className="mb-2 text-xs font-medium text-amber-300/80">
          Maintenance window — these DROP every host session
        </div>
        <div className="flex flex-wrap gap-2">
          <Button kind="danger" needsAdmin disabled={busy || info?.draining} onClick={dataplane}
            title="mhVTL restart + target rebuild (the Activate sequence) — drives must be empty and idle">
            Restart data plane…
          </Button>
          <Button kind="danger" needsAdmin disabled={busy} onClick={reboot}
            title="full appliance reboot — the recovery hammer for wedged daemons and D-state probes">
            Reboot appliance…
          </Button>
        </div>
        <p className="mt-2 text-xs text-zinc-600">
          Data plane = tape daemons + target rebuild. Reboot = the whole appliance.
          Both recover automatically; afterwards vary the host tape devices off/on.
        </p>
        {steps.length > 0 && (
          <div className="mt-3 space-y-0.5 border-t border-zinc-800 pt-2 font-mono text-[11px] text-emerald-300/80">
            {steps.map((s, i) => <div key={i}>✓ {s}</div>)}
          </div>
        )}
      </div>
    </Panel>
  )
}

// SupportPanel — the appliance's support key: a derived fingerprint of this
// machine that the customer gives support to link the instance to their
// account. It re-keys on an OS reinstall or hardware transfer; when that
// happens the notice prompts the user to update it on their support profile.
function SupportPanel() {
  const [info, setInfo] = useState<LicenseInfo | null>(null)
  const [bundling, setBundling] = useState(false)
  const load = () => api.license().then(setInfo).catch(() => {})
  useEffect(() => { load() }, [])

  const bundle = () => {
    setBundling(true)
    api.downloadSupportBundle()
      .then(() => notify('support bundle downloaded'))
      .catch((e: Error) => notify(e.message, true))
      .finally(() => setBundling(false))
  }

  const copy = () => {
    if (!info) return
    navigator.clipboard?.writeText(info.fingerprint)
      .then(() => notify('support key copied'))
      .catch(() => notify('copy failed — select and copy manually', true))
  }
  const ack = () => {
    api.ackLicense().then(() => { notify('key change acknowledged'); load() })
      .catch((e: Error) => notify(e.message, true))
  }

  return (
    <Panel title="Support">
      <p className="mb-3 text-xs text-zinc-500">
        Your support key identifies this appliance to support. Give it when you sign up,
        and update it on your support profile if it ever changes. It is never uploaded from here.
      </p>
      {!info ? <p className="text-xs text-zinc-600">loading…</p> : (
        <>
          <div className="flex flex-wrap items-center gap-3">
            <code className="select-all rounded bg-zinc-900 px-3 py-1.5 font-mono text-sm tracking-wider text-emerald-300">
              {info.fingerprint}
            </code>
            <Button onClick={copy}>Copy</Button>
          </div>
          {info.changed && (
            <div className="mt-3 rounded border border-amber-700/50 bg-amber-950/30 px-3 py-2 text-xs text-amber-200">
              <div className="font-semibold text-amber-300">This appliance's support key changed.</div>
              <div className="mt-0.5">
                A hardware or OS change re-keyed it{info.previous ? <> (was <span className="font-mono">{info.previous}</span>)</> : null}.
                Update it on your support profile, then acknowledge.
              </div>
              <div className="mt-2">
                <Button needsAdmin onClick={ack}>I've updated my support profile</Button>
              </div>
            </div>
          )}
          <div className="mt-4 border-t border-zinc-800 pt-3">
            <div className="flex flex-wrap items-center gap-3">
              <Button needsAdmin disabled={bundling} onClick={bundle}>
                {bundling ? 'preparing…' : 'Generate support bundle'}
              </Button>
              <span className="text-xs text-zinc-500">
                A redacted .tar.gz of logs and status for support — no secrets or passwords are included.
              </span>
            </div>
          </div>
        </>
      )}
    </Panel>
  )
}

// UpdatesPanel — upload a signed release bundle and apply it
// The apply runs DETACHED on the box (it restarts
// the control plane), so after a 202 this panel polls /healthz until
// the target version answers — or the old one does, meaning the
// updater's health probe failed and it rolled itself back.
function UpdatesPanel() {
  const [st, setSt] = useState<UpdateStatus | null>(null)
  const [file, setFile] = useState<File | null>(null)
  const [phase, setPhase] = useState<'idle' | 'uploading' | 'applying' | 'rolling-back'>('idle')
  const [outcome, setOutcome] = useState('')

  const refresh = () => api.updateStatus().then(setSt).catch(() => {})
  useEffect(() => { refresh() }, [])

  // Ride out the control-plane restart: poll the public /healthz until
  // some version answers steadily, then judge success by which one.
  const watch = (target: string, verb: string) => {
    const t0 = Date.now()
    const tick = () => {
      fetch('/healthz')
        .then((r) => (r.ok ? r.json() : Promise.reject(new Error(String(r.status)))))
        .then((h: { version?: string }) => {
          if (h.version === target) {
            setPhase('idle'); setFile(null)
            setOutcome(`${verb} succeeded — now running ${target}`)
            notify(`${verb} complete: ${target}`)
            refresh()
          } else if (Date.now() - t0 > 30_000 && h.version && h.version !== target) {
            // The box settled on a different version well after the
            // restart window: the updater rolled back.
            setPhase('idle')
            setOutcome(`${verb} did not take — the box is running ${h.version} (auto-rolled-back; see journalctl on the appliance)`)
            refresh()
          } else if (Date.now() - t0 < 300_000) {
            window.setTimeout(tick, 2500)
          } else {
            setPhase('idle')
            setOutcome(`${verb} status unknown after 5 min — check Settings again or journalctl on the appliance`)
            refresh()
          }
        })
        .catch(() => {
          if (Date.now() - t0 < 300_000) window.setTimeout(tick, 2500)
          else { setPhase('idle'); setOutcome(`${verb}: appliance did not come back within 5 min — investigate before retrying`) }
        })
    }
    window.setTimeout(tick, 4000)
  }

  const upload = () => {
    if (!file) return
    setOutcome(''); setPhase('uploading')
    api.uploadUpdate(file)
      .then((r) => {
        setPhase('applying')
        notify(`bundle verified — applying ${r.from} → ${r.to}`)
        watch(r.to, 'Update')
      })
      .catch((e: Error) => { setPhase('idle'); setOutcome(''); notify(e.message, true) })
  }

  const rollback = () => {
    const target = st?.pending?.from_version ?? st?.last_good?.from_version
    if (!target) return
    if (!window.confirm(`Roll back to ${target}? The control plane restarts and the database snapshot taken at that update is restored.`)) return
    setOutcome(''); setPhase('rolling-back')
    api.rollbackUpdate()
      .then((r) => { notify(`rolling back to ${r.to}`); watch(r.to, 'Rollback') })
      .catch((e: Error) => { setPhase('idle'); notify(e.message, true) })
  }

  const busy = phase !== 'idle'
  const rollbackTarget = st?.pending?.from_version ?? st?.last_good?.from_version

  return (
    <Panel title="Updates" right={
      st ? <span className="font-mono text-xs text-zinc-500">running {st.version}</span> : undefined
    }>
      <p className="text-xs text-zinc-500">
        Upload a signed OpenVTL release bundle (openvtl-&lt;version&gt;.tar.gz from your support portal).
        The signature is verified before anything is touched; applying restarts the control plane only —
        tape I/O and host sessions are unaffected. Data-plane (mhVTL/kernel) bundles are refused here and
        need install.sh in a maintenance window.
      </p>

      {st?.pending && (
        <div className="mt-3 rounded border border-amber-500/40 bg-amber-500/10 px-3 py-2 text-xs text-amber-300">
          An update {st.pending.from_version} → {st.pending.to_version} is staged but not yet confirmed.
          If the box is healthy this clears itself; otherwise use Roll back.
        </div>
      )}
      {outcome && (
        <div className="mt-3 rounded border border-zinc-700 bg-zinc-900/60 px-3 py-2 text-xs text-zinc-300">{outcome}</div>
      )}

      <div className="mt-3 flex flex-wrap items-center gap-3">
        <input type="file" accept=".gz,.tgz,application/gzip"
          className="text-xs text-zinc-400 file:mr-3 file:rounded file:border file:border-zinc-700 file:bg-zinc-800 file:px-2 file:py-1 file:text-xs file:text-zinc-200"
          disabled={busy}
          onChange={(e) => setFile(e.target.files?.[0] ?? null)} />
        <Button kind="primary" needsAdmin disabled={busy || !file} onClick={upload}>
          {phase === 'uploading' ? 'uploading…' : phase === 'applying' ? 'applying — control plane restarting…' : 'Verify & apply'}
        </Button>
      </div>

      <div className="mt-4 border-t border-zinc-800 pt-3">
        <div className="flex flex-wrap items-center gap-3">
          <Button kind="danger" needsAdmin disabled={busy || !rollbackTarget} onClick={rollback}
            title={rollbackTarget ? `revert to ${rollbackTarget} + its DB snapshot` : 'no update has been applied via the updater yet'}>
            {phase === 'rolling-back' ? 'rolling back…' : 'Roll back'}
          </Button>
          <span className="text-xs text-zinc-500">
            {rollbackTarget
              ? <>reverts to <span className="font-mono text-zinc-400">{rollbackTarget}</span> and restores the database snapshot taken when it was updated away from</>
              : 'available after the first update applied through the updater'}
          </span>
        </div>
      </div>
    </Panel>
  )
}

// APIKeysPanel — bearer-token identities for scripts. The
// master toggle is the external-API switch: off = keys rejected
// wholesale. Tokens are shown exactly once at creation.
function APIKeysPanel() {
  const [enabled, setEnabled] = useState(false)
  const [keys, setKeys] = useState<APIKey[]>([])
  const [name, setName] = useState('')
  const [role, setRole] = useState<Role>('readonly')
  const [fresh, setFresh] = useState<{ name: string; token: string } | null>(null)
  const [busy, setBusy] = useState(false)

  const load = () => {
    api.settings().then((m) => setEnabled(m[K_APIKEYS] === '1')).catch(() => {})
    api.apiKeys().then(setKeys).catch(() => {})
  }
  useEffect(load, [])

  const toggle = () => {
    const next = enabled ? '' : '1'
    api.saveSettings({ [K_APIKEYS]: next })
      .then(() => { setEnabled(!enabled); notify(`API key authentication ${next ? 'enabled' : 'disabled'}`) })
      .catch((e: Error) => notify(e.message, true))
  }

  const create = () => {
    setBusy(true)
    api.createAPIKey(name.trim(), role)
      .then((r) => { setFresh({ name: r.name, token: r.token }); setName(''); load() })
      .catch((e: Error) => notify(e.message, true))
      .finally(() => setBusy(false))
  }

  const revoke = (k: APIKey) => {
    if (!window.confirm(`Revoke key "${k.name}"? Callers using it are denied immediately.`)) return
    api.deleteAPIKey(k.id)
      .then(() => { notify(`key ${k.name} revoked`); load() })
      .catch((e: Error) => notify(e.message, true))
  }

  return (
    <Panel title="API access keys" right={
      <div className="flex items-center gap-3">
        <Badge ok={enabled} label={enabled ? 'key auth enabled' : 'key auth disabled'} />
        <Button needsAdmin kind={enabled ? 'danger' : 'primary'} onClick={toggle}>
          {enabled ? 'Disable' : 'Enable'}
        </Button>
      </div>
    }>
      <p className="mb-3 text-xs text-zinc-500">
        Tokens for scripts and integrations (<span className="font-mono">Authorization: Bearer ovtl_…</span>).
        Each key has a role and shows in the audit log. While disabled, all keys are rejected.
      </p>

      {fresh && (
        <div className="mb-3 rounded border border-amber-700/50 bg-amber-950/30 px-3 py-2 text-xs">
          <div className="font-semibold text-amber-300">
            Key “{fresh.name}” created — copy the token now, it is never shown again.
          </div>
          <div className="mt-1 flex items-center gap-2">
            <code className="select-all break-all font-mono text-amber-200">{fresh.token}</code>
            <Button onClick={() => setFresh(null)}>Done</Button>
          </div>
        </div>
      )}

      <table className="w-full text-left text-xs">
        <thead>
          <tr className="border-b border-zinc-800 text-[10px] uppercase tracking-wider text-zinc-500">
            <th className="py-1.5 pr-3 font-medium">name</th>
            <th className="py-1.5 pr-3 font-medium">role</th>
            <th className="py-1.5 pr-3 font-medium">created</th>
            <th className="py-1.5 pr-3 font-medium">last used</th>
            <th className="py-1.5 font-medium"></th>
          </tr>
        </thead>
        <tbody>
          {keys.length === 0 && (
            <tr><td colSpan={5} className="py-2 text-zinc-600">no keys</td></tr>
          )}
          {keys.map((k) => (
            <tr key={k.id} className="border-b border-zinc-800/60">
              <td className="py-1.5 pr-3 font-mono text-zinc-300">{k.name}</td>
              <td className="py-1.5 pr-3 text-zinc-400">{k.role === 'admin' ? 'admin' : 'read-only'}</td>
              <td className="py-1.5 pr-3 font-mono text-zinc-500">{new Date(k.created_at).toLocaleDateString()}</td>
              <td className="py-1.5 pr-3 font-mono text-zinc-500">
                {k.last_used_at ? new Date(k.last_used_at).toLocaleString() : 'never'}
              </td>
              <td className="py-1.5 text-right">
                <Button kind="danger" needsAdmin onClick={() => revoke(k)}>Revoke</Button>
              </td>
            </tr>
          ))}
        </tbody>
      </table>

      <div className="mt-4 grid grid-cols-1 items-end gap-3 border-t border-zinc-800 pt-4 md:grid-cols-[1fr_160px_auto]">
        <Field label="key name (e.g. grafana, brms-scripts)">
          <input className={inputCls} value={name} onChange={(e) => setName(e.target.value)} />
        </Field>
        <Field label="role">
          <select className={inputCls} value={role} onChange={(e) => setRole(e.target.value as Role)}>
            <option value="readonly">read-only</option>
            <option value="admin">admin</option>
          </select>
        </Field>
        <Button kind="primary" needsAdmin disabled={busy || name.trim().length < 2} onClick={create}>
          Create key
        </Button>
      </div>
    </Panel>
  )
}

// UsersPanel — admin-only account management.
function UsersPanel() {
  const { user: me } = useAuthCtx()
  const [users, setUsers] = useState<User[]>([])
  const [name, setName] = useState('')
  const [pw, setPw] = useState('')
  const [role, setRole] = useState<Role>('readonly')
  const [busy, setBusy] = useState(false)

  const reload = () => api.users().then(setUsers).catch((e: Error) => notify(e.message, true))
  useEffect(() => { reload() }, [])

  const create = () => {
    setBusy(true)
    api.createUser(name, pw, role)
      .then(() => { notify(`user ${name} created`); setName(''); setPw(''); reload() })
      .catch((e: Error) => notify(e.message, true))
      .finally(() => setBusy(false))
  }
  const patch = (u: User, p: { role?: Role; disabled?: boolean }) =>
    api.updateUser(u.id, p).then(() => reload()).catch((e: Error) => notify(e.message, true))
  const del = (u: User) => {
    if (!window.confirm(`Delete user ${u.username}? Their sessions end immediately.`)) return
    api.deleteUser(u.id).then(() => { notify(`user ${u.username} deleted`); reload() })
      .catch((e: Error) => notify(e.message, true))
  }
  const resetPw = (u: User) => {
    const pw = window.prompt(`New password for ${u.username} (min 8 chars):`)
    if (!pw) return
    api.setPassword(u.id, pw).then(() => notify(`password set for ${u.username}`))
      .catch((e: Error) => notify(e.message, true))
  }

  return (
    <Panel title="Users" right={<span className="text-xs text-zinc-600">admin = full control · read-only = observe</span>}>
      <table className="w-full text-left text-xs">
        <thead>
          <tr className="border-b border-zinc-800 text-[10px] uppercase tracking-wider text-zinc-500">
            <th className="py-1.5 pr-3 font-medium">user</th>
            <th className="py-1.5 pr-3 font-medium">role</th>
            <th className="py-1.5 pr-3 font-medium">created</th>
            <th className="py-1.5 font-medium">actions</th>
          </tr>
        </thead>
        <tbody>
          {users.map((u) => (
            <tr key={u.id} className={`border-b border-zinc-800/60 ${u.disabled ? 'opacity-50' : ''}`}>
              <td className="py-2 pr-3 font-mono text-zinc-300">
                {u.username}{u.id === me?.id && <span className="ml-1.5 text-zinc-600">(you)</span>}
                {u.disabled && <span className="ml-1.5 rounded bg-red-500/10 px-1.5 text-[10px] text-red-400">disabled</span>}
              </td>
              <td className="py-2 pr-3">
                <select className="rounded border border-zinc-700 bg-zinc-900 px-1.5 py-0.5 text-xs text-zinc-300"
                  value={u.role} disabled={u.id === me?.id}
                  onChange={(e) => patch(u, { role: e.target.value as Role })}>
                  <option value="admin">admin</option>
                  <option value="readonly">read-only</option>
                </select>
              </td>
              <td className="py-2 pr-3 font-mono text-zinc-500">{new Date(u.created_at).toLocaleDateString()}</td>
              <td className="space-x-1.5 py-2">
                <Button onClick={() => resetPw(u)}>Set password</Button>
                {u.id !== me?.id && (
                  <>
                    <Button onClick={() => patch(u, { disabled: !u.disabled })}>
                      {u.disabled ? 'Enable' : 'Disable'}
                    </Button>
                    <Button kind="danger" onClick={() => del(u)}>Delete</Button>
                  </>
                )}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
      <div className="mt-4 grid grid-cols-1 items-end gap-3 border-t border-zinc-800 pt-4 md:grid-cols-4">
        <Field label="new username">
          <input className={inputCls} value={name} onChange={(e) => setName(e.target.value)} />
        </Field>
        <Field label="password (min 8)">
          <input className={inputCls} type="password" autoComplete="new-password"
            value={pw} onChange={(e) => setPw(e.target.value)} />
        </Field>
        <Field label="role">
          <select className={inputCls} value={role} onChange={(e) => setRole(e.target.value as Role)}>
            <option value="readonly">read-only</option>
            <option value="admin">admin</option>
          </select>
        </Field>
        <Button kind="primary" disabled={busy || name.length < 2 || pw.length < 8} onClick={create}>
          Add user
        </Button>
      </div>
    </Panel>
  )
}

// PasswordPanel — every signed-in user may rotate their own password.
function PasswordPanel() {
  const { user: me } = useAuthCtx()
  const [pw, setPw] = useState('')
  const [confirm, setConfirm] = useState('')
  const [busy, setBusy] = useState(false)
  if (!me) return null

  const save = () => {
    if (pw !== confirm) { notify('passwords do not match', true); return }
    setBusy(true)
    api.setPassword(me.id, pw)
      .then(() => { notify('password changed'); setPw(''); setConfirm('') })
      .catch((e: Error) => notify(e.message, true))
      .finally(() => setBusy(false))
  }

  return (
    <Panel title="My password">
      <div className="grid grid-cols-1 items-end gap-3 md:grid-cols-3">
        <Field label="new password (min 8)">
          <input className={inputCls} type="password" autoComplete="new-password"
            value={pw} onChange={(e) => setPw(e.target.value)} />
        </Field>
        <Field label="confirm">
          <input className={inputCls} type="password" autoComplete="new-password"
            value={confirm} onChange={(e) => setConfirm(e.target.value)} />
        </Field>
        <Button kind="primary" disabled={busy || pw.length < 8} onClick={save}>Change password</Button>
      </div>
    </Panel>
  )
}
