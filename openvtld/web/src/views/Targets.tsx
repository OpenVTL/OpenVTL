import { useEffect, useState } from 'react'
import { api } from '../api'
import { Badge, Button, Field, Panel, inputCls, notify } from '../components/ui'
import type { InitiatorView, TargetsView } from '../types'

// Library display names for scope editor + LUN map (id stays visible
// in tooltips — serial/id are the host-facing identities).
function useLibNames(): Map<number, string> {
  const [names, setNames] = useState<Map<number, string>>(new Map())
  useEffect(() => {
    api.libraryRows()
      .then((rows) => setNames(new Map(rows.map((r) => [r.id, r.name]))))
      .catch(() => {})
  }, [])
  return names
}

// Targets & access (v0.7; FC-only since 2026-08-24):
//  · every target-capable FC port serves by default (per-port toggle);
//  · access is an initiator REGISTRY: WWPN + alias + port scope +
//    library scope, defaulting to all/all; unregistered = denied.
export default function Targets() {
  const [view, setView] = useState<TargetsView | null>(null)
  const libNames = useLibNames()

  const apply = (v: TargetsView) => setView(v)
  const reload = () => api.targets().then(apply).catch((e: Error) => notify(e.message, true))
  useEffect(() => {
    reload()
    const t = window.setInterval(reload, 10_000) // session status is live-ish
    return () => window.clearInterval(t)
  }, []) // eslint-disable-line react-hooks/exhaustive-deps

  if (!view) return null
  return (
    <div className="space-y-6">
      {view.error && (
        <div className="rounded border border-red-800/50 bg-red-950/40 px-4 py-3 text-sm text-red-300">
          {view.error}
        </div>
      )}
      <PortsPanel view={view} onChange={apply} />
      <InitiatorsPanel view={view} onChange={apply} libNames={libNames} />
      <LUNPanel view={view} libNames={libNames} />
      <p className="text-xs text-zinc-600">
        Changes apply live — the host may need a vary off/on of its tape devices to notice.
      </p>
    </div>
  )
}

// ---------------------------------------------------------- FC ports -------

function PortsPanel({ view, onChange }: { view: TargetsView; onChange: (v: TargetsView) => void }) {
  const toggle = (wwpn: string, serving: boolean, built: boolean) => {
    if (!serving && built && !window.confirm(
      `Stop serving on ${wwpn}? Any sessions on this port are dropped.`)) return
    api.setPortServing(wwpn, serving)
      .then((v) => { notify(`port ${wwpn} ${serving ? 'serving' : 'disabled'}`); onChange(v) })
      .catch((e: Error) => notify(e.message, true))
  }
  return (
    <Panel title="FC target ports" right={
      <span className="text-xs text-zinc-600">every port serves unless disabled</span>
    }>
      {view.fc.no_hba && (
        <p className="text-xs text-zinc-500">
          No FC HBA detected. Attach one to the VM (see the reference VM spec) — it starts
          serving on boot.
        </p>
      )}
      <div className="space-y-1.5">
        {view.fc.ports.map((p) => (
          <div key={p.wwpn} className="flex flex-wrap items-center justify-between gap-2 rounded bg-zinc-900/70 px-3 py-2 text-xs">
            <span className="font-mono text-zinc-300">{p.host} · {p.wwpn}</span>
            <span className="flex items-center gap-3">
              <span className={`font-mono ${p.state === 'Online' ? 'text-emerald-400' : 'text-zinc-500'}`}>
                {p.state}{p.speed && p.speed !== 'unknown' ? ` · ${p.speed}` : ''}
              </span>
              {p.serving
                ? <Badge ok={p.built} label={p.built ? 'serving' : 'serving (target pending)'}
                    title={p.built ? 'target built on this port' : 'will be built at the next ensure/apply'} />
                : <span className="inline-flex items-center gap-1.5 rounded-full bg-zinc-800 px-2.5 py-1 font-medium text-zinc-400">
                    <span className="h-1.5 w-1.5 rounded-full bg-zinc-500" />disabled</span>}
              <Button needsAdmin kind={p.serving ? 'danger' : 'default'}
                onClick={() => toggle(p.wwpn, !p.serving, p.built)}>
                {p.serving ? 'Disable' : 'Serve'}
              </Button>
            </span>
          </div>
        ))}
      </div>
    </Panel>
  )
}

// --------------------------------------------------------- initiators ------

const splitCSV = (s: string) => s.split(',').map((x) => x.trim()).filter(Boolean)

// '' = all, '-' = explicitly none (see ScopeEditor).
function scopeSummary(i: InitiatorView, libNames: Map<number, string>): string {
  const ports = i.ports === '-' ? 'NO ports (access off)' : i.ports ? `${splitCSV(i.ports).length} port(s)` : 'all ports'
  const libs = i.libraries === '-'
    ? 'NO libraries'
    : i.libraries
      ? splitCSV(i.libraries).map((id) => libNames.get(Number(id)) ?? `lib ${id}`).join(', ')
      : 'all libraries'
  return `${ports} · ${libs}`
}

