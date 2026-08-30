import { createContext, useCallback, useContext, useEffect, useState } from 'react'
import { api } from './api'
import type { User } from './types'

// Login-shell state machine: loading → setup (no users yet) | anon |
// authed. A global 'auth:required' event (fired by the api layer on
// any 401) drops an expired session back to the login screen.
export type AuthPhase =
  | { phase: 'loading' }
  | { phase: 'setup' }
  | { phase: 'anon' }
  | { phase: 'authed'; user: User }

export function useAuth() {
  const [state, setState] = useState<AuthPhase>({ phase: 'loading' })

  const refresh = useCallback(async () => {
    try {
      const b = await api.me()
      if (b.setup_required) setState({ phase: 'setup' })
      else if (b.user) setState({ phase: 'authed', user: b.user })
      else setState({ phase: 'anon' })
    } catch {
      // daemon unreachable: show login; submit will surface the error
      setState({ phase: 'anon' })
    }
  }, [])

  useEffect(() => {
    refresh()
    const onRequired = () => setState((s) => (s.phase === 'authed' ? { phase: 'anon' } : s))
    window.addEventListener('auth:required', onRequired)
    return () => window.removeEventListener('auth:required', onRequired)
  }, [refresh])

  const signedIn = (user: User) => setState({ phase: 'authed', user })
  const logout = async () => {
    try { await api.logout() } catch { /* cookie cleared server-side regardless */ }
    setState({ phase: 'anon' })
  }

  return { state, refresh, signedIn, logout }
}

// Context consumed by role-aware controls (Button needsAdmin) and the
// header chip. Only meaningful under an authed shell.
export interface AuthCtx {
  user: User | null
  admin: boolean
  logout: () => void
}

export const AuthContext = createContext<AuthCtx>({ user: null, admin: false, logout: () => {} })
export const useAuthCtx = () => useContext(AuthContext)
