import * as T from '@radix-ui/react-toast'
import { createContext, useCallback, useContext, useState, type ReactNode } from 'react'
import { cls } from './ui'
import s from './toast.module.css'

interface ToastMsg {
  id: number
  title: string
  desc?: string
  tone: 'ok' | 'err'
}

interface ToastApi {
  ok: (title: string, desc?: string) => void
  err: (title: string, desc?: string) => void
}

const Ctx = createContext<ToastApi | null>(null)

export function useToast(): ToastApi {
  const v = useContext(Ctx)
  if (!v) throw new Error('useToast must be used within <ToastProvider>')
  return v
}

export function ToastProvider({ children }: { children: ReactNode }) {
  const [items, setItems] = useState<ToastMsg[]>([])

  const push = useCallback((tone: 'ok' | 'err', title: string, desc?: string) => {
    setItems((xs) => [...xs, { id: Date.now() + Math.random(), tone, title, desc }])
  }, [])

  const api: ToastApi = {
    ok: (title, desc) => push('ok', title, desc),
    err: (title, desc) => push('err', title, desc),
  }

  return (
    <Ctx.Provider value={api}>
      <T.Provider swipeDirection="right" duration={4500}>
        {children}
        {items.map((it) => (
          <T.Root
            key={it.id}
            className={cls(s.toast, it.tone === 'err' && s.err)}
            onOpenChange={(open) => {
              if (!open) setItems((xs) => xs.filter((x) => x.id !== it.id))
            }}
          >
            <T.Title className={s.title}>{it.title}</T.Title>
            {it.desc && <T.Description className={s.desc}>{it.desc}</T.Description>}
          </T.Root>
        ))}
        <T.Viewport className={s.viewport} />
      </T.Provider>
    </Ctx.Provider>
  )
}
