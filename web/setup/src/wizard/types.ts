// Typed form state for the first-run wizard. Field names mirror the JSON tags
// the coordinator's setup.Answers expects (internal/setup/answers.go); the
// Review step serializes exactly these keys before POST /setup/answers.
export type TLSMode = 'dev-self-signed' | 'http-01' | 'dns-01' | 'static' | 'onion'
export type AuthMode = 'local' | 'external'

// FormState holds every operator input plus the generated master-key material
// (kept in component state only — never echoed back to the API in answers).
export interface FormState {
  // Bootstrap token (printed to the coordinator log) sent as the
  // X-Nova-Setup-Token header on every /setup/* request. Transport-only — it is
  // a credential and is never serialized into answers/operator.yaml.
  bootstrapToken: string
  hostname: string
  contact_email: string
  display_name: string
  admin_email: string
  admin_password: string
  tls_mode: TLSMode
  cert_path: string
  key_path: string
  auth_mode: AuthMode
  issuer_url: string
  client_id: string
  public_uploads: boolean
  tos_url: string
  // Privacy hardening — protective state (true = hardened). `paranoid` is
  // derived from these in toAnswers(), not stored. Note hardenPrivateDHT
  // defaults true: the DHT's safe default is already private.
  hardenNoIPRecording: boolean
  hardenShortRetention: boolean
  hardenPrivateDHT: boolean
}

export const initialForm: FormState = {
  bootstrapToken: '',
  hostname: '',
  contact_email: '',
  display_name: '',
  admin_email: '',
  admin_password: '',
  tls_mode: 'dev-self-signed',
  cert_path: '',
  key_path: '',
  auth_mode: 'local',
  issuer_url: '',
  client_id: '',
  public_uploads: false,
  tos_url: '',
  hardenNoIPRecording: false,
  hardenShortRetention: false,
  hardenPrivateDHT: true,
}

// toAnswers builds the wire payload, dropping empty optional fields so the
// coordinator's omitempty/validation sees the same shape novactl sends.
export function toAnswers(f: FormState): Record<string, unknown> {
  const a: Record<string, unknown> = {
    hostname: f.hostname.trim(),
    contact_email: f.contact_email.trim(),
    admin_email: f.admin_email.trim(),
    admin_password: f.admin_password,
    tls_mode: f.tls_mode,
    auth_mode: f.auth_mode,
    public_uploads: f.public_uploads,
    // Privacy constituents — explicit so operator.yaml is WYSIWYG. Note the
    // polarity: the toggles store the *protective* state.
    record_source_ip: !f.hardenNoIPRecording,
    source_ip_retention_days: f.hardenShortRetention ? 1 : 30,
    public_ipfs_dht: !f.hardenPrivateDHT,
    paranoid: f.hardenNoIPRecording && f.hardenShortRetention && f.hardenPrivateDHT,
  }
  const dn = f.display_name.trim()
  if (dn) a.display_name = dn
  if (f.tls_mode === 'static') {
    a.cert_path = f.cert_path.trim()
    a.key_path = f.key_path.trim()
  }
  if (f.auth_mode === 'external') {
    a.issuer_url = f.issuer_url.trim()
    a.client_id = f.client_id.trim()
  }
  if (f.public_uploads) a.tos_url = f.tos_url.trim()
  return a
}
