import type { ReactNode } from 'react'
import s from './wizard.module.css'

// ConsequenceNote is a one-line consequence under a control. `warn` marks that
// the operator is relaxing a protection; `note` is neutral clarification.
export function ConsequenceNote({
  tone = 'note',
  children,
}: {
  tone?: 'note' | 'warn'
  children: ReactNode
}) {
  return <p className={tone === 'warn' ? s.consequenceWarn : s.consequence}>{children}</p>
}
