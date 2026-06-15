import { useState } from 'react'
import s from './settings.module.css'
import { InfoTerm } from './InfoTerm'
import { ConsequenceNote } from './ConsequenceNote'
import { Banner, Button, TextInput } from '../ui/ui'
import { normalizeOrigin, type SettingsDraft } from './mergePatch'

export function CorsSection({
  draft,
  onChange,
}: {
  draft: SettingsDraft
  onChange: (patch: Partial<SettingsDraft>) => void
}) {
  const [entry, setEntry] = useState('')
  const [error, setError] = useState<string | null>(null)
  const enabledEmpty = draft.corsEnabled && draft.corsOrigins.length === 0

  const add = () => {
    const norm = normalizeOrigin(entry)
    if (!norm) {
      setError('Enter a full origin: scheme + host (+ port). No path or trailing slash.')
      return
    }
    if (draft.corsOrigins.includes(norm)) {
      setError('Already added.')
      return
    }
    onChange({ corsOrigins: [...draft.corsOrigins, norm] })
    setEntry('')
    setError(null)
  }
  const remove = (o: string) => onChange({ corsOrigins: draft.corsOrigins.filter((x) => x !== o) })

  return (
    <div className={s.section}>
      <div className={s.sectionHead}>
        Cross-origin uploads <span className={`${s.effectBadge} ${s.effectLive}`}>live</span>
      </div>
      <p className={s.sectionSub}>
        <InfoTerm id="cors">CORS</InfoTerm> constrains browsers, not <code>curl</code> — it only governs
        which web origins a browser will let script the upload endpoint.
      </p>

      <label className={s.toggle}>
        <input
          type="checkbox"
          checked={draft.corsEnabled}
          onChange={(e) => onChange({ corsEnabled: e.target.checked })}
        />
        <span className={s.lbl}>Enable CORS on the upload endpoint</span>
      </label>

      <div className={s.originList}>
        {draft.corsOrigins.map((o) => (
          <div key={o} className={s.originRow}>
            <span className={s.originChip}>{o}</span>
            <button className={s.originX} aria-label={`Remove ${o}`} onClick={() => remove(o)}>
              ✕
            </button>
          </div>
        ))}
      </div>

      <div className={s.addOrigin}>
        <TextInput
          value={entry}
          placeholder="https://app.example.com"
          onChange={(e) => setEntry(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === 'Enter') {
              e.preventDefault()
              add()
            }
          }}
        />
        <Button size="sm" onClick={add}>
          + Add origin
        </Button>
      </div>
      {error && <ConsequenceNote tone="warn">{error}</ConsequenceNote>}
      {enabledEmpty && (
        <Banner tone="warn">
          CORS is enabled with no allowed origins — it will match nothing. Add an origin or disable CORS.
        </Banner>
      )}
    </div>
  )
}

// corsBlocksSave reports the save-blocking state (consumed by Settings.tsx).
export function corsBlocksSave(draft: SettingsDraft): boolean {
  return draft.corsEnabled && draft.corsOrigins.length === 0
}
