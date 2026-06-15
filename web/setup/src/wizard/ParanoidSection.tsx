import { useEffect, useRef } from 'react'
import s from './wizard.module.css'
import { InfoTerm } from './InfoTerm'
import { ConsequenceNote } from './ConsequenceNote'

// Protective state: each flag true = hardened. Mirrors operator intent; the
// wizard maps these to explicit operator.yaml values in toAnswers().
export interface ParanoidState {
  hardenNoIPRecording: boolean
  hardenShortRetention: boolean
  hardenPrivateDHT: boolean
}

const CHILD_KEYS = ['hardenNoIPRecording', 'hardenShortRetention', 'hardenPrivateDHT'] as const

export function ParanoidSection({
  value,
  onChange,
}: {
  value: ParanoidState
  onChange: (next: ParanoidState) => void
}) {
  const allOn = CHILD_KEYS.every((k) => value[k])
  const noneOn = CHILD_KEYS.every((k) => !value[k])
  const indeterminate = !allOn && !noneOn

  // `indeterminate` is a DOM property, not an attribute — set it imperatively.
  const parentRef = useRef<HTMLInputElement>(null)
  useEffect(() => {
    if (parentRef.current) parentRef.current.indeterminate = indeterminate
  }, [indeterminate])

  const toggleParent = () => {
    const next = !allOn // fully on → clear all; otherwise → set all
    onChange({ hardenNoIPRecording: next, hardenShortRetention: next, hardenPrivateDHT: next })
  }
  const setChild = (k: (typeof CHILD_KEYS)[number], v: boolean) => onChange({ ...value, [k]: v })

  return (
    <div className={s.paranoid}>
      <label className={s.toggle}>
        <input
          ref={parentRef}
          type="checkbox"
          checked={allOn}
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
            checked={value.hardenNoIPRecording}
            onChange={(e) => setChild('hardenNoIPRecording', e.target.checked)}
          />
          <span className={s.lbl}>
            Don't record <InfoTerm id="source-ip">uploader IP addresses</InfoTerm>
          </span>
        </label>
        {!value.hardenNoIPRecording && (
          <ConsequenceNote tone="warn">Each blob will store the client's source IP.</ConsequenceNote>
        )}

        <label className={s.childToggle}>
          <input
            type="checkbox"
            checked={value.hardenShortRetention}
            onChange={(e) => setChild('hardenShortRetention', e.target.checked)}
          />
          <span className={s.lbl}>Keep IP logs 1 day, not 30</span>
        </label>
        {value.hardenNoIPRecording ? (
          <ConsequenceNote>Only applies while IP recording is on.</ConsequenceNote>
        ) : value.hardenShortRetention ? null : (
          <ConsequenceNote tone="warn">Recorded IPs are kept 30 days.</ConsequenceNote>
        )}

        <label className={s.childToggle}>
          <input
            type="checkbox"
            checked={value.hardenPrivateDHT}
            onChange={(e) => setChild('hardenPrivateDHT', e.target.checked)}
          />
          <span className={s.lbl}>
            Keep pinned CIDs off the <InfoTerm id="ipfs-dht">public IPFS DHT</InfoTerm>
          </span>
        </label>
        {!value.hardenPrivateDHT && (
          <ConsequenceNote tone="warn">This node will advertise stored CIDs publicly.</ConsequenceNote>
        )}

        <div className={s.infoRow}>
          · No outbound webhooks <span className={s.muted}>(none configured)</span>
        </div>
        <div className={s.infoRow}>
          · Metrics stay loopback-only <span className={s.muted}>(not internet-exposed)</span>
        </div>
      </div>
    </div>
  )
}
