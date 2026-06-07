import * as D from '@radix-ui/react-dialog'
import type { ReactNode } from 'react'
import { Button } from './ui'
import s from './Dialog.module.css'

export function Modal({
  open,
  onOpenChange,
  title,
  children,
  footer,
  wide,
}: {
  open: boolean
  onOpenChange: (o: boolean) => void
  title: ReactNode
  children: ReactNode
  footer?: ReactNode
  wide?: boolean
}) {
  return (
    <D.Root open={open} onOpenChange={onOpenChange}>
      <D.Portal>
        <D.Overlay className={s.overlay} />
        <D.Content className={wide ? `${s.content} ${s.wide}` : s.content}>
          <D.Title className={s.title}>{title}</D.Title>
          <div className={s.body}>{children}</div>
          {footer && <div className={s.footer}>{footer}</div>}
          <D.Close asChild>
            <button className={s.x} aria-label="Close">
              ✕
            </button>
          </D.Close>
        </D.Content>
      </D.Portal>
    </D.Root>
  )
}

export function ConfirmDialog({
  open,
  onOpenChange,
  title,
  children,
  confirmLabel = 'Confirm',
  danger,
  busy,
  onConfirm,
}: {
  open: boolean
  onOpenChange: (o: boolean) => void
  title: ReactNode
  children: ReactNode
  confirmLabel?: string
  danger?: boolean
  busy?: boolean
  onConfirm: () => void
}) {
  return (
    <Modal
      open={open}
      onOpenChange={onOpenChange}
      title={title}
      footer={
        <>
          <Button onClick={() => onOpenChange(false)} disabled={busy}>
            Cancel
          </Button>
          <Button variant={danger ? 'danger' : 'primary'} onClick={onConfirm} disabled={busy}>
            {busy ? 'Working…' : confirmLabel}
          </Button>
        </>
      }
    >
      {children}
    </Modal>
  )
}