function InitiatorsPanel({ view, onChange, libNames }: {
  view: TargetsView; onChange: (v: TargetsView) => void; libNames: Map<number, string>
}) {
  const [wwpn, setWwpn] = useState('')
  const [alias, setAlias] = useState('')
  const [busy, setBusy] = useState(false)
  const [editing, setEditing] = useState<string | null>(null)

  const add = () => {
    setBusy(true)
    api.addACL({ wwpn: wwpn.trim(), alias: alias.trim() })
      .then((v) => { notify(`initiator ${wwpn.trim()} registered (all ports, all libraries)`); setWwpn(''); setAlias(''); onChange(v) })
      .catch((e: Error) => notify(e.message, true))
      .finally(() => setBusy(false))
  }
  const remove = (i: InitiatorView) => {
    if (!window.confirm(i.logged_in
      ? `Initiator ${i.wwpn} HAS A LIVE SESSION — removing it drops the host mid-flight. Remove anyway?`
      : `Remove initiator ${i.wwpn}? It will be denied on the next login attempt.`)) return
    api.removeACL(i.wwpn, i.logged_in)
      .then((v) => { notify(`initiator ${i.wwpn} removed`); onChange(v) })
      .catch((e: Error) => notify(e.message, true))
  }

  return (
    <Panel title="Initiators & access" right={
      <span className="text-xs text-zinc-600">unregistered initiators are denied</span>
    }>
      <div className="space-y-2">
        {view.initiators.length === 0 && (
          <p className="text-xs text-zinc-600">no initiators registered</p>
        )}
        {view.initiators.map((i) => (
          <div key={i.wwpn} className="rounded border border-zinc-800 bg-zinc-900/60 px-3 py-2.5">
            <div className="flex flex-wrap items-center justify-between gap-2">
              <span className="flex items-center gap-2">
                <span className="font-mono text-xs text-zinc-200">{i.wwpn}</span>
              </span>
              <span className="flex items-center gap-2">
                <Badge ok={i.logged_in}
                  label={i.logged_in ? `logged in${i.port_state ? ` (${i.port_state})` : ''}` : 'not logged in'}
                  title={i.logged_in ? 'live session on a target' : 'no live session'} />
                <Button onClick={() => setEditing(editing === i.wwpn ? null : i.wwpn)}>Edit access</Button>
                <Button kind="danger" needsAdmin onClick={() => remove(i)}>Remove</Button>
              </span>
            </div>
            <div className="mt-1.5 flex flex-wrap items-center gap-2 text-[11px] text-zinc-500">
              {i.alias && <span className="text-zinc-400">“{i.alias}”</span>}
              <span>{scopeSummary(i, libNames)}</span>
              {!i.applied && (
                <span className="rounded bg-amber-500/10 px-1.5 py-0.5 text-amber-300"
                  title="registered but not (fully) present on the live targets — ensured at boot/apply">
                  pending apply
                </span>
              )}
            </div>
            {editing === i.wwpn && <ScopeEditor view={view} i={i} libNames={libNames} onDone={(v) => { setEditing(null); if (v) onChange(v) }} />}
          </div>
        ))}
      </div>

      <div className="mt-4 grid grid-cols-1 items-end gap-3 border-t border-zinc-800 pt-4 md:grid-cols-[1fr_1fr_auto]">
        <Field label="initiator WWPN (naa. / colon-hex / bare)">
          <input className={inputCls}
            placeholder="10:00:00:90:fa:xx:xx:xx"
            value={wwpn} onChange={(e) => setWwpn(e.target.value)} />
        </Field>
        <Field label="alias (optional)">
          <input className={inputCls} placeholder="IBMI-PROD port 1"
            value={alias} onChange={(e) => setAlias(e.target.value)} />
        </Field>
        <Button kind="primary" needsAdmin disabled={busy || !wwpn.trim()} onClick={add}>Register</Button>
      </div>

      {(view.unmanaged?.length ?? 0) > 0 && (
        <p className="mt-3 text-[11px] text-amber-300/80">
          also on the target, not registered here (left untouched): {view.unmanaged!.join(' · ')}
        </p>
      )}
    </Panel>
  )
}

