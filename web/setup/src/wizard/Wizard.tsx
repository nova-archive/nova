import { useCallback, useEffect, useMemo, useState } from 'react'
import { ApiError, createSetupApi, type MasterKey, type SetupApi } from '../api/client'
import { buildBackupContent } from '../backup'
import { initialForm, toAnswers, type FormState, type TLSMode } from './types'
import s from './wizard.module.css'

// The linear stepper. Each step is a self-contained render; navigation is gated
// by per-step `canNext`. The master-key step's readback gate (typed fingerprint
// must equal the returned fingerprint) and the final commit → orientation are
// the load-bearing flows.
const STEPS = [
  'token',
  'welcome',
  'master-key',
  'keys',
  'admin',
  'tls',
  'tos',
  'paranoid',
  'review',
  'commit',
  'orientation',
] as const
type StepId = (typeof STEPS)[number]

const MIN_PASSWORD = 12

const TLS_NOTES: Record<TLSMode, string> = {
  'dev-self-signed': 'Local-only self-signed certificate. Browsers will warn; fine for testing.',
  'http-01': 'Let’s Encrypt over HTTP-01 — your hostname is published to public Certificate Transparency logs.',
  'dns-01': 'Let’s Encrypt over DNS-01 — requires manual operator steps after setup.',
  static: 'Bring your own certificate and key files.',
  onion: 'Tor onion service — requires manual operator steps after setup.',
}

export interface WizardProps {
  // Injectable for tests; defaults to the same-origin /setup/* client.
  api?: SetupApi
}

