import { useEffect, useRef } from 'react'
import s from './settings.module.css'
import { InfoTerm } from './InfoTerm'
import { ConsequenceNote } from './ConsequenceNote'
import { Banner } from '../ui/ui'
import type { SettingsDraft } from './mergePatch'

const DEFAULT_RETENTION = 30

// ParanoidSection edits the three privacy constituents as a tri-state "select all"
// parent. Protective ⇔ all three children hardened AND webhooks empty; when
// webhooks are configured the parent can never reach fully-checked (it cannot clear
// webhooks), so it stays indeterminate — keeping auth.paranoid derivable as warn-free.
export function ParanoidSection({
  draft,
  loadedRetentionDays,
  webhooksConfigured,
  privacyWarnings,
  onChange,
}: {
  draft: SettingsDraft
  loadedRetentionDays: number
  webhooksConfigured: boolean
  privacyWarnings: string[]
  onChange: (patch: Partial<SettingsDraft>) => void
}) {
  const shortRetention = draft.retentionDays <= 1
  const childrenAllOn = draft.hardenNoIPRecording && shortRetention && draft.hardenPrivateDHT
  const childrenNoneOn = !draft.hardenNoIPRecording && !shortRetention && !draft.hardenPrivateDHT
  const fullyProtective = childrenAllOn && !webhooksConfigured
  const indeterminate = !fullyProtective && !childrenNoneOn

  const parentRef = useRef<HTMLInputElement>(null)
  useEffect(() => {
    if (parentRef.current) parentRef.current.indeterminate = indeterminate
  }, [indeterminate])

  const restoreRetention = () =>
    loadedRetentionDays > 1 ? loadedRetentionDays : DEFAULT_RETENTION

  const toggleParent = () => {
    const next = !childrenAllOn // fully on → relax all; else → harden all
    onChange({
      hardenNoIPRecording: next,
      hardenPrivateDHT: next,
      retentionDays: next ? 1 : restoreRetention(),
    })
  }
  const toggleRetention = (checked: boolean) =>
    onChange({ retentionDays: checked ? 1 : restoreRetention() })

  return (
    <div className={s.section}>
      <div className={s.sectionHead}>
        Privacy &amp; hardening{' '}
        <span className={`${s.effectBadge} ${s.effectRestart}`}>restart</span>
      </div>
      {privacyWarnings.length > 0 && (
        <Banner tone="warn">
          {privacyWarnings.map((w, i) => (
            <div key={i}>{w}</div>
          ))}
        </Banner>
      )}

      <label className={s.toggle}>
        <input
          ref={parentRef}
          type="checkbox"
          checked={fullyProtective}
          onChange={toggleParent}
          aria-label="Harden privacy (paranoid)"
        />
        <span className={s.lbl}>
          Harden privacy (<InfoTerm id="paranoid">paranoid</InfoTerm>)
          {indeterminate && <span className={s.partial}> — partial</span>}
        </span>
      </label>
      <span className={s.hint}>Sets every protection below. Uncheck any one to relax it.</span>

      <div className={s.children}>
        <label className={s.childToggle}>
          <input
            type="checkbox"
            checked={draft.hardenNoIPRecording}
            onChange={(e) => onChange({ hardenNoIPRecording: e.target.checked })}
          />
          <span className={s.lbl}>
            Don't record <InfoTerm id="source-ip">uploader IP addresses</InfoTerm>
          </span>
        </label>
        {!draft.hardenNoIPRecording && (
          <ConsequenceNote tone="warn">Each blob will store the client's source IP.</ConsequenceNote>
        )}

        <label className={s.childToggle}>
          <input type="checkbox" checked={shortRetention} onChange={(e) => toggleRetention(e.target.checked)} />
          <span className={s.lbl}>Keep IP logs 1 day, not 30</span>
        </label>
        {draft.hardenNoIPRecording ? (
          <ConsequenceNote>Only applies while IP recording is on.</ConsequenceNote>
        ) : shortRetention ? null : (
          <ConsequenceNote tone="warn">Recorded IPs are kept {draft.retentionDays} days.</ConsequenceNote>
        )}

        <label className={s.childToggle}>
          <input
            type="checkbox"
            checked={draft.hardenPrivateDHT}
            onChange={(e) => onChange({ hardenPrivateDHT: e.target.checked })}
          />
          <span className={s.lbl}>
            Keep pinned CIDs off the <InfoTerm id="ipfs-dht">public IPFS DHT</InfoTerm>
          </span>
        </label>
        {!draft.hardenPrivateDHT && (
          <ConsequenceNote tone="warn">This node will advertise stored CIDs publicly.</ConsequenceNote>
        )}

        <div className={s.infoRow}>
          ·{' '}
          {webhooksConfigured ? (
            <>
              Outbound webhooks <span className={s.muted}>configured</span> — clear via{' '}
              <code>novactl config</code> to represent the full preset warning-free
            </>
          ) : (
            <>
              No outbound webhooks <span className={s.muted}>(none configured)</span>
            </>
          )}
        </div>
        <div className={s.infoRow}>
          · Metrics stay loopback-only <span className={s.muted}>(not internet-exposed)</span>
        </div>
      </div>
    </div>
  )
}
