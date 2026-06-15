import s from './settings.module.css'
import { InfoTerm } from './InfoTerm'
import { ConsequenceNote } from './ConsequenceNote'
import { Field, TextInput } from '../ui/ui'
import type { SettingsDraft } from './mergePatch'

export function PublicUploadsSection({
  draft,
  onChange,
}: {
  draft: SettingsDraft
  onChange: (patch: Partial<SettingsDraft>) => void
}) {
  const needsTos = draft.publicUploads && draft.tosUrl.trim() === ''
  return (
    <div className={s.section}>
      <div className={s.sectionHead}>
        Public uploads <span className={`${s.effectBadge} ${s.effectRestart}`}>restart</span>
      </div>
      <label className={s.toggle}>
        <input
          type="checkbox"
          checked={draft.publicUploads}
          onChange={(e) => onChange({ publicUploads: e.target.checked })}
        />
        <span className={s.lbl}>
          Allow <InfoTerm id="public-uploads">anonymous public uploads</InfoTerm>
        </span>
      </label>
      {draft.publicUploads && (
        <ConsequenceNote tone="warn">
          Anyone can upload through this node — this widens your abuse surface and requires a published
          Terms of Service.
        </ConsequenceNote>
      )}
      <div style={{ marginTop: 10, maxWidth: 460 }}>
        <Field label="Terms-of-Service URL">
          <TextInput
            value={draft.tosUrl}
            placeholder="https://example.org/terms"
            onChange={(e) => onChange({ tosUrl: e.target.value })}
          />
        </Field>
        {needsTos && (
          <ConsequenceNote tone="warn">
            A <InfoTerm id="tos">ToS URL</InfoTerm> is required when public uploads are enabled (T1.20).
          </ConsequenceNote>
        )}
      </div>
    </div>
  )
}

// publicUploadsBlocksSave: the local guard mirroring the T1.20 backend hard-fail.
export function publicUploadsBlocksSave(draft: SettingsDraft): boolean {
  return draft.publicUploads && draft.tosUrl.trim() === ''
}
