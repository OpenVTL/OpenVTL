import { useState } from 'react'
import { api } from '../api'
import { inputCls } from '../components/ui'
import type { User } from '../types'

// Login + first-run setup, one centered card. Setup only exists while
// the user table is empty (the server enforces it; this is just UX).
export default function Login({ mode, onSignedIn }: {
  mode: 'login' | 'setup'
  onSignedIn: (u: User) => void
}) {
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [confirm, setConfirm] = useState('')
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  const setup = mode === 'setup'

  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    setError('')
    if (setup && password !== confirm) {
      setError('passwords do not match')
      return
    }
    setBusy(true)
    try {
      const r = setup ? await api.setup(username, password) : await api.login(username, password)
      onSignedIn(r.user)
    } catch (err) {
      setError((err as Error).message)
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="flex min-h-screen items-center justify-center px-6">
      <div className="w-full max-w-sm">
        <div className="mb-6 text-center">
          <h1>
            <img src="/brand/icon.svg" alt="" className="mx-auto mb-4 h-[288px] w-auto" />
            <img src="/brand/wordmark-slogan.svg" alt="OpenVTL" className="mx-auto h-20 w-auto" />
          </h1>
          <p className="mt-2 text-sm text-zinc-500">
            {setup ? 'first run — create the administrator account' : 'sign in to continue'}
          </p>
        </div>
        <form onSubmit={submit}
          className="space-y-4 rounded-lg border border-zinc-800 bg-zinc-900/40 p-6">
          <label className="block text-xs">
            <span className="mb-1 block text-zinc-500">username</span>
            <input className={inputCls} autoFocus autoComplete="username"
              value={username} onChange={(e) => setUsername(e.target.value)} />
          </label>
          <label className="block text-xs">
            <span className="mb-1 block text-zinc-500">password</span>
            <input className={inputCls} type="password"
              autoComplete={setup ? 'new-password' : 'current-password'}
              value={password} onChange={(e) => setPassword(e.target.value)} />
          </label>
          {setup && (
            <label className="block text-xs">
              <span className="mb-1 block text-zinc-500">confirm password</span>
              <input className={inputCls} type="password" autoComplete="new-password"
                value={confirm} onChange={(e) => setConfirm(e.target.value)} />
            </label>
          )}
          {setup && (
            <p className="text-xs text-zinc-600">
              At least 8 characters. This account gets the admin role; more users
              (admin or read-only) can be added under Settings afterwards.
            </p>
          )}
          {error && (
            <div className="rounded border border-red-500/40 bg-red-950/50 px-3 py-2 text-xs text-red-300">
              {error}
            </div>
          )}
          <button type="submit" disabled={busy || !username || !password}
            className="w-full rounded border border-sky-700 bg-sky-600/20 px-3 py-2 text-sm font-medium text-sky-300 transition-colors hover:bg-sky-600/35 disabled:cursor-not-allowed disabled:opacity-40">
            {busy ? '…' : setup ? 'Create admin account' : 'Sign in'}
          </button>
        </form>
      </div>
    </div>
  )
}
