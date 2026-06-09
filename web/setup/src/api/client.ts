// Same-origin fetch wrapper for the first-run setup wizard (M13). Mirrors
// web/admin's client plumbing (ApiError + parse) but targets the /setup/* API
// endpoints the coordinator mounts only while bootstrap is pending. Every
// /setup/* API request must carry the bootstrap token (printed to the
// coordinator log) as the X-Nova-Setup-Token header (m13.1/H2); without it the
// server returns 401 setup_token_required. The static bundle (the page itself)
// does not need the token. Task 10 wires the stepper onto this client.

// ApiError carries the coordinator's structured { error: { code, message } } so
// the wizard can branch on code (e.g. invalid_answers → 400) and show message.
export class ApiError extends Error {
  constructor(
    readonly status: number,
    readonly code: string,
    message: string,
  ) {
    super(message)
    this.name = 'ApiError'
  }
}

async function parse<T>(res: Response): Promise<T> {
  if (res.status === 204) return undefined as T
  const text = await res.text()
  const body = text ? JSON.parse(text) : undefined
  if (!res.ok) {
    const err = (body && (body.error ?? body)) || {}
    throw new ApiError(res.status, err.code ?? 'error', err.message ?? res.statusText)
  }
  return body as T
}

// GET /setup/state — bootstrap status. While the setup handler is mounted this
// is always { bootstrap_complete: false }.
export interface SetupState {
  bootstrap_complete: boolean
}

// POST /setup/keys/master — generates first-run secrets in coordinator memory
// and returns the master key (hex) plus its fingerprint for operator verification.
export interface MasterKey {
  master_key_hex: string
  fingerprint: string
}

// Answers is the operator-supplied first-run configuration. The shape is kept
// loose here; Task 10's stepper defines the typed form fields and validation
// mirrors the coordinator's setup.Answers.
export type SetupAnswers = Record<string, unknown>

export interface OkResponse {
  ok: boolean
}

// Header the coordinator requires on every /setup/* API request (m13.1/H2).
export const SETUP_TOKEN_HEADER = 'X-Nova-Setup-Token'

// createSetupApi builds the typed client over a fetcher, closing over the
// bootstrap token so every /setup/* API request carries the X-Nova-Setup-Token
// header. The token is transport-only: it authenticates the request and is
// never echoed into answers/operator.yaml. The /setup/* paths are same-origin
// and only mounted pre-commit.
export function createSetupApi(token: string, fetcher: typeof fetch = fetch) {
  const get = <T>(path: string) =>
    fetcher(path, { headers: { [SETUP_TOKEN_HEADER]: token } }).then((r) => parse<T>(r))
  const send = <T>(method: string, path: string, body?: unknown) =>
    fetcher(path, {
      method,
      headers:
        body === undefined
          ? { [SETUP_TOKEN_HEADER]: token }
          : { 'content-type': 'application/json', [SETUP_TOKEN_HEADER]: token },
      body: body === undefined ? undefined : JSON.stringify(body),
    }).then((r) => parse<T>(r))

  return {
    state: () => get<SetupState>('/setup/state'),
    generateMasterKey: () => send<MasterKey>('POST', '/setup/keys/master'),
    submitAnswers: (a: SetupAnswers) => send<OkResponse>('POST', '/setup/answers', a),
    commit: (a: SetupAnswers) => send<OkResponse>('POST', '/setup/commit', a),
  }
}

export type SetupApi = ReturnType<typeof createSetupApi>