// ScopeEditor: per-initiator port + library scoping. "All" = '' scope
// (new ports/libraries are included automatically); unchecking "all"
// and leaving every box empty = explicit NO access (stored as '-').
function ScopeEditor({ view, i, libNames, onDone }: {
  view: TargetsView; i: InitiatorView; libNames: Map<number, string>; onDone: (v: TargetsView | null) => void
}) {
  const [alias, setAlias] = useState(i.alias)
  const [allPorts, setAllPorts] = useState(i.ports === '')
  const [ports, setPorts] = useState<string[]>(i.ports === '-' ? [] : splitCSV(i.ports))
  const [allLibs, setAllLibs] = useState(i.libraries === '')
  const [libs, setLibs] = useState<number[]>(i.libraries === '-' ? [] : splitCSV(i.libraries).map(Number))
  const [busy, setBusy] = useState(false)

  const togglePort = (w: string) =>
    setPorts((p) => p.includes(w) ? p.filter((x) => x !== w) : [...p, w])
  const toggleLib = (l: number) =>
    setLibs((p) => p.includes(l) ? p.filter((x) => x !== l) : [...p, l])

  const save = () => {
    if (i.logged_in && !window.confirm(
      `${i.wwpn} is logged in — saving briefly bounces its session. Continue?`)) return
    setBusy(true)
    api.updateACL(i.wwpn, {
      alias: alias.trim(),
      ports: allPorts ? null : ports,
      libraries: allLibs ? null : libs,
    })
      .then((v) => { notify(`access updated for ${i.wwpn}`); onDone(v) })
      .catch((e: Error) => { notify(e.message, true); setBusy(false) })
  }

  const cb = 'h-3.5 w-3.5 accent-sky-500'
  return (
    <div className="mt-3 space-y-3 rounded border border-zinc-800 bg-zinc-950/60 p-3 text-xs">
      <div className="grid grid-cols-1 gap-3 md:grid-cols-2">
        <Field label="alias">
          <input className={inputCls} value={alias} onChange={(e) => setAlias(e.target.value)} />
        </Field>
      </div>
      <div>
        <label className="flex items-center gap-2 text-zinc-300">
          <input type="checkbox" className={cb} checked={allPorts} onChange={(e) => setAllPorts(e.target.checked)} />
          all ports (including future ones)
        </label>
        {!allPorts && (
          <div className="mt-1.5 flex flex-wrap gap-3 pl-5">
            {view.fc.ports.map((p) => (
              <label key={p.wwpn} className="flex items-center gap-1.5 font-mono text-zinc-400">
                <input type="checkbox" className={cb} checked={ports.includes(p.wwpn)} onChange={() => togglePort(p.wwpn)} />
                {p.wwpn}
              </label>
            ))}
          </div>
        )}
        {!allPorts && ports.length === 0 && (
          <p className="mt-1 pl-5 text-[11px] text-amber-300/80">
            no ports selected — this initiator will be denied on every port
          </p>
        )}
      </div>
      <div>
        <label className="flex items-center gap-2 text-zinc-300">
          <input type="checkbox" className={cb} checked={allLibs} onChange={(e) => setAllLibs(e.target.checked)} />
          all libraries (including future ones)
        </label>
        {!allLibs && (
          <div className="mt-1.5 flex flex-wrap gap-3 pl-5">
            {view.libraries.map((l) => (
              <label key={l} className="flex items-center gap-1.5 font-mono text-zinc-400" title={`library id ${l}`}>
                <input type="checkbox" className={cb} checked={libs.includes(l)} onChange={() => toggleLib(l)} />
                {libNames.get(l) ?? `library ${l}`}
              </label>
            ))}
          </div>
        )}
        {!allLibs && libs.length === 0 && (
          <p className="mt-1 pl-5 text-[11px] text-amber-300/80">
            no libraries selected — this initiator will see no LUNs
          </p>
        )}
      </div>
      <div className="flex gap-2">
        <Button kind="primary" needsAdmin disabled={busy} onClick={save}>Save access</Button>
        <Button onClick={() => onDone(null)}>Cancel</Button>
      </div>
    </div>
  )
}

// ------------------------------------------------------------ LUN map ------

function LUNPanel({ view, libNames }: { view: TargetsView; libNames: Map<number, string> }) {
  return (
    <Panel title="LUN map" right={
      <span className="text-xs text-zinc-600">same on every port</span>
    }>
      <table className="w-full text-left text-xs">
        <thead>
          <tr className="border-b border-zinc-800 text-[10px] uppercase tracking-wider text-zinc-500">
            <th className="py-1.5 pr-3 font-medium">lun</th>
            <th className="py-1.5 pr-3 font-medium">backstore</th>
            <th className="py-1.5 pr-3 font-medium">device</th>
            <th className="py-1.5 font-medium">library</th>
          </tr>
        </thead>
        <tbody>
          {view.luns.map((l) => (
            <tr key={l.lun} className="border-b border-zinc-800/60 font-mono">
              <td className="py-1.5 pr-3 text-zinc-300">{l.lun}</td>
              <td className="py-1.5 pr-3 text-sky-300">{l.backstore}</td>
              <td className="py-1.5 pr-3 text-zinc-500">{l.device || '—'}</td>
              <td className="py-1.5 text-zinc-400" title={`library id ${l.library}`}>
                {libNames.get(l.library) ?? l.library}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </Panel>
  )
}