export function Wizard({ api: injected }: WizardProps = {}) {
  const [idx, setIdx] = useState(0)
  const [form, setForm] = useState<FormState>(initialForm)

  // Tests inject a mock api (used as-is). Otherwise build the real same-origin
  // client once the bootstrap token is entered: it closes over the token and
  // sends it as X-Nova-Setup-Token on every /setup/* request. The first real
  // API call (generateMasterKey) is on the master-key step, which is after the
  // token step, so the token is always set before the client is used.
  const api = useMemo<SetupApi>(
    () => injected ?? createSetupApi(form.bootstrapToken),
    [injected, form.bootstrapToken],
  )

  // Master-key material (generated on entering the master-key step) and the
  // readback the operator types to prove they captured the fingerprint.
  const [masterKey, setMasterKey] = useState<MasterKey | null>(null)
  const [keyErr, setKeyErr] = useState<string | null>(null)
  const [readback, setReadback] = useState('')

  // Review/commit error surfaced inline.
  const [submitErr, setSubmitErr] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)

  const step: StepId = STEPS[idx]
  const set = useCallback(<K extends keyof FormState>(k: K, v: FormState[K]) => {
    setForm((f) => ({ ...f, [k]: v }))
  }, [])

  // Generate the master key exactly once, when the operator first lands on the
  // master-key step.
  useEffect(() => {
    if (step !== 'master-key' || masterKey || keyErr) return
    let live = true
    api
      .generateMasterKey()
      .then((k) => {
        if (live) setMasterKey(k)
      })
      .catch((e: unknown) => {
        if (live) setKeyErr(e instanceof Error ? e.message : 'failed to generate master key')
      })
    return () => {
      live = false
    }
  }, [step, masterKey, keyErr, api])

  const fingerprintMatches =
    !!masterKey && readback.trim() === masterKey.fingerprint

  const downloadBackup = () => {
    if (!masterKey) return
    const blob = new Blob([buildBackupContent(masterKey.master_key_hex, masterKey.fingerprint)], {
      type: 'text/plain',
    })
    const url = URL.createObjectURL(blob)
    const a = document.createElement('a')
    a.href = url
    a.download = 'nova-master-key.txt'
    document.body.appendChild(a)
    a.click()
    document.body.removeChild(a)
    URL.revokeObjectURL(url)
  }

  // Per-step gate for the Next/Continue control.
  const canNext = (): boolean => {
    switch (step) {
      case 'token':
        return form.bootstrapToken.trim().length > 0
      case 'welcome':
        return form.hostname.trim().length > 0 && form.contact_email.trim().length > 0
      case 'master-key':
        return fingerprintMatches
      case 'admin':
        return (
          form.admin_email.trim().length > 0 && form.admin_password.length >= MIN_PASSWORD
        )
      case 'tls':
        if (form.tls_mode === 'static')
          return form.cert_path.trim().length > 0 && form.key_path.trim().length > 0
        return true
      case 'tos':
        return form.public_uploads ? form.tos_url.trim().length > 0 : true
      default:
        return true
    }
  }

  const goNext = async () => {
    setSubmitErr(null)
    if (step === 'review') {
      setBusy(true)
      try {
        await api.submitAnswers(toAnswers(form))
        setIdx((i) => i + 1)
      } catch (e) {
        if (e instanceof ApiError && e.code === 'invalid_answers') {
          setSubmitErr(e.message)
        } else {
          setSubmitErr(e instanceof Error ? e.message : 'failed to submit answers')
        }
      } finally {
        setBusy(false)
      }
      return
    }
    if (step === 'commit') {
      setBusy(true)
      try {
        await api.commit(toAnswers(form))
        setIdx((i) => i + 1)
      } catch (e) {
        setSubmitErr(e instanceof Error ? e.message : 'failed to commit setup')
      } finally {
        setBusy(false)
      }
      return
    }
    setIdx((i) => Math.min(i + 1, STEPS.length - 1))
  }

  const goBack = () => {
    setSubmitErr(null)
    setIdx((i) => Math.max(i - 1, 0))
  }

  const nextLabel =
    step === 'review' ? 'Submit' : step === 'commit' ? 'Commit & go live' : 'Next'

  return (
    <div className={s.wrap}>
      <div className={s.card}>
        <div className={s.brand}>
          <span className={s.mark}>nova</span>
          <span className={s.kicker}>First-run setup</span>
        </div>

        <nav className={s.rail} aria-label="Setup progress">
          {STEPS.map((sid, i) => (
            <span
              key={sid}
              className={`${s.dot} ${i === idx ? s.dotActive : ''} ${i < idx ? s.dotDone : ''}`}
              aria-current={i === idx ? 'step' : undefined}
            />
          ))}
        </nav>

        {renderStep()}

        {step !== 'orientation' && (
          <div className={s.actions}>
            <button className={s.btn} onClick={goBack} disabled={idx === 0 || busy} type="button">
              Back
            </button>
            <button
              className={`${s.btn} ${s.primary}`}
              onClick={() => void goNext()}
              disabled={!canNext() || busy}
              type="button"
            >
              {busy ? 'Working…' : nextLabel}
            </button>
          </div>
        )}
      </div>
      <div className={s.foot}>Nova · federated content-addressed archive</div>
    </div>
  )

  function renderStep() {
    switch (step) {
      case 'token':
        return (
          <>
            <h1 className={s.h}>Bootstrap token</h1>
            <div className={s.step}>Step 1 · Bootstrap token</div>
            <p className={s.lead}>
              Setup is locked to whoever holds this node’s bootstrap token. Paste it below to
              authorize this wizard — every setup request carries it.
            </p>

            <label className={s.field} style={{ marginTop: 18 }}>
              <span className={s.lbl}>Bootstrap token</span>
              <input
                className={`${s.in} ${s.mono}`}
                type="text"
                autoComplete="off"
                spellCheck={false}
                aria-label="Bootstrap token"
                value={form.bootstrapToken}
                onChange={(e) => set('bootstrapToken', e.target.value)}
              />
              <span className={s.hint}>
                Copy it from the coordinator startup log
                (<span className={s.mono}>docker compose logs coordinator</span>) — it appears as{' '}
                <span className={s.mono}>bootstrap_token=…</span>.
              </span>
            </label>
          </>
        )

      case 'welcome':
        return (
          <>
            <h1 className={s.h}>Welcome to Nova</h1>
            <div className={s.step}>Step 2 · Welcome</div>
            <p className={s.lead}>
              This wizard configures your Nova node’s first run: it generates your master key,
              creates the operator account, and writes <span className={s.mono}>operator.yaml</span>.
              You’ll only do this once.
            </p>
            <p className={s.hint}>
              Nova is released under its project license. By continuing you agree to operate this
              node responsibly per your jurisdiction and the bundled terms.
            </p>

            <label className={s.field} style={{ marginTop: 18 }}>
              <span className={s.lbl}>Hostname</span>
              <input
                className={s.in}
                type="text"
                placeholder="nova.example.org"
                aria-label="Hostname"
                value={form.hostname}
                onChange={(e) => set('hostname', e.target.value)}
              />
            </label>

            <label className={s.field}>
              <span className={s.lbl}>Contact email</span>
              <input
                className={s.in}
                type="email"
                aria-label="Contact email"
                value={form.contact_email}
                onChange={(e) => set('contact_email', e.target.value)}
              />
            </label>

            <label className={s.field}>
              <span className={s.lbl}>Display name (optional)</span>
              <input
                className={s.in}
                type="text"
                aria-label="Display name"
                value={form.display_name}
                onChange={(e) => set('display_name', e.target.value)}
              />
            </label>
          </>
        )

      case 'master-key':
        return (
          <>
            <h1 className={s.h}>Your master key</h1>
            <div className={s.step}>Step 3 · Master key</div>
            <p className={s.lead}>
              This key encrypts your node’s secrets and <strong>cannot be recovered</strong>. Save
              the backup file somewhere safe, then type the fingerprint below to confirm you have it.
            </p>

            {keyErr && <div className={s.banner} role="alert">{keyErr}</div>}

            {!masterKey && !keyErr && <p className={s.hint}>Generating master key…</p>}

            {masterKey && (
              <>
                <div className={s.lbl}>Master key (hex)</div>
                <div className={s.keyBox} data-testid="master-key-hex">
                  {masterKey.master_key_hex}
                </div>

                <div className={s.fpRow}>
                  <span className={s.lbl}>Fingerprint:</span>
                  <span className={s.mono} data-testid="fingerprint">
                    {masterKey.fingerprint}
                  </span>
                </div>

                <button className={s.btn} type="button" onClick={downloadBackup}>
                  Download backup
                </button>

                <label className={s.field} style={{ marginTop: 18 }}>
                  <span className={s.lbl}>Confirm — type the fingerprint above</span>
                  <input
                    className={`${s.in} ${s.mono}`}
                    type="text"
                    value={readback}
                    autoComplete="off"
                    spellCheck={false}
                    onChange={(e) => setReadback(e.target.value)}
                    aria-label="Confirm fingerprint"
                  />
                  <span className={s.hint}>
                    {fingerprintMatches ? 'Fingerprint matches.' : 'Next stays disabled until this matches.'}
                  </span>
                </label>
              </>
            )}
          </>
        )

      case 'keys':
        return (
          <>
            <h1 className={s.h}>Node keys</h1>
            <div className={s.step}>Step 4 · Keys</div>
            <p className={s.lead}>
              Your swarm identity and content-signing keys are generated automatically and sealed
              with your master key when you commit. There’s nothing to enter here.
            </p>
            <p className={s.hint}>
              These keys live only on this node. Losing your master key means losing access to them.
            </p>
          </>
        )

      case 'admin':
        return (
          <>
            <h1 className={s.h}>Operator account</h1>
            <div className={s.step}>Step 5 · Admin user</div>
            <p className={s.lead}>The first operator can sign in to the admin console at /admin.</p>

            <label className={s.field}>
              <span className={s.lbl}>Admin email</span>
              <input
                className={s.in}
                type="email"
                autoComplete="username"
                aria-label="Admin email"
                value={form.admin_email}
                onChange={(e) => set('admin_email', e.target.value)}
              />
            </label>

            <label className={s.field}>
              <span className={s.lbl}>Admin password</span>
              <input
                className={s.in}
                type="password"
                autoComplete="new-password"
                aria-label="Admin password"
                value={form.admin_password}
                onChange={(e) => set('admin_password', e.target.value)}
              />
              <span className={form.admin_password.length >= MIN_PASSWORD ? s.hint : s.warn}>
                {form.admin_password.length >= MIN_PASSWORD
                  ? 'Password length looks good.'
                  : `Minimum ${MIN_PASSWORD} characters (${form.admin_password.length}/${MIN_PASSWORD}).`}
              </span>
            </label>
          </>
        )

      case 'tls':
        return (
          <>
            <h1 className={s.h}>TLS &amp; certificates</h1>
            <div className={s.step}>Step 6 · TLS mode</div>
            <p className={s.lead}>How should Nova obtain its HTTPS certificate?</p>

            <label className={s.field}>
              <span className={s.lbl}>TLS mode</span>
              <select
                className={s.sel}
                value={form.tls_mode}
                onChange={(e) => set('tls_mode', e.target.value as TLSMode)}
              >
                <option value="dev-self-signed">dev-self-signed</option>
                <option value="http-01">http-01</option>
                <option value="dns-01">dns-01</option>
                <option value="static">static</option>
                <option value="onion">onion</option>
              </select>
              <span className={form.tls_mode === 'http-01' ? s.warn : s.hint}>
                {TLS_NOTES[form.tls_mode]}
              </span>
            </label>

            {form.tls_mode === 'static' && (
              <>
                <label className={s.field}>
                  <span className={s.lbl}>Certificate path</span>
                  <input
                    className={s.in}
                    type="text"
                    value={form.cert_path}
                    onChange={(e) => set('cert_path', e.target.value)}
                  />
                </label>
                <label className={s.field}>
                  <span className={s.lbl}>Private key path</span>
                  <input
                    className={s.in}
                    type="text"
                    value={form.key_path}
                    onChange={(e) => set('key_path', e.target.value)}
                  />
                </label>
              </>
            )}
          </>
        )

      case 'tos':
        return (
          <>
            <h1 className={s.h}>Public uploads</h1>
            <div className={s.step}>Step 7 · Terms of service</div>
            <p className={s.lead}>
              Allow anyone to upload through your node’s public widget? If enabled, you must publish
              a terms-of-service URL (T1.20).
            </p>

            <label className={s.toggle}>
              <input
                type="checkbox"
                checked={form.public_uploads}
                onChange={(e) => set('public_uploads', e.target.checked)}
              />
              <span className={s.lbl}>Enable public uploads</span>
            </label>

            {form.public_uploads && (
              <label className={s.field}>
                <span className={s.lbl}>Terms of service URL (required)</span>
                <input
                  className={s.in}
                  type="url"
                  placeholder="https://example.org/terms"
                  value={form.tos_url}
                  onChange={(e) => set('tos_url', e.target.value)}
                />
              </label>
            )}
          </>
        )

      case 'paranoid':
        return (
          <>
            <h1 className={s.h}>Paranoid mode</h1>
            <div className={s.step}>Step 8 · Paranoid</div>
            <p className={s.lead}>
              Hardened defaults for hostile environments.
            </p>
            <label className={s.toggle}>
              <input
                type="checkbox"
                checked={form.paranoid}
                onChange={(e) => set('paranoid', e.target.checked)}
              />
              <span className={s.lbl}>Enable paranoid mode</span>
            </label>
            <span className={s.hint}>
              Tightens metadata exposure and rate limits at the cost of some convenience and
              throughput. You can revisit this in operator.yaml later.
            </span>
          </>
        )

      case 'review':
        return (
          <>
            <h1 className={s.h}>Review</h1>
            <div className={s.step}>Step 9 · Review</div>
            <p className={s.lead}>Confirm your choices. Secrets (password, master key) are not shown.</p>

            {submitErr && <div className={s.banner} role="alert">{submitErr}</div>}

            <div className={s.review}>
              <Row k="Hostname" v={form.hostname || '—'} />
              <Row k="Contact email" v={form.contact_email || '—'} />
              {form.display_name && <Row k="Display name" v={form.display_name} />}
              <Row k="Admin email" v={form.admin_email || '—'} />
              <Row k="TLS mode" v={form.tls_mode} />
              {form.tls_mode === 'static' && <Row k="Cert path" v={form.cert_path || '—'} />}
              {form.tls_mode === 'static' && <Row k="Key path" v={form.key_path || '—'} />}
              <Row k="Auth mode" v={form.auth_mode} />
              <Row k="Public uploads" v={form.public_uploads ? 'on' : 'off'} />
              {form.public_uploads && <Row k="ToS URL" v={form.tos_url || '—'} />}
              <Row k="Paranoid" v={form.paranoid ? 'on' : 'off'} />
            </div>
          </>
        )

      case 'commit':
        return (
          <>
            <h1 className={s.h}>Commit</h1>
            <div className={s.step}>Step 10 · Commit</div>
            <p className={s.lead}>
              Committing writes <span className={s.mono}>operator.yaml</span>, creates your operator
              account, and seals the generated keys with your master key. This finalizes setup.
            </p>
            {submitErr && <div className={s.banner} role="alert">{submitErr}</div>}
            <p className={s.hint}>Make sure you’ve backed up your master key — it cannot be recovered.</p>
          </>
        )

      case 'orientation':
        return (
          <>
            <h1 className={s.h}>You’re live</h1>
            <div className={s.step}>Step 11 · Orientation</div>
            <p className={s.lead}>
              Setup is complete. Sign in to the operator console at{' '}
              <a className={s.mono} href="/admin">/admin</a>.
            </p>

            <div className={s.lbl}>Embed the upload widget on any page</div>
            <pre className={s.snippet} data-testid="widget-snippet">{`<script src="/widget/nova-upload-widget.js" defer></script>
<div data-nova-upload-widget data-product="image"></div>`}</pre>

            <p className={s.hint}>
              How to upload: open a page with the snippet above (or the widget demo), then drag a file
              onto the widget or click it to choose one — uploads go straight to this node.
            </p>
            <p className={s.warn}>
              Reminder: store your master-key backup offline. If you lose it, your node’s sealed
              secrets are unrecoverable.
            </p>
          </>
        )
    }
  }
}

function Row({ k, v }: { k: string; v: string }) {
  return (
    <div className={s.reviewRow}>
      <span className={s.reviewKey}>{k}</span>
      <span className={s.reviewVal}>{v}</span>
    </div>
  )
}
