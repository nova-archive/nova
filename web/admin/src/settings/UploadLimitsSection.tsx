import s from './settings.module.css'
import { formatBytes } from '../lib/format'
import type { SettingsDraft } from './mergePatch'

type NumKey =
  | 'maxUploadSizeBytes'
  | 'maxConcurrentAssembly'
  | 'maxConcurrentGlobal'
  | 'maxConcurrentPerSession'
  | 'maxFilesPerSession'

const FIELDS: Array<{ key: NumKey; label: string; bytes?: boolean }> = [
  { key: 'maxUploadSizeBytes', label: 'Max upload size (bytes)', bytes: true },
  { key: 'maxConcurrentAssembly', label: 'Max concurrent assembly' },
  { key: 'maxConcurrentGlobal', label: 'Max concurrent uploads (global)' },
  { key: 'maxConcurrentPerSession', label: 'Max concurrent uploads (per session)' },
  { key: 'maxFilesPerSession', label: 'Max files per session' },
]

export function UploadLimitsSection({
  draft,
  onChange,
}: {
  draft: SettingsDraft
  onChange: (patch: Partial<SettingsDraft>) => void
}) {
  return (
    <div className={s.section}>
      <div className={s.sectionHead}>
        Upload limits <span className={`${s.effectBadge} ${s.effectLive}`}>live</span>
      </div>
      <p className={s.sectionSub}>
        Leave positive — zero or negative is normalized by the config loader to Nova's default.
      </p>
      <div className={s.row}>
        {FIELDS.map((f) => (
          <label key={f.key} className={s.numField}>
            <span className={s.numLabel}>{f.label}</span>
            <input
              className={s.input}
              type="number"
              min={0}
              value={draft[f.key]}
              aria-label={f.label}
              onChange={(e) => onChange({ [f.key]: Number(e.target.value) } as Partial<SettingsDraft>)}
            />
            {f.bytes && <span className={s.numHint}>{formatBytes(draft.maxUploadSizeBytes)}</span>}
          </label>
        ))}
      </div>
    </div>
  )
}
