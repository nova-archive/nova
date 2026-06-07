import type { ReactNode } from 'react'
import { Navigate, useLocation } from 'react-router-dom'
import { useAuth } from '../auth/AuthProvider'

export function RequireAuth({ children }: { children: ReactNode }) {
  const { ready, user } = useAuth()
  const loc = useLocation()

  if (!ready) {
    return (
      <div
        style={{
          height: '100vh',
          display: 'grid',
          placeItems: 'center',
          color: 'var(--ink-faint)',
          fontFamily: 'var(--mono)',
          fontSize: 12,
          letterSpacing: '0.1em',
        }}
      >
        loading console…
      </div>
    )
  }
  if (!user) {
    return <Navigate to="/login" replace state={{ from: loc.pathname + loc.search }} />
  }
  return <>{children}</>
}
