import { useState, type FormEvent } from 'react'
import { Navigate, useLocation } from 'react-router-dom'
import { useAuth } from '../auth/AuthProvider'
import { Banner, Button } from '../ui/ui'
import s from './Login.module.css'

function hostOf(u?: string): string {
  try {
    return u ? new URL(u).host : 'your identity provider'
  } catch {
    return 'your identity provider'
  }
}

export function LoginScreen() {
  const { ready, config, user, error, loginLocal, beginExternalLogin } = useAuth()
  const loc = useLocation() as { state?: { from?: string } }
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [busy, setBusy] = useState(false)

  if (user) return <Navigate to={loc.state?.from || '/blobs'} replace />

  const external = ready && config?.mode === 'external'

  const onSubmit = async (e: FormEvent) => {
    e.preventDefault()
    setBusy(true)
    try {
      await loginLocal(username, password)
    } catch {
      // error surfaced via auth.error
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className={s.wrap}>
      <div className={s.card}>
        <div className={s.brand}>
          <span className={s.mark}>nova</span>
        </div>
        <div className={s.kicker}>Operator Console</div>

        {external ? (
          <>
            <p className={s.lead}>
              This deployment authenticates through an external identity provider.
            </p>
            <Button variant="primary" className={s.full} onClick={() => void beginExternalLogin()}>
              Continue with {hostOf(config?.issuer_url)} →
            </Button>
          </>
        ) : (
          <form className={s.form} onSubmit={onSubmit}>
            {error && <Banner>{error}</Banner>}
            <label className={s.lbl}>
              Email
              <input
                className={s.in}
                type="text"
                autoFocus
                autoComplete="username"
                value={username}
                onChange={(e) => setUsername(e.target.value)}
              />
            </label>
            <label className={s.lbl}>
              Password
              <input
                className={s.in}
                type="password"
                autoComplete="current-password"
                value={password}
                onChange={(e) => setPassword(e.target.value)}
              />
            </label>
            <Button
              variant="primary"
              className={s.full}
              type="submit"
              disabled={busy || !username || !password}
            >
              {busy ? 'Signing in…' : 'Sign in'}
            </Button>
          </form>
        )}
      </div>
      <div className={s.foot}>Nova · federated content-addressed archive</div>
    </div>
  )
}
