import { useEffect, useRef, useState } from 'react'
import { Navigate, useSearchParams } from 'react-router-dom'
import { useAuth } from '../auth/AuthProvider'
import { Banner } from '../ui/ui'
import s from './Login.module.css'

export function CallbackScreen() {
  const { ready, user, completeExternalLogin } = useAuth()
  const [params] = useSearchParams()
  const [err, setErr] = useState<string | null>(null)
  const ran = useRef(false)

  useEffect(() => {
    if (!ready || ran.current) return
    ran.current = true
    completeExternalLogin(params).catch((e: unknown) =>
      setErr(e instanceof Error ? e.message : String(e)),
    )
  }, [ready, params, completeExternalLogin])

  if (user) return <Navigate to="/blobs" replace />
  return (
    <div className={s.center}>
      {err ? <Banner>Sign-in failed: {err}</Banner> : 'completing sign-in…'}
    </div>
  )
}
