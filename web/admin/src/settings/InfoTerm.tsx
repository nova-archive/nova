import type { ReactNode } from 'react'
import s from './settings.module.css'
import { GLOSSARY } from './glossary'

// InfoTerm renders a "learn-this" jargon term: the term text (dotted underline)
// followed by a native <details> ⓘ disclosure. Native <details> is keyboard- and
// screen-reader-accessible; the <summary> carries the accessible name and the
// glyph is decorative. Unknown ids render as plain text (no button).
export function InfoTerm({ id, children }: { id: string; children: ReactNode }) {
  const entry = GLOSSARY[id]
  if (!entry) return <>{children}</>
  return (
    <span className={s.infoTerm}>
      <span className={s.infoLabel}>{children}</span>
      <details className={s.infoDetails}>
        <summary className={s.infoSummary} aria-label={entry.label}>
          <span aria-hidden="true">ⓘ</span>
        </summary>
        <span className={s.infoBody}>{entry.body}</span>
      </details>
    </span>
  )
}
