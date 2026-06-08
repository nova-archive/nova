# M12 Upload Widget Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship Nova's embeddable, hermetic drag-and-drop upload widget (`web/widget/`) — Uppy + tus + a Nova-aware finalize orchestrator — served behind an optional coordinator `/widget/*` seam, proven end-to-end through nginx.

**Architecture:** The widget is a framework-agnostic Vite **library-mode** IIFE bundle exposing a global `NovaUploadWidget.{mount,mountAll}`. It drives the *existing, unchanged* upload contract (`POST /api/v1/uploads` → tus `PATCH` → `POST .../finalize` → `UploadResult`). The one piece of real logic is the **transport→finalize boundary**: Uppy/tus success (`offset==length`) is only transport; the orchestrator then `POST`s `finalize` and surfaces the CID via `onComplete`. The only backend change is a feature-gated static handler (`NOVA_WIDGET_DIST_DIR`) mirroring M11's admin-SPA seam.

**Tech Stack:** TypeScript (vanilla, no React), Vite 4.5 library mode, Vitest 0.34 (jsdom), `@uppy/core`/`drag-drop`/`tus`/`status-bar` 3.x, `vite-plugin-css-injected-by-js`; Go (`net/http` static handler, chi router); testcontainers + nginx integration. Node-16-safe pins throughout (local Node 16, CI Node 20).

**Design doc:** `docs/superpowers/specs/2026-06-07-phase1-m12-upload-widget-design.md`

---

## File structure

**Created (frontend — `web/widget/`):**
- `package.json` — `@nova/widget`; Uppy 3.x deps; Vite/Vitest pins; build/lint/test scripts
- `vite.config.ts` — library mode (IIFE, global `NovaUploadWidget`, stable entry), CSS-injected, vitest block
- `tsconfig.json` — mirrors `web/admin` (ES2020, bundler, strict), no JSX
- `public/index.html` — demo/example host page (Vite copies `public/` → `dist/`, served at `/widget/`)
- `src/api/types.ts` — `UploadResult`, `WidgetError` (tracked to openapi); no runtime logic
- `src/config.ts` — `MountOptions`, `NormalizedConfig`, `normalizeOptions`, `parseElementConfig`
- `src/auth.ts` — `resolveToken`, `authHeaders` (per-request token resolution)
- `src/errors.ts` — `mapError(status, body)` → `WidgetError`
- `src/uploader.ts` — `finalizeUpload`, `wireFinalize`, `buildTusOptions`, `fileMeta` (the boundary logic)
- `src/widget.ts` — `buildUppy` (Uppy(core)+DragDrop+StatusBar glue; uses uploader)
- `src/index.ts` — `mount`, `mountAll`, `autoBootstrap`, the `WeakMap` registry (the public API)
- `src/widget.css` — brand-tokened Uppy overrides
- `src/*.test.ts` — Vitest for config/auth/errors/uploader/index

**Created (backend + tests):**
- `internal/api/handlers/widget_static.go` — `/widget/*` static (CSP, caching, 404, no SPA fallback)
- `internal/api/handlers/widget_static_test.go`
- `internal/integration/m12_upload_widget_test.go` — nginx-fronted end-to-end

**Modified:**
- `package.json` (root) — re-add `web/widget` to `workspaces`
- `package-lock.json` — regenerated
- `internal/api/server.go` — `ServerConfig.WidgetStatic` + mount `/widget` + `/widget/*`
- `pkg/coordinator/coordinator.go` — `WidgetConfig` + `Config.Widget` + build the handler
- `cmd/coordinator/main.go` — read `NOVA_WIDGET_DIST_DIR`
- `Makefile` — `widget-{build,lint,test}`, `hermetic-widget`, `web` aggregate
- `scripts/hermetic-spa.sh` — generalize wording (admin → bundle)
- `.github/workflows/ci.yml` — widget steps in the `web-admin` job
- `nginx/nova.conf.example` — `location /widget` proxy note
- `docs/ROADMAP.md`, `docs/THREAT_MODEL.md`, `docs/legal/OPERATOR_CHECKLIST.md`,
  `docs/specs/openapi.yaml`, `docs/superpowers/specs/2026-05-25-phase1-single-node-mvp-design.md`

---

## Task 1: Scaffold `web/widget` workspace + build config

**Files:**
- Create: `web/widget/package.json`, `web/widget/tsconfig.json`, `web/widget/vite.config.ts`, `web/widget/src/index.ts` (placeholder)
- Modify: `package.json` (root)
- Delete: `web/widget/.gitkeep`

- [ ] **Step 1: Re-add the workspace to the root `package.json`**

Modify `package.json` (root) — change the `workspaces` array:

```json
  "workspaces": [
    "web/admin",
    "web/widget"
  ],
```

- [ ] **Step 2: Write `web/widget/package.json`**

```json
{
  "name": "@nova/widget",
  "private": true,
  "version": "0.0.0",
  "type": "module",
  "description": "Nova embeddable upload widget (M12) — hermetic Uppy + tus, no third-party runtime assets.",
  "scripts": {
    "build": "vite build",
    "lint": "tsc --noEmit",
    "test": "vitest"
  },
  "dependencies": {
    "@uppy/core": "^3.13.0",
    "@uppy/drag-drop": "^3.1.0",
    "@uppy/status-bar": "^3.3.0",
    "@uppy/tus": "^3.5.0"
  },
  "devDependencies": {
    "jsdom": "^22.1.0",
    "typescript": "^5.3.0",
    "vite": "^4.5.0",
    "vite-plugin-css-injected-by-js": "^3.3.0",
    "vitest": "^0.34.0"
  }
}
```

- [ ] **Step 3: Write `web/widget/tsconfig.json`**

```json
{
  "compilerOptions": {
    "target": "ES2020",
    "useDefineForClassFields": true,
    "lib": ["ES2020", "DOM", "DOM.Iterable"],
    "module": "ESNext",
    "skipLibCheck": true,
    "moduleResolution": "bundler",
    "allowImportingTsExtensions": true,
    "resolveJsonModule": true,
    "isolatedModules": true,
    "noEmit": true,
    "strict": true,
    "noUnusedLocals": true,
    "noUnusedParameters": true,
    "noFallthroughCasesInSwitch": true
  },
  "include": ["src"]
}
```

- [ ] **Step 4: Write `web/widget/vite.config.ts`**

```ts
import { defineConfig } from 'vitest/config'
import cssInjectedByJsPlugin from 'vite-plugin-css-injected-by-js'

// Hermetic upload-widget build (M12). Library mode → a single IIFE bundle exposing
// the global `NovaUploadWidget`, with a STABLE entry filename so
// `<script src="/widget/nova-upload-widget.js">` is a durable embed URL operators
// can hardcode. Uppy + tus CSS and the brand overrides are injected into the JS at
// runtime (vite-plugin-css-injected-by-js, a build-time-only dep) so the whole
// integration is one <script> tag. No external CDN: deps are bundled; the
// `hermetic-widget` CI gate greps dist for external origins.
export default defineConfig({
  base: '/widget/',
  plugins: [cssInjectedByJsPlugin()],
  build: {
    outDir: 'dist',
    sourcemap: false,
    cssCodeSplit: false,
    lib: {
      entry: 'src/index.ts',
      name: 'NovaUploadWidget',
      formats: ['iife'],
      fileName: () => 'nova-upload-widget.js',
    },
  },
  test: {
    environment: 'jsdom',
    css: false,
  },
})
```

- [ ] **Step 5: Write a placeholder `web/widget/src/index.ts`** (real API lands in Task 7)

```ts
// Placeholder entry — replaced in Task 7 with the real mount/mountAll API.
export const VERSION = '0.0.0'
```

- [ ] **Step 6: Remove the placeholder keep-file**

Run: `git rm web/widget/.gitkeep`
Expected: `.gitkeep` staged for deletion.

- [ ] **Step 7: Install (regenerates the lockfile with the new workspace)**

Run: `npm install`
Expected: completes without error; `package-lock.json` now includes `web/widget` + the Uppy deps. (Uppy 3.x is Node-16-safe; if `npm install` fails on the local Node 16 host, confirm the pinned versions above, not `@uppy/*@^4`.)

- [ ] **Step 8: Verify the placeholder builds the IIFE bundle**

Run: `npm run build --workspace web/widget`
Expected: PASS; `web/widget/dist/nova-upload-widget.js` exists.

- [ ] **Step 9: Commit**

```bash
git add package.json package-lock.json web/widget/package.json web/widget/tsconfig.json web/widget/vite.config.ts web/widget/src/index.ts
git add -u web/widget
git commit -m "build(widget): scaffold web/widget workspace — Vite library mode, Uppy 3.x pins (m12)"
```

---

## Task 2: `api/types.ts` + `config.ts` (option normalization + data-* parsing)

**Files:**
- Create: `web/widget/src/api/types.ts`, `web/widget/src/config.ts`, `web/widget/src/config.test.ts`

- [ ] **Step 1: Write `web/widget/src/api/types.ts`** (types only — no test)

```ts
// Mirrors docs/specs/openapi.yaml UploadResult + Error. Kept minimal: the widget
// only consumes the upload surface.
export interface UploadResult {
  cid: string
  byte_size: number
  mime_type: string
  product: string
  urls: { original: string; json: string; presets?: Record<string, string> }
}

export interface WidgetError {
  code: string
  message?: string
}
```

- [ ] **Step 2: Write the failing test `web/widget/src/config.test.ts`**

```ts
import { describe, it, expect } from 'vitest'
import { normalizeOptions, parseElementConfig } from './config'

describe('normalizeOptions', () => {
  it('applies defaults', () => {
    const c = normalizeOptions()
    expect(c.endpoint).toBe('/api/v1/uploads')
    expect(c.product).toBe('raw')
    expect(c.collectionId).toBeUndefined()
    expect(c.chunkSize).toBe(5 * 1024 * 1024)
  })

  it('overrides endpoint/product/collection/chunk', () => {
    const c = normalizeOptions({ endpoint: '/x', product: 'image', collectionId: 'abc', chunkSize: 100 })
    expect(c.endpoint).toBe('/x')
    expect(c.product).toBe('image')
    expect(c.collectionId).toBe('abc')
    expect(c.chunkSize).toBe(100)
  })

  it('token: t becomes getToken returning t', async () => {
    const c = normalizeOptions({ token: 'jwt' })
    expect(await c.getToken()).toBe('jwt')
  })

  it('getToken wins when both token and getToken are set', async () => {
    const c = normalizeOptions({ token: 'static', getToken: () => 'dynamic' })
    expect(await c.getToken()).toBe('dynamic')
  })

  it('defaults getToken to null (public-uploads floor)', async () => {
    const c = normalizeOptions()
    expect(await c.getToken()).toBeNull()
  })
})

describe('parseElementConfig', () => {
  it('reads non-secret data-* config', () => {
    const el = document.createElement('div')
    el.setAttribute('data-endpoint', '/api/v1/uploads')
    el.setAttribute('data-product', 'image')
    el.setAttribute('data-collection', 'col-1')
    const opts = parseElementConfig(el)
    expect(opts).toEqual({ endpoint: '/api/v1/uploads', product: 'image', collectionId: 'col-1' })
  })

  it('omits unset attributes', () => {
    const el = document.createElement('div')
    expect(parseElementConfig(el)).toEqual({ endpoint: undefined, product: undefined, collectionId: undefined })
  })
})
```

- [ ] **Step 3: Run the test to verify it fails**

Run: `npm run test --workspace web/widget -- --run src/config.test.ts`
Expected: FAIL — `Failed to resolve import './config'`.

- [ ] **Step 4: Write `web/widget/src/config.ts`**

```ts
import type { UploadResult, WidgetError } from './api/types'

export interface MountOptions {
  endpoint?: string
  product?: string
  collectionId?: string
  maxFileSize?: number
  chunkSize?: number
  getToken?: () => string | null | Promise<string | null>
  token?: string
  onComplete?: (r: UploadResult) => void
  onError?: (e: WidgetError) => void
  onProgress?: (p: { bytesUploaded: number; bytesTotal: number }) => void
}

export interface NormalizedConfig {
  endpoint: string
  product: string
  collectionId?: string
  maxFileSize?: number
  chunkSize: number
  getToken: () => string | null | Promise<string | null>
  onComplete: (r: UploadResult) => void
  onError: (e: WidgetError) => void
  onProgress: (p: { bytesUploaded: number; bytesTotal: number }) => void
}

const DEFAULT_ENDPOINT = '/api/v1/uploads'
const DEFAULT_PRODUCT = 'raw'
const DEFAULT_CHUNK_SIZE = 5 * 1024 * 1024

export function normalizeOptions(opts: MountOptions = {}): NormalizedConfig {
  const tok = opts.token
  const getToken = opts.getToken ?? (tok != null ? () => tok : () => null)
  return {
    endpoint: opts.endpoint || DEFAULT_ENDPOINT,
    product: opts.product || DEFAULT_PRODUCT,
    collectionId: opts.collectionId || undefined,
    maxFileSize: opts.maxFileSize,
    chunkSize: opts.chunkSize ?? DEFAULT_CHUNK_SIZE,
    getToken,
    onComplete: opts.onComplete ?? (() => {}),
    onError: opts.onError ?? (() => {}),
    onProgress: opts.onProgress ?? (() => {}),
  }
}

// parseElementConfig extracts NON-SECRET config from a data-nova-upload-widget
// element. A bearer token is NEVER read from the DOM — it arrives only via getToken.
export function parseElementConfig(el: Element): MountOptions {
  const ds = (el as HTMLElement).dataset
  return {
    endpoint: ds.endpoint || undefined,
    product: ds.product || undefined,
    collectionId: ds.collection || undefined,
  }
}
```

- [ ] **Step 5: Run the test to verify it passes**

Run: `npm run test --workspace web/widget -- --run src/config.test.ts`
Expected: PASS (all config tests green).

- [ ] **Step 6: Commit**

```bash
git add web/widget/src/api/types.ts web/widget/src/config.ts web/widget/src/config.test.ts
git commit -m "feat(widget): option normalization + data-* parsing (m12)"
```

---

## Task 3: `auth.ts` (per-request token resolution)

**Files:**
- Create: `web/widget/src/auth.ts`, `web/widget/src/auth.test.ts`

- [ ] **Step 1: Write the failing test `web/widget/src/auth.test.ts`**

```ts
import { describe, it, expect, vi } from 'vitest'
import { resolveToken, authHeaders } from './auth'

describe('resolveToken', () => {
  it('returns the token from getToken', async () => {
    expect(await resolveToken({ getToken: () => 'jwt' })).toBe('jwt')
  })

  it('awaits an async getToken', async () => {
    expect(await resolveToken({ getToken: async () => 'async-jwt' })).toBe('async-jwt')
  })

  it('treats empty/null as no token', async () => {
    expect(await resolveToken({ getToken: () => '' })).toBeNull()
    expect(await resolveToken({ getToken: () => null })).toBeNull()
  })

  it('is called per invocation — honors a rotated token', async () => {
    const getToken = vi.fn().mockReturnValueOnce('t1').mockReturnValueOnce('t2')
    expect(await resolveToken({ getToken })).toBe('t1')
    expect(await resolveToken({ getToken })).toBe('t2')
    expect(getToken).toHaveBeenCalledTimes(2)
  })
})

describe('authHeaders', () => {
  it('builds a Bearer header for a token', () => {
    expect(authHeaders('jwt')).toEqual({ Authorization: 'Bearer jwt' })
  })
  it('returns no header for a null token', () => {
    expect(authHeaders(null)).toEqual({})
  })
})
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `npm run test --workspace web/widget -- --run src/auth.test.ts`
Expected: FAIL — `Failed to resolve import './auth'`.

- [ ] **Step 3: Write `web/widget/src/auth.ts`**

```ts
import type { NormalizedConfig } from './config'

// resolveToken awaits the caller's getToken PER REQUEST so a long resumable upload
// survives the 15-minute access-token expiry; an empty/null result means no
// Authorization header (the public-uploads floor).
export async function resolveToken(cfg: Pick<NormalizedConfig, 'getToken'>): Promise<string | null> {
  const tok = await cfg.getToken()
  return tok && tok.length > 0 ? tok : null
}

export function authHeaders(token: string | null): Record<string, string> {
  return token ? { Authorization: `Bearer ${token}` } : {}
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `npm run test --workspace web/widget -- --run src/auth.test.ts`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add web/widget/src/auth.ts web/widget/src/auth.test.ts
git commit -m "feat(widget): per-request token resolution (getToken) (m12)"
```

---

## Task 4: `errors.ts` (finalize/commit error mapping)

**Files:**
- Create: `web/widget/src/errors.ts`, `web/widget/src/errors.test.ts`

- [ ] **Step 1: Write the failing test `web/widget/src/errors.test.ts`**

```ts
import { describe, it, expect } from 'vitest'
import { mapError } from './errors'

describe('mapError', () => {
  it('prefers the server error code when present', () => {
    expect(mapError(400, { code: 'mime_rejected', message: 'bad' })).toEqual({ code: 'mime_rejected', message: 'bad' })
  })

  it('maps known statuses when no code is present', () => {
    expect(mapError(413, {}).code).toBe('payload_too_large')
    expect(mapError(401, {}).code).toBe('unauthenticated')
    expect(mapError(409, {}).code).toBe('upload_incomplete')
    expect(mapError(422, {}).code).toBe('moderation_rejected')
    expect(mapError(451, {}).code).toBe('blocklisted')
    expect(mapError(503, {}).code).toBe('server_busy')
  })

  it('falls back to upload_failed for an unmapped status', () => {
    expect(mapError(418, {}).code).toBe('upload_failed')
  })
})
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `npm run test --workspace web/widget -- --run src/errors.test.ts`
Expected: FAIL — `Failed to resolve import './errors'`.

- [ ] **Step 3: Write `web/widget/src/errors.ts`**

```ts
import type { WidgetError } from './api/types'

// mapError turns a finalize/commit HTTP failure into a stable WidgetError. The
// server's snake_case `code` (from internal/api/handlers/upload.go writePutError)
// is authoritative; the status table is the fallback when the body is empty.
export function mapError(status: number, body: { code?: string; message?: string } = {}): WidgetError {
  if (body.code) return { code: body.code, message: body.message }
  const byStatus: Record<number, string> = {
    400: 'bad_request',
    401: 'unauthenticated',
    403: 'forbidden',
    404: 'not_found',
    409: 'upload_incomplete',
    413: 'payload_too_large',
    415: 'unsupported_media_type',
    422: 'moderation_rejected',
    451: 'blocklisted',
    503: 'server_busy',
  }
  return { code: byStatus[status] ?? 'upload_failed', message: body.message }
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `npm run test --workspace web/widget -- --run src/errors.test.ts`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add web/widget/src/errors.ts web/widget/src/errors.test.ts
git commit -m "feat(widget): finalize error → WidgetError mapping (m12)"
```

---

## Task 5: `uploader.ts` (the transport→finalize boundary)

**Files:**
- Create: `web/widget/src/uploader.ts`, `web/widget/src/uploader.test.ts`

This is the milestone's load-bearing logic: `finalizeUpload` (the Nova commit after tus success), `wireFinalize` (bridges Uppy's `upload-success` to finalize), `buildTusOptions` (per-request auth + metadata fields), and `fileMeta` (the Upload-Metadata mapping).

- [ ] **Step 1: Write the failing test `web/widget/src/uploader.test.ts`**

```ts
import { describe, it, expect, vi } from 'vitest'
import { finalizeUpload, wireFinalize, buildTusOptions, fileMeta } from './uploader'
import { normalizeOptions } from './config'

const okResult = { cid: 'bafy123', byte_size: 5, mime_type: 'text/plain', product: 'raw', urls: { original: '/blob/bafy123', json: '/blob/bafy123.json' } }

function jsonResponse(status: number, body: unknown) {
  return { ok: status >= 200 && status < 300, status, json: async () => body } as Response
}

describe('finalizeUpload', () => {
  it('POSTs {uploadURL}/finalize with the bearer and returns the UploadResult', async () => {
    const fetch = vi.fn().mockResolvedValue(jsonResponse(200, okResult))
    const cfg = normalizeOptions({ token: 'jwt' })
    const res = await finalizeUpload('http://h/api/v1/uploads/abc', cfg, { fetch })
    expect(res.cid).toBe('bafy123')
    expect(fetch).toHaveBeenCalledWith('http://h/api/v1/uploads/abc/finalize', {
      method: 'POST',
      headers: { Authorization: 'Bearer jwt' },
    })
  })

  it('sends no Authorization header when getToken yields null', async () => {
    const fetch = vi.fn().mockResolvedValue(jsonResponse(200, okResult))
    await finalizeUpload('http://h/api/v1/uploads/abc', normalizeOptions(), { fetch })
    expect(fetch).toHaveBeenCalledWith('http://h/api/v1/uploads/abc/finalize', { method: 'POST', headers: {} })
  })

  it('throws a mapped WidgetError on a non-2xx finalize', async () => {
    const fetch = vi.fn().mockResolvedValue(jsonResponse(422, { code: 'moderation_rejected' }))
    await expect(finalizeUpload('http://h/api/v1/uploads/abc', normalizeOptions(), { fetch }))
      .rejects.toEqual({ code: 'moderation_rejected', message: undefined })
  })
})

describe('wireFinalize', () => {
  function fakeEmitter() {
    let cb: ((file: unknown, resp: { uploadURL: string }) => unknown) | undefined
    return {
      on(_event: string, fn: (file: unknown, resp: { uploadURL: string }) => unknown) { cb = fn },
      emit(resp: { uploadURL: string }) { return cb!({}, resp) },
    }
  }

  it('finalizes on upload-success and calls onComplete (never on transport alone)', async () => {
    const fetch = vi.fn().mockResolvedValue(jsonResponse(200, okResult))
    const onComplete = vi.fn()
    const cfg = normalizeOptions({ onComplete })
    const em = fakeEmitter()
    wireFinalize(em, cfg, { fetch })
    await em.emit({ uploadURL: 'http://h/api/v1/uploads/abc' })
    expect(onComplete).toHaveBeenCalledWith(okResult)
  })

  it('routes a finalize failure to onError', async () => {
    const fetch = vi.fn().mockResolvedValue(jsonResponse(413, {}))
    const onError = vi.fn()
    const cfg = normalizeOptions({ onError })
    const em = fakeEmitter()
    wireFinalize(em, cfg, { fetch })
    await em.emit({ uploadURL: 'http://h/api/v1/uploads/abc' })
    expect(onError).toHaveBeenCalledWith({ code: 'payload_too_large', message: undefined })
  })
})

describe('buildTusOptions', () => {
  it('sets endpoint, chunkSize and the allowed metadata fields', () => {
    const o = buildTusOptions(normalizeOptions({ endpoint: '/api/v1/uploads', chunkSize: 99 }))
    expect(o.endpoint).toBe('/api/v1/uploads')
    expect(o.chunkSize).toBe(99)
    expect(o.allowedMetaFields).toEqual(['filename', 'mime_type', 'product', 'collection_id'])
  })

  it('onBeforeRequest sets Authorization per request, omits it when token is null', async () => {
    const withTok = buildTusOptions(normalizeOptions({ token: 'jwt' }))
    const set = vi.fn()
    await withTok.onBeforeRequest({ setHeader: set })
    expect(set).toHaveBeenCalledWith('Authorization', 'Bearer jwt')

    const noTok = buildTusOptions(normalizeOptions())
    const set2 = vi.fn()
    await noTok.onBeforeRequest({ setHeader: set2 })
    expect(set2).not.toHaveBeenCalled()
  })
})

describe('fileMeta', () => {
  it('maps filename/mime/product and includes collection only when set', () => {
    expect(fileMeta(normalizeOptions({ product: 'image' }), { name: 'a.jpg', type: 'image/jpeg' }))
      .toEqual({ filename: 'a.jpg', mime_type: 'image/jpeg', product: 'image' })
    expect(fileMeta(normalizeOptions({ product: 'image', collectionId: 'c1' }), { name: 'a.jpg', type: 'image/jpeg' }))
      .toEqual({ filename: 'a.jpg', mime_type: 'image/jpeg', product: 'image', collection_id: 'c1' })
  })
})
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `npm run test --workspace web/widget -- --run src/uploader.test.ts`
Expected: FAIL — `Failed to resolve import './uploader'`.

- [ ] **Step 3: Write `web/widget/src/uploader.ts`**

```ts
import type { NormalizedConfig } from './config'
import type { UploadResult } from './api/types'
import { resolveToken, authHeaders } from './auth'
import { mapError } from './errors'

export interface UploaderDeps {
  fetch: typeof fetch
}

// finalizeUpload performs the Nova-specific commit after tus transport success.
// uploadURL is the tus session URL (the tus Location, resolved absolute by the
// client). Only finalize yields a CID — tus "success" is transport only.
export async function finalizeUpload(
  uploadURL: string,
  cfg: NormalizedConfig,
  deps: UploaderDeps,
): Promise<UploadResult> {
  const token = await resolveToken(cfg)
  const resp = await deps.fetch(`${uploadURL}/finalize`, { method: 'POST', headers: authHeaders(token) })
  if (!resp.ok) {
    const body = await resp.json().catch(() => ({}))
    throw mapError(resp.status, body)
  }
  return (await resp.json()) as UploadResult
}

// A minimal emitter shape (satisfied by Uppy) so wireFinalize is testable without
// constructing a real Uppy/DragDrop instance.
export interface UploadSuccessEmitter {
  on(event: 'upload-success', cb: (file: unknown, resp: { uploadURL: string }) => void): void
}

// wireFinalize bridges transport-success → Nova finalize → onComplete/onError.
export function wireFinalize(emitter: UploadSuccessEmitter, cfg: NormalizedConfig, deps: UploaderDeps): void {
  emitter.on('upload-success', async (_file, resp) => {
    try {
      cfg.onComplete(await finalizeUpload(resp.uploadURL, cfg, deps))
    } catch (e) {
      cfg.onError(e as { code: string; message?: string })
    }
  })
}

export interface TusRequest {
  setHeader(name: string, value: string): void
}

// buildTusOptions returns the @uppy/tus options: endpoint, chunkSize, the metadata
// fields Nova reads, and per-request auth via the tus onBeforeRequest hook.
export function buildTusOptions(cfg: NormalizedConfig) {
  return {
    endpoint: cfg.endpoint,
    chunkSize: cfg.chunkSize,
    allowedMetaFields: ['filename', 'mime_type', 'product', 'collection_id'],
    async onBeforeRequest(req: TusRequest) {
      const token = await resolveToken(cfg)
      if (token) req.setHeader('Authorization', `Bearer ${token}`)
    },
  }
}

// fileMeta builds the tus Upload-Metadata for a file (collection_id only when set).
export function fileMeta(cfg: NormalizedConfig, file: { name: string; type: string }): Record<string, string> {
  return {
    filename: file.name,
    mime_type: file.type,
    product: cfg.product,
    ...(cfg.collectionId ? { collection_id: cfg.collectionId } : {}),
  }
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `npm run test --workspace web/widget -- --run src/uploader.test.ts`
Expected: PASS (all uploader tests green — the boundary is covered).

- [ ] **Step 5: Commit**

```bash
git add web/widget/src/uploader.ts web/widget/src/uploader.test.ts
git commit -m "feat(widget): transport→finalize orchestrator + tus options (m12)"
```

---

## Task 6: `widget.ts` (Uppy glue)

**Files:**
- Create: `web/widget/src/widget.ts`

`widget.ts` is thin glue over Uppy — its only logic (`fileMeta`, `buildTusOptions`, `wireFinalize`) is already unit-tested in Task 5, so this task is gated by `tsc` (type-check) rather than a unit test (constructing the real Uppy DragDrop UI is the integration test's job, not a unit test's).

- [ ] **Step 1: Write `web/widget/src/widget.ts`**

```ts
import Uppy from '@uppy/core'
import DragDrop from '@uppy/drag-drop'
import StatusBar from '@uppy/status-bar'
import Tus from '@uppy/tus'
import '@uppy/core/dist/style.css'
import '@uppy/drag-drop/dist/style.css'
import '@uppy/status-bar/dist/style.css'
import './widget.css'
import type { NormalizedConfig } from './config'
import { buildTusOptions, wireFinalize, fileMeta, type UploadSuccessEmitter } from './uploader'

export interface WidgetInstance {
  close(): void
}

// buildUppy constructs one Uppy instance bound to `target`, wired for Nova: tus
// transport with per-request auth, Upload-Metadata on file-added, progress
// forwarding, and the finalize bridge. autoProceed uploads on drop.
export function buildUppy(target: Element, cfg: NormalizedConfig): WidgetInstance {
  const uppy = new Uppy({
    autoProceed: true,
    restrictions: cfg.maxFileSize ? { maxFileSize: cfg.maxFileSize } : undefined,
  })
    .use(DragDrop, { target: target as HTMLElement })
    .use(StatusBar, { target: target as HTMLElement })
    .use(Tus, buildTusOptions(cfg))

  uppy.on('file-added', (file) => {
    uppy.setFileMeta(file.id, fileMeta(cfg, { name: file.name ?? '', type: file.type ?? '' }))
  })

  uppy.on('upload-progress', (_file, progress) => {
    if (progress) cfg.onProgress({ bytesUploaded: progress.bytesUploaded, bytesTotal: progress.bytesTotal })
  })

  wireFinalize(uppy as unknown as UploadSuccessEmitter, cfg, { fetch: globalThis.fetch.bind(globalThis) })

  return { close: () => uppy.close() }
}
```

- [ ] **Step 2: Type-check**

Run: `npm run lint --workspace web/widget`
Expected: PASS (no type errors). If `@uppy/*` type imports resolve and `setFileMeta`/`close`/`on` type-check, the glue is correct.

- [ ] **Step 3: Commit**

```bash
git add web/widget/src/widget.ts
git commit -m "feat(widget): Uppy(core)+DragDrop+StatusBar instance glue (m12)"
```

---

## Task 7: `index.ts` (public API — mount/mountAll/auto-bootstrap)

**Files:**
- Modify: `web/widget/src/index.ts` (replace the Task-1 placeholder)
- Create: `web/widget/src/index.test.ts`

- [ ] **Step 1: Write the failing test `web/widget/src/index.test.ts`**

The test mocks `./widget` so it never constructs real Uppy — it asserts the registry, mount/mountAll, and bootstrap selectivity.

```ts
import { describe, it, expect, vi, beforeEach } from 'vitest'

const close = vi.fn()
const buildUppy = vi.fn(() => ({ close }))
vi.mock('./widget', () => ({ buildUppy }))

import { mount, mountAll } from './index'

beforeEach(() => {
  document.body.innerHTML = ''
  buildUppy.mockClear()
  close.mockClear()
})

describe('mount', () => {
  it('builds one instance and returns a handle', () => {
    const el = document.createElement('div')
    document.body.appendChild(el)
    const handle = mount(el)
    expect(buildUppy).toHaveBeenCalledTimes(1)
    expect(typeof handle.destroy).toBe('function')
  })

  it('resolves a string selector', () => {
    document.body.innerHTML = '<div id="up"></div>'
    mount('#up')
    expect(buildUppy).toHaveBeenCalledTimes(1)
  })

  it('throws when the target is missing', () => {
    expect(() => mount('#nope')).toThrow(/target not found/)
  })

  it('double-mounting the same element returns the existing handle (no second instance)', () => {
    const el = document.createElement('div')
    document.body.appendChild(el)
    const h1 = mount(el)
    const h2 = mount(el)
    expect(h1).toBe(h2)
    expect(buildUppy).toHaveBeenCalledTimes(1)
  })

  it('destroy() closes the instance and allows a remount', () => {
    const el = document.createElement('div')
    document.body.appendChild(el)
    mount(el).destroy()
    expect(close).toHaveBeenCalledTimes(1)
    mount(el)
    expect(buildUppy).toHaveBeenCalledTimes(2)
  })
})

describe('mountAll', () => {
  it('mounts only [data-nova-upload-widget] elements', () => {
    document.body.innerHTML =
      '<div data-nova-upload-widget data-product="image"></div><div></div><div data-nova-upload-widget></div>'
    const handles = mountAll(document)
    expect(handles).toHaveLength(2)
    expect(buildUppy).toHaveBeenCalledTimes(2)
  })

  it('passes parsed data-* config through to buildUppy', () => {
    document.body.innerHTML = '<div data-nova-upload-widget data-product="image" data-collection="c1"></div>'
    mountAll(document)
    const cfg = buildUppy.mock.calls[0][1] as { product: string; collectionId?: string }
    expect(cfg.product).toBe('image')
    expect(cfg.collectionId).toBe('c1')
  })
})
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `npm run test --workspace web/widget -- --run src/index.test.ts`
Expected: FAIL — `mount`/`mountAll` not exported (the placeholder only exports `VERSION`).

- [ ] **Step 3: Replace `web/widget/src/index.ts` with the real API**

```ts
import { normalizeOptions, parseElementConfig, type MountOptions } from './config'
import { buildUppy, type WidgetInstance } from './widget'

export interface NovaUploadWidgetHandle {
  destroy(): void
}

// One isolated instance per host element. WeakMap so a detached element is GC'd
// without a manual deregister, and a double-mount returns the existing handle.
const registry = new WeakMap<Element, NovaUploadWidgetHandle>()

function resolveTarget(target: string | Element): Element {
  const el = typeof target === 'string' ? document.querySelector(target) : target
  if (!el) throw new Error(`NovaUploadWidget: target not found: ${String(target)}`)
  return el
}

export function mount(target: string | Element, options: MountOptions = {}): NovaUploadWidgetHandle {
  const el = resolveTarget(target)
  const existing = registry.get(el)
  if (existing) return existing

  const cfg = normalizeOptions(options)
  const instance: WidgetInstance = buildUppy(el, cfg)
  const handle: NovaUploadWidgetHandle = {
    destroy() {
      instance.close()
      registry.delete(el)
    },
  }
  registry.set(el, handle)
  return handle
}

// mountAll mounts ONLY elements explicitly marked data-nova-upload-widget, reading
// non-secret config from data-* attributes. No whole-page scan; no token in the DOM.
export function mountAll(root: ParentNode = document): NovaUploadWidgetHandle[] {
  return Array.from(root.querySelectorAll('[data-nova-upload-widget]')).map((el) =>
    mount(el, parseElementConfig(el)),
  )
}

// autoBootstrap wires marked elements once the DOM is ready. Errors are swallowed so
// a malformed embed can never break the host page's load.
export function autoBootstrap(): void {
  if (typeof document === 'undefined') return
  const run = () => {
    try {
      mountAll(document)
    } catch {
      /* never break host page load */
    }
  }
  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', run, { once: true })
  } else {
    run()
  }
}

autoBootstrap()
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `npm run test --workspace web/widget -- --run src/index.test.ts`
Expected: PASS.

- [ ] **Step 5: Run the full widget suite + type-check**

Run: `npm run test --workspace web/widget -- --run && npm run lint --workspace web/widget`
Expected: PASS (config, auth, errors, uploader, index all green; no type errors).

- [ ] **Step 6: Commit**

```bash
git add web/widget/src/index.ts web/widget/src/index.test.ts
git commit -m "feat(widget): mount/mountAll + auto-bootstrap + WeakMap registry (m12)"
```

---

## Task 8: Brand styles + demo page; full hermetic build

**Files:**
- Create: `web/widget/src/widget.css`, `web/widget/public/index.html`

- [ ] **Step 1: Write `web/widget/src/widget.css`** (brand-tokened Uppy overrides)

```css
/* Brand-tokened overrides for the Uppy DragDrop + StatusBar surface. Nova palette
   from docs/design/Nova Brand _standalone_.html. Local-only: no @font-face URLs,
   no external url() — the hermetic-widget gate fails on any external origin. */
.nova-upload-widget {
  --nova-paper: #ECE8DF;
  --nova-card: #F4F1E8;
  --nova-accent: #E2502B;
  --nova-ink: #1A1A1A;
  font-family: ui-sans-serif, system-ui, -apple-system, Segoe UI, Roboto, sans-serif;
}

.nova-upload-widget .uppy-DragDrop-container {
  background: var(--nova-card);
  border-radius: 8px;
  border: 1px dashed color-mix(in srgb, var(--nova-ink) 25%, transparent);
}

.nova-upload-widget .uppy-DragDrop-container:focus,
.nova-upload-widget .uppy-DragDrop--isDragDropSupported.is-dragover {
  border-color: var(--nova-accent);
}

.nova-upload-widget .uppy-StatusBar-actionBtn--upload {
  background-color: var(--nova-accent);
}
```

(Apply the `nova-upload-widget` class to the host element in `buildUppy` — add `target.classList.add('nova-upload-widget')` as the first line of `buildUppy` in `web/widget/src/widget.ts`, then re-run `npm run lint --workspace web/widget` to confirm it still type-checks.)

- [ ] **Step 2: Add the class to the host element**

Modify `web/widget/src/widget.ts` — first line inside `buildUppy`:

```ts
export function buildUppy(target: Element, cfg: NormalizedConfig): WidgetInstance {
  target.classList.add('nova-upload-widget')
  const uppy = new Uppy({
```

- [ ] **Step 3: Write `web/widget/public/index.html`** (demo — Vite copies `public/` → `dist/`)

```html
<!doctype html>
<html lang="en">
  <head>
    <meta charset="utf-8" />
    <meta name="viewport" content="width=device-width, initial-scale=1" />
    <title>Nova upload widget — demo</title>
    <style>
      body { font-family: ui-sans-serif, system-ui, sans-serif; max-width: 640px; margin: 3rem auto; }
      #result img { max-width: 100%; border-radius: 8px; }
      code { background: #ECE8DF; padding: 0 .25rem; }
    </style>
  </head>
  <body>
    <h1>Nova upload widget</h1>
    <p>Drop a file below. On finalize, the returned CID renders from <code>/blob/&lt;cid&gt;</code>.</p>

    <!-- zero-JS auto-bootstrap; works under the public-uploads floor -->
    <div data-nova-upload-widget data-product="raw"></div>

    <!-- advanced mount with a result callback -->
    <div id="up"></div>
    <div id="result"></div>

    <script src="/widget/nova-upload-widget.js"></script>
    <script>
      NovaUploadWidget.mount('#up', {
        product: 'raw',
        onComplete: function (r) {
          document.getElementById('result').innerHTML =
            '<p>CID: <code>' + r.cid + '</code></p><img src="' + r.urls.original + '" alt="upload" />';
        },
        onError: function (e) {
          document.getElementById('result').textContent = 'Upload failed: ' + e.code;
        },
      });
    </script>
  </body>
</html>
```

- [ ] **Step 4: Build the full bundle**

Run: `npm run build --workspace web/widget`
Expected: PASS; `web/widget/dist/nova-upload-widget.js` and `web/widget/dist/index.html` both exist (Vite copied `public/`).

- [ ] **Step 5: Verify hermeticity (no external origins) — this gate is added in Task 9, run the script directly here**

Run: `./scripts/hermetic-spa.sh web/widget/dist`
Expected: `hermetic-spa: clean (web/widget/dist)` (no `http(s)://` origins in the bundle's HTML/CSS). If it fails, an Uppy CSS `url()` or a demo-page external reference leaked — remove it.

- [ ] **Step 6: Commit**

```bash
git add web/widget/src/widget.css web/widget/src/widget.ts web/widget/public/index.html
git commit -m "feat(widget): brand-tokened styles + hermetic demo page (m12)"
```

---

## Task 9: Makefile targets, hermetic-spa generalization, CI lane

**Files:**
- Modify: `Makefile`, `scripts/hermetic-spa.sh`, `.github/workflows/ci.yml`

- [ ] **Step 1: Add widget targets to the `Makefile`** (after the `admin:` aggregate line, ~line 94)

```makefile
.PHONY: widget widget-build widget-lint widget-test hermetic-widget web

# M12 Upload Widget (web/widget). Hermetic Uppy + tus; no third-party runtime assets.
widget-build:
	npm run build --workspace web/widget

widget-lint:
	npm run lint --workspace web/widget

widget-test:
	npm run test --workspace web/widget -- --run

# hermetic-widget fails the build if the widget bundle declares any third-party asset load.
hermetic-widget:
	./scripts/hermetic-spa.sh web/widget/dist

widget: admin-install widget-lint widget-test widget-build hermetic-widget

# web builds + checks both web workspaces (npm ci installs all workspaces).
web: admin widget
```

- [ ] **Step 2: Generalize `scripts/hermetic-spa.sh` wording (admin → bundle)**

The script is already dir-parametrized; only the human-facing strings say "admin". Update them so the message reads correctly for any bundle dir. Change these three lines:

- The header comment line `# asset load. Nova's threat model requires the admin SPA to make no third-party` → `# asset load. Nova's threat model requires web bundles to make no third-party`
- `echo "hermetic-spa: '$dist' not found — build the admin SPA first" >&2` → `echo "hermetic-spa: '$dist' not found — build the bundle first" >&2`
- `echo "hermetic-spa: external origin(s) in admin bundle HTML/CSS:" >&2` → `echo "hermetic-spa: external origin(s) in bundle HTML/CSS:" >&2`

- [ ] **Step 3: Add widget steps to the `web-admin` CI job** (`.github/workflows/ci.yml`, after the `Hermetic bundle audit` step that runs `make hermetic-spa`)

```yaml
      - name: Type-check (widget)
        run: make widget-lint

      - name: Unit tests (widget)
        run: make widget-test

      - name: Build (widget)
        run: make widget-build

      - name: Hermetic widget bundle audit
        run: make hermetic-widget
```

(`npm ci` earlier in the job already installs the `web/widget` workspace, so no extra install step is needed.)

- [ ] **Step 4: Verify the targets run end-to-end**

Run: `make widget-lint && make widget-test && make widget-build && make hermetic-widget`
Expected: type-check clean, all Vitest specs pass, build emits `dist/`, hermetic gate prints `clean`.

- [ ] **Step 5: Commit**

```bash
git add Makefile scripts/hermetic-spa.sh .github/workflows/ci.yml
git commit -m "build(widget): Makefile targets + hermetic-widget gate + CI lane (m12)"
```

---

## Task 10: Coordinator static handler `widget_static.go`

**Files:**
- Create: `internal/api/handlers/widget_static.go`, `internal/api/handlers/widget_static_test.go`

- [ ] **Step 1: Write the failing test `internal/api/handlers/widget_static_test.go`**

```go
package handlers

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func writeWidgetDist(t *testing.T) string {
	t.Helper()
	dist := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dist, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	must := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dist, filepath.FromSlash(name)), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	must("index.html", "<!doctype html><title>nova upload widget</title>")
	must("nova-upload-widget.js", "/* iife */")
	must("assets/uppy-cafebabe.css", ".uppy{}")
	return dist
}

func serve(t *testing.T, h *WidgetStaticHandler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	h.Serve(rr, httptest.NewRequest(http.MethodGet, path, nil))
	return rr
}

func TestNewWidgetStaticNilWhenUnset(t *testing.T) {
	if NewWidgetStatic("") != nil {
		t.Fatal("NewWidgetStatic(\"\") must be nil so /widget/* stays unmounted")
	}
}

func TestWidgetStaticServesDemoIndex(t *testing.T) {
	h := NewWidgetStatic(writeWidgetDist(t))
	rr := serve(t, h, "/widget/")
	if rr.Code != http.StatusOK {
		t.Fatalf("got %d", rr.Code)
	}
	if got := rr.Body.String(); got == "" || got[:9] != "<!doctype" {
		t.Fatalf("expected demo index.html, got %q", got)
	}
	if csp := rr.Header().Get("Content-Security-Policy"); csp == "" {
		t.Fatal("missing CSP")
	}
	if cc := rr.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("demo index Cache-Control = %q, want no-store", cc)
	}
}

func TestWidgetStaticServesEntryJSNoCache(t *testing.T) {
	h := NewWidgetStatic(writeWidgetDist(t))
	rr := serve(t, h, "/widget/nova-upload-widget.js")
	if rr.Code != http.StatusOK {
		t.Fatalf("got %d", rr.Code)
	}
	if cc := rr.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Fatalf("entry JS Cache-Control = %q, want no-cache (stable filename)", cc)
	}
}

func TestWidgetStaticHashedAssetImmutable(t *testing.T) {
	h := NewWidgetStatic(writeWidgetDist(t))
	rr := serve(t, h, "/widget/assets/uppy-cafebabe.css")
	if rr.Code != http.StatusOK {
		t.Fatalf("got %d", rr.Code)
	}
	if cc := rr.Header().Get("Cache-Control"); cc != "public, max-age=31536000, immutable" {
		t.Fatalf("hashed asset Cache-Control = %q", cc)
	}
}

func TestWidgetStaticUnknownPath404NoSPAFallback(t *testing.T) {
	h := NewWidgetStatic(writeWidgetDist(t))
	if rr := serve(t, h, "/widget/does-not-exist.js"); rr.Code != http.StatusNotFound {
		t.Fatalf("unknown path = %d, want 404 (no SPA fallback)", rr.Code)
	}
}

func TestWidgetStaticTraversalBlocked(t *testing.T) {
	h := NewWidgetStatic(writeWidgetDist(t))
	if rr := serve(t, h, "/widget/../../etc/passwd"); rr.Code != http.StatusNotFound {
		t.Fatalf("traversal = %d, want 404", rr.Code)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/api/handlers/ -run TestWidgetStatic -v`
Expected: FAIL — `undefined: NewWidgetStatic` / `WidgetStaticHandler`.

- [ ] **Step 3: Write `internal/api/handlers/widget_static.go`**

```go
package handlers

import (
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// buildWidgetCSP builds the strict CSP for the hermetic widget bundle + demo page:
// no third-party origins. blob: in img-src covers Uppy's object-URL previews;
// 'unsafe-inline' in style-src covers the runtime-injected CSS; scripts are
// first-party only. Matches nginx/nova.conf.example.
func buildWidgetCSP() string {
	return "default-src 'self'; img-src 'self' data: blob:; style-src 'self' 'unsafe-inline'; " +
		"font-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'"
}

// WidgetStaticHandler serves the built upload-widget bundle + demo page from a
// directory at /widget/* (M12). Content-hashed assets under /assets/ are
// immutable-cached; the stable entry JS is no-cache; the demo index.html is
// no-store. Unlike the admin SPA there is NO SPA fallback — an unknown path is a
// plain 404 (the widget is a script + demo page, not a routed app). Built only
// when NOVA_WIDGET_DIST_DIR is set — NewWidgetStatic("") returns nil so /widget/*
// is left unmounted (the feature-gate posture). Production may serve the bundle
// directly from nginx (M13); this is the self-contained, testable path.
type WidgetStaticHandler struct {
	dist  string
	index string
	csp   string
}

// NewWidgetStatic returns a handler serving dist, or nil when dist is empty.
func NewWidgetStatic(dist string) *WidgetStaticHandler {
	if dist == "" {
		return nil
	}
	return &WidgetStaticHandler{
		dist:  dist,
		index: filepath.Join(dist, "index.html"),
		csp:   buildWidgetCSP(),
	}
}

// Serve resolves the request path to a file under dist. "/widget" and "/widget/"
// serve the demo index.html; other paths serve the named file or 404 (no SPA
// fallback). Path traversal cannot escape dist: the request path is rooted with a
// leading slash before path.Clean, so any ".." collapses to the root.
func (h *WidgetStaticHandler) Serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Security-Policy", h.csp)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "same-origin")

	rel := strings.TrimPrefix(r.URL.Path, "/widget")
	clean := path.Clean("/" + strings.TrimPrefix(rel, "/")) // leading slash collapses any ".."

	if clean == "/" {
		w.Header().Set("Cache-Control", "no-store")
		http.ServeFile(w, r, h.index)
		return
	}

	fsPath := filepath.Join(h.dist, filepath.FromSlash(clean))
	info, err := os.Stat(fsPath)
	if err != nil || info.IsDir() {
		http.NotFound(w, r)
		return
	}
	if strings.HasPrefix(clean, "/assets/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
	http.ServeFile(w, r, fsPath)
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/api/handlers/ -run TestWidgetStatic -v`
Expected: PASS (all widget_static cases green).

- [ ] **Step 5: gofmt the new file**

Run: `gofmt -w internal/api/handlers/widget_static.go internal/api/handlers/widget_static_test.go`
Expected: no diff after (already formatted).

- [ ] **Step 6: Commit**

```bash
git add internal/api/handlers/widget_static.go internal/api/handlers/widget_static_test.go
git commit -m "feat(api): /widget/* static handler — CSP, caching, 404 (no SPA fallback) (m12)"
```

---

## Task 11: Wire the seam (server / coordinator / main)

**Files:**
- Modify: `internal/api/server.go`, `pkg/coordinator/coordinator.go`, `cmd/coordinator/main.go`

- [ ] **Step 1: Add the `ServerConfig` field** (`internal/api/server.go`, after the `AdminSPA` field ~line 60)

```go
	AdminSPA        *handlers.AdminSPAHandler        // nil ⇒ /admin/* static unmounted
	WidgetStatic    *handlers.WidgetStaticHandler    // nil ⇒ /widget/* static unmounted
```

- [ ] **Step 2: Mount the routes** (`internal/api/server.go`, immediately after the `if cfg.AdminSPA != nil { ... }` block ~line 109)

```go
	// Upload widget static assets (M12), served from NOVA_WIDGET_DIST_DIR at
	// /widget/*. A distinct prefix from /api/v1/uploads; nil ⇒ unmounted. The
	// handler applies its own strict CSP; no SPA fallback (404 on unknown paths).
	if cfg.WidgetStatic != nil {
		r.Handle("/widget", http.HandlerFunc(cfg.WidgetStatic.Serve))
		r.Handle("/widget/*", http.HandlerFunc(cfg.WidgetStatic.Serve))
	}
```

- [ ] **Step 3: Add the `WidgetConfig` struct + `Config.Widget` field** (`pkg/coordinator/coordinator.go`)

After the `AdminSPA AdminSPAConfig` field on `Config` (~line 122):

```go
	// AdminSPA configures coordinator-served admin SPA static assets (M11). An
	// empty DistDir leaves /admin/* unmounted (the feature-gate posture).
	AdminSPA AdminSPAConfig

	// Widget configures coordinator-served upload-widget static assets (M12). An
	// empty DistDir leaves /widget/* unmounted (the feature-gate posture).
	Widget WidgetConfig
```

After the `AdminSPAConfig` struct definition (~line 173):

```go
// WidgetConfig configures coordinator-served upload-widget static assets (M12).
type WidgetConfig struct {
	DistDir string // NOVA_WIDGET_DIST_DIR; empty ⇒ /widget/* unmounted
}
```

- [ ] **Step 4: Build the handler** (`pkg/coordinator/coordinator.go`, immediately after the `sc.AdminSPA = handlers.NewAdminSPA(...)` line ~line 390)

```go
	sc.AdminSPA = handlers.NewAdminSPA(cfg.AdminSPA.DistDir, spaConnect...)
	sc.WidgetStatic = handlers.NewWidgetStatic(cfg.Widget.DistDir)
```

- [ ] **Step 5: Read the env var** (`cmd/coordinator/main.go`)

Add the doc comment near the other env docs (~line 36, after the `NOVA_ADMIN_DIST_DIR` line):

```go
//	NOVA_ADMIN_DIST_DIR                  M11 admin SPA bundle dir served at /admin/* (unset ⇒ disabled)
//	NOVA_WIDGET_DIST_DIR                 M12 upload-widget bundle dir served at /widget/* (unset ⇒ disabled)
```

Add the config field to the `coordinator.Config{...}` literal, immediately after the `AdminSPA: coordinator.AdminSPAConfig{...}` block (~line 228):

```go
		AdminSPA: coordinator.AdminSPAConfig{
			DistDir: os.Getenv("NOVA_ADMIN_DIST_DIR"),
		},
		Widget: coordinator.WidgetConfig{
			DistDir: os.Getenv("NOVA_WIDGET_DIST_DIR"),
		},
```

- [ ] **Step 6: Build the whole tree + re-run the handler tests**

Run: `go build ./... && go test ./internal/api/... -run 'Widget|Server' -v`
Expected: build succeeds; handler tests pass.

- [ ] **Step 7: gofmt touched files**

Run: `gofmt -w internal/api/server.go pkg/coordinator/coordinator.go cmd/coordinator/main.go`
Expected: no diff (already formatted).

- [ ] **Step 8: Commit**

```bash
git add internal/api/server.go pkg/coordinator/coordinator.go cmd/coordinator/main.go
git commit -m "feat(coordinator): wire /widget/* static behind NOVA_WIDGET_DIST_DIR (m12)"
```

---

## Task 12: nginx-fronted integration test

**Files:**
- Create: `internal/integration/m12_upload_widget_test.go`

Reuses the package's existing helpers (`startCoordinatorWithNginxCfg`, `seedAuthUser`, `m6Login`, `doJSONAuth`, `b64`, `decodeCID`, `offlineBackend`, `dbtest.New`, `atoiPort`, `ipfs.WriteFileForTest`). The test boots with `PublicUploads: false` and an uploader token (the widget's primary authenticated path) and asserts the no-token `401` boundary. It writes a **synthetic** dist (no npm build in Go — the real bundle's hermeticity is the CI `make hermetic-widget` gate); the anonymous public-floor happy path is already proven by `m4`/`m5`.

- [ ] **Step 1: Write `internal/integration/m12_upload_widget_test.go`**

```go
package integration_test

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/nova-archive/nova/internal/api"
	"github.com/nova-archive/nova/internal/auth"
	"github.com/nova-archive/nova/internal/auth/localissuer"
	"github.com/nova-archive/nova/internal/auth/password"
	"github.com/nova-archive/nova/internal/auth/signedurl"
	"github.com/nova-archive/nova/internal/auth/token"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/dbtest"
	"github.com/nova-archive/nova/internal/envelope"
	"github.com/nova-archive/nova/internal/ipfs"
	"github.com/nova-archive/nova/pkg/coordinator"
)

// TestIntegrationM12UploadWidgetThroughNginx exercises the M12 surface end-to-end
// through nginx: the coordinator-served widget static seam (/widget/*, strict CSP,
// caching, 404 with no SPA fallback) and the real upload lifecycle the widget
// drives (tus create → PATCH → finalize → CID → /blob render), plus the no-token
// 401 boundary with the public-uploads floor off.
func TestIntegrationM12UploadWidgetThroughNginx(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping M12 integration test in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	pool := dbtest.New(t, ctx)
	t.Setenv("NOVA_MASTER_KEY_V1", "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff")
	t.Setenv("NOVA_MASTER_KEY_ACTIVE", "v1")
	ks, err := envelope.NewKeystoreFromEnv(pool)
	require.NoError(t, err)
	_, err = ks.Bootstrap(ctx)
	require.NoError(t, err)
	require.NoError(t, signedurl.EnsureActiveKey(ctx, gen.New(pool), ks))

	backend := offlineBackend(t, ctx)

	signer, err := token.NewSignerFromSeed(signerSeedHex)
	require.NoError(t, err)
	iss, err := localissuer.New(localissuer.Config{
		Queries: gen.New(pool), Signer: signer, Gate: password.NewGate(4),
		IssuerURL: "https://nova.test/", Audience: "nova",
		AccessTTL: 15 * time.Minute, RefreshTTL: time.Hour,
	})
	require.NoError(t, err)
	authCfg := coordinator.AuthConfig{
		Verifiers:     []auth.Verifier{iss.Verifier()},
		Issuer:        iss,
		Descriptor:    api.AuthConfigDescriptor{Mode: "local"},
		PublicUploads: false, // bearer required ⇒ exercises the widget's getToken path + the 401 boundary
	}

	dist := m12WriteDist(t)

	cfg := coordinator.Config{
		ListenAddr:            "0.0.0.0:19012",
		Version:               "m12-itest",
		RateLimit:             coordinator.RateLimitConfig{RatePerSec: 1000, Burst: 1000},
		MaxUploadSizeBytes:    4 << 20,
		MaxConcurrentAssembly: 4,
		SessionTTL:            time.Hour,
		UploadTmpDir:          t.TempDir(),
		UploadGCInterval:      time.Hour,
		Auth:                  authCfg,
		Widget:                coordinator.WidgetConfig{DistDir: dist},
	}
	base := startCoordinatorWithNginxCfg(t, ctx, pool, backend, ks, cfg, startNginxM12)

	const pw = "hunter2hunter2"
	_ = seedAuthUser(t, ctx, pool, "up@example.com", "uploader", pw)
	upTok, _ := m6Login(t, base, "up@example.com", pw)

	// ---- Serving (Exit #1): the coordinator-served widget seam through nginx.
	code, body, hdr := m11GetRaw(t, base+"/widget/")
	require.Equal(t, http.StatusOK, code)
	require.Contains(t, string(body), "nova upload widget", "/widget/ serves the demo index.html")
	require.Contains(t, hdr.Get("Content-Security-Policy"), "default-src 'self'", "strict CSP")
	require.Equal(t, "no-store", hdr.Get("Cache-Control"), "demo index is no-store")

	code, _, jhdr := m11GetRaw(t, base+"/widget/nova-upload-widget.js")
	require.Equal(t, http.StatusOK, code, "entry bundle served")
	require.Equal(t, "no-cache", jhdr.Get("Cache-Control"), "stable entry JS is no-cache")

	code, _, _ = m11GetRaw(t, base+"/widget/does-not-exist.js")
	require.Equal(t, http.StatusNotFound, code, "unknown widget path 404s (no SPA fallback)")

	// ---- Upload lifecycle (Exit #2): the tus→finalize flow the widget performs.
	payload := []byte("hello from the nova upload widget")
	cid := m12TusUpload(t, base, payload, upTok)
	require.NotEmpty(t, cid)
	rc, rbody, _ := m11GetRaw(t, base+"/blob/"+cid)
	require.Equal(t, http.StatusOK, rc, "finalized blob reads 200")
	require.Equal(t, payload, rbody, "served bytes match the upload")

	// Resume: a fresh session, HEAD-probe the offset, then finalize to a CID.
	cid2 := m12TusResumeUpload(t, base, payload, upTok)
	require.NotEmpty(t, cid2)

	// ---- Token/floor (Exit #3): no token ⇒ 401 (public-uploads floor off).
	req, _ := http.NewRequest(http.MethodPost, base+"/api/v1/uploads", nil)
	req.Header.Set("Tus-Resumable", "1.0.0")
	req.Header.Set("Upload-Length", strconv.Itoa(len(payload)))
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode, "no token ⇒ 401 with the floor off")
}

// --- M12 helpers ------------------------------------------------------------

func m12WriteDist(t *testing.T) string {
	t.Helper()
	dist := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dist, "assets"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dist, "index.html"),
		[]byte(`<!doctype html><html><head><title>nova upload widget</title></head>`+
			`<body><div data-nova-upload-widget></div><script src="/widget/nova-upload-widget.js"></script></body></html>`),
		0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dist, "nova-upload-widget.js"),
		[]byte("/* iife bundle */"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dist, "assets", "uppy-cafebabe.css"),
		[]byte(".uppy{}"), 0o644))
	return dist
}

// m12TusUpload runs the full authenticated tus lifecycle the widget drives:
// create → single PATCH → finalize, returning the resulting CID.
func m12TusUpload(t *testing.T, base string, body []byte, tokenStr string) string {
	t.Helper()
	loc := m12TusCreate(t, base, len(body), tokenStr)
	m12TusPatch(t, base, loc, 0, body, tokenStr, http.StatusNoContent)
	return m12TusFinalize(t, base, loc, tokenStr)
}

// m12TusResumeUpload creates a session, HEAD-probes the (zero) offset, then PATCHes
// and finalizes — proving the resumable HEAD path end-to-end.
func m12TusResumeUpload(t *testing.T, base string, body []byte, tokenStr string) string {
	t.Helper()
	loc := m12TusCreate(t, base, len(body), tokenStr)

	hreq, _ := http.NewRequest(http.MethodHead, base+loc, nil)
	hreq.Header.Set("Tus-Resumable", "1.0.0")
	hreq.Header.Set("Authorization", "Bearer "+tokenStr)
	hresp, err := http.DefaultClient.Do(hreq)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, hresp.StatusCode)
	off, _ := strconv.Atoi(hresp.Header.Get("Upload-Offset"))
	_ = hresp.Body.Close()

	m12TusPatch(t, base, loc, off, body[off:], tokenStr, http.StatusNoContent)
	return m12TusFinalize(t, base, loc, tokenStr)
}

func m12TusCreate(t *testing.T, base string, length int, tokenStr string) string {
	t.Helper()
	meta := "mime_type " + b64("text/plain") + ",filename " + b64("hello.txt") + ",product " + b64("raw")
	req, _ := http.NewRequest(http.MethodPost, base+"/api/v1/uploads", nil)
	req.Header.Set("Tus-Resumable", "1.0.0")
	req.Header.Set("Upload-Length", strconv.Itoa(length))
	req.Header.Set("Upload-Metadata", meta)
	req.Header.Set("Authorization", "Bearer "+tokenStr)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	loc := resp.Header.Get("Location")
	_ = resp.Body.Close()
	require.NotEmpty(t, loc)
	return loc
}

func m12TusPatch(t *testing.T, base, loc string, offset int, chunk []byte, tokenStr string, want int) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPatch, base+loc, bytesReader(chunk))
	req.Header.Set("Tus-Resumable", "1.0.0")
	req.Header.Set("Upload-Offset", strconv.Itoa(offset))
	req.Header.Set("Content-Type", "application/offset+octet-stream")
	req.Header.Set("Authorization", "Bearer "+tokenStr)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, want, resp.StatusCode)
}

func m12TusFinalize(t *testing.T, base, loc, tokenStr string) string {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, base+loc+"/finalize", nil)
	req.Header.Set("Authorization", "Bearer "+tokenStr)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	cid := decodeCID(t, resp.Body)
	_ = resp.Body.Close()
	return cid
}

// startNginxM12 fronts the coordinator with the M12 proxy surface: the upload
// endpoints, blob reads, auth, and the coordinator-served widget static prefix
// (/widget, distinct from /api/v1/uploads).
func startNginxM12(t *testing.T, ctx context.Context, coordPort string) string {
	t.Helper()
	up := "http://host.testcontainers.internal:" + coordPort
	conf := fmt.Sprintf(`
server {
  listen 8080;
  client_max_body_size 100m;
  location = /health      { proxy_pass %s; proxy_set_header X-Forwarded-For $remote_addr; }
  location /blob/         { proxy_pass %s; proxy_set_header X-Forwarded-For $remote_addr; }
  location /widget        { proxy_pass %s; proxy_set_header X-Forwarded-For $remote_addr; }
  location /api/v1/auth/  { proxy_pass %s; proxy_set_header X-Forwarded-For $remote_addr; }
  location /api/v1/uploads { proxy_pass %s; proxy_set_header X-Forwarded-For $remote_addr; proxy_request_buffering off; }
}
`, up, up, up, up, up)

	confPath := filepath.Join(t.TempDir(), "default.conf")
	require.NoError(t, ipfs.WriteFileForTest(confPath, []byte(conf)))

	req := testcontainers.ContainerRequest{
		Image:           "nginx:1.25-alpine",
		ExposedPorts:    []string{"8080/tcp"},
		HostAccessPorts: []int{atoiPort(t, coordPort)},
		WaitingFor:      wait.ForListeningPort("8080/tcp").WithStartupTimeout(60 * time.Second),
		Files: []testcontainers.ContainerFile{{
			HostFilePath:      confPath,
			ContainerFilePath: "/etc/nginx/conf.d/default.conf",
			FileMode:          0o644,
		}},
	}
	ctr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req, Started: true,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		cc, ccancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer ccancel()
		_ = ctr.Terminate(cc)
	})
	host, err := ctr.Host(ctx)
	require.NoError(t, err)
	mapped, err := ctr.MappedPort(ctx, "8080/tcp")
	require.NoError(t, err)
	return fmt.Sprintf("http://%s:%s", host, mapped.Port())
}
```

- [ ] **Step 2: Add the `bytesReader` helper if the package lacks one**

The PATCH helper needs an `io.Reader` over a `[]byte`. Check whether the package already has one:

Run: `grep -rn "func bytesReader" internal/integration/`
- If it exists, delete the local `bytesReader` definition isn't needed (use the existing one).
- If it does NOT exist, add this to the new file (and add `"bytes"` to the imports):

```go
func bytesReader(b []byte) *bytes.Reader { return bytes.NewReader(b) }
```

(If `m5_image_test.go`'s `tusPatch` already takes a `[]byte`, prefer matching its exact reader idiom; the goal is only a `*bytes.Reader` over the chunk.)

- [ ] **Step 3: Run the integration test**

Run: `go test ./internal/integration/ -run TestIntegrationM12UploadWidgetThroughNginx -v`
Expected: PASS (requires Docker for testcontainers; serving + lifecycle + resume + 401 all green). If `m6Login`/`seedAuthUser`/`decodeCID`/`b64`/`atoiPort`/`signerSeedHex`/`offlineBackend`/`m11GetRaw` are reported undefined, confirm they are defined in sibling `internal/integration/*_test.go` files (they are, in `m6`/`m5`/`m11` tests) — the new file is in the same `integration_test` package.

- [ ] **Step 4: gofmt the new file**

Run: `gofmt -w internal/integration/m12_upload_widget_test.go`
Expected: no diff after.

- [ ] **Step 5: Commit**

```bash
git add internal/integration/m12_upload_widget_test.go
git commit -m "test(m12): nginx-fronted e2e — widget serving + tus→finalize lifecycle + 401 boundary (m12)"
```

---

## Task 13: Documentation reconciliations

**Files:**
- Modify: `docs/ROADMAP.md`, `docs/THREAT_MODEL.md`, `docs/legal/OPERATOR_CHECKLIST.md`, `docs/specs/openapi.yaml`, `docs/superpowers/specs/2026-05-25-phase1-single-node-mvp-design.md`, `nginx/nova.conf.example`

- [ ] **Step 1: `docs/ROADMAP.md` — mark the M12 row done**

Replace the `| **M12** |` table row (the drag-and-drop widget line) with a completed entry matching the M11 row's style:

```markdown
| **M12** ✅ | Drag-and-drop upload widget (`web/widget/`): hermetic Vite **library-mode** IIFE bundle exposing the global `NovaUploadWidget` (single-`<script>` embed, stable entry filename, CSS injected at runtime); `@uppy/core`+`drag-drop`+`tus`+`status-bar` (3.x; the maintained `@uppy/status-bar`, **not** the deprecated `@uppy/progress-bar`); the Nova-aware finalize orchestrator (tus `upload-success` is transport-only → `POST .../finalize` → `UploadResult`); `getToken()` resolved **per request** (survives the M6 15-min access TTL; `null` ⇒ public-uploads floor); `mount`/`mountAll` + a `data-nova-upload-widget` auto-bootstrap with a `WeakMap` double-mount guard. Backend slice: a feature-gated coordinator `/widget/*` static seam (`internal/api/handlers/widget_static.go`, strict CSP, no SPA fallback) gated by `NOVA_WIDGET_DIST_DIR`; `web/widget` re-added to the root workspaces; a `hermetic-widget` CI gate. Implemented (tag `m12-upload-widget`). Design: `docs/superpowers/specs/2026-06-07-phase1-m12-upload-widget-design.md`. Plan: `docs/superpowers/plans/2026-06-07-phase1-m12-upload-widget.md`. Deferrals: cross-origin embedding + first-class CORS → operator nginx / later milestone; production nginx two-vhost split + Docker → M13; rich Uppy Dashboard / hosted upload app → later; tus-result preset URLs → later backend change.
```

- [ ] **Step 2: `docs/THREAT_MODEL.md` — extend the hermetic-asset boundary**

Find the Tier-1 hermetic-asset commitment (the admin-SPA "no third-party runtime requests, CI-enforced" entry added in M11). Append the widget to it. Add a sentence to that entry, e.g.:

```markdown
The same hermetic guarantee covers the M12 upload widget (`web/widget/`): Uppy + tus
are bundled, CSS is injected locally, there is no telemetry, and the `hermetic-widget`
CI gate fails the build on any external origin in `web/widget/dist`. Phase-1 widget
embedding is **same-origin** (the host page is served from the Nova origin); cross-origin
embedding requires operator-managed CORS at the reverse proxy and is deferred with the
hardened two-vhost public/admin split (M13). No first-class coordinator CORS surface
ships in Phase 1.
```

(Match the document's existing heading/wording; if the M11 hermetic entry is a bullet, add a sibling bullet rather than a paragraph.)

- [ ] **Step 3: `docs/legal/OPERATOR_CHECKLIST.md` — the widget runbook**

Add a subsection near the M11 admin-SPA runbook (`NOVA_ADMIN_DIST_DIR`):

```markdown
### Upload widget (M12)

Build the widget bundle and point the coordinator at it to serve `/widget/*`:

    npm ci
    make widget-build           # → web/widget/dist (bundle + demo page)
    NOVA_WIDGET_DIST_DIR=$PWD/web/widget/dist   # coordinator env; unset ⇒ /widget disabled

Embed it in any **same-origin** page with a single script tag:

    <div data-nova-upload-widget data-product="image"></div>
    <script src="/widget/nova-upload-widget.js"></script>

For authenticated uploads, mount via JS and supply a token provider (never put a
bearer token in a `data-*` attribute):

    NovaUploadWidget.mount('#uploader', {
      product: 'image',
      getToken: async () => yourApp.getAccessToken(),   // called per request; survives token rotation
      onComplete: (r) => console.log(r.cid, r.urls),
    });

Notes:
- `getToken` returning `null` sends no `Authorization` header — uploads then require
  the public-uploads floor (`NOVA_PUBLIC_UPLOADS=true`, which itself requires
  `NOVA_TOS_URL`). The zero-JS `data-nova-upload-widget` path only works under that floor.
- The bundle is hermetic (no third-party CDN); CI enforces it (`make hermetic-widget`).
- Phase-1 embedding is same-origin. To embed on a different origin, add CORS for the
  upload endpoints at your reverse proxy; first-class CORS lands with the M13 host split.
```

- [ ] **Step 4: `docs/specs/openapi.yaml` — note-only (no API path change)**

Find the `upload` tag's description (or the top-level `info`/`tags` block) and add a short note that the coordinator optionally serves the widget bundle as a non-API static surface. Do NOT add any `paths:` entry. Minimal change — add to the `upload` tag description, mirroring how the admin `/admin/*` static surface is noted:

```yaml
  - name: upload
    description: |
      Resumable (tus.io) and multipart upload endpoints. The M12 browser widget
      (`web/widget/`) is the reference consumer of this surface; after the final
      tus `PATCH`, it calls `POST .../finalize`. When `NOVA_WIDGET_DIST_DIR` is set
      the coordinator also serves the widget bundle as static assets at `/widget/*`
      (a non-API surface, not described here).
```

(If the `upload` tag has no description yet, add one; otherwise append the M12 sentence. Keep the `oapi-codegen` drift gate green — no schema/path change.)

- [ ] **Step 5: `docs/superpowers/specs/2026-05-25-phase1-single-node-mvp-design.md` — M12 status**

In the "Walking-skeleton milestone breakdown" section, update the **M12** entry to mark it implemented and link this design + plan (mirror how the M11 entry was annotated on completion). One or two lines:

```markdown
**M12 — Widget (implemented, tag `m12-upload-widget`)**
- Hermetic Uppy + tus embeddable widget (`web/widget/`); single-`<script>` embed;
  `getToken` bearer; coordinator `/widget/*` seam (`NOVA_WIDGET_DIST_DIR`).
- Design: `docs/superpowers/specs/2026-06-07-phase1-m12-upload-widget-design.md`.
  Plan: `docs/superpowers/plans/2026-06-07-phase1-m12-upload-widget.md`.
```

- [ ] **Step 6: `nginx/nova.conf.example` — add the `location /widget` note**

After the `location /admin { ... }` block (~line 197), add a sibling block:

```nginx
    # ----- Upload widget (M12) -----------------------------------------
    #
    # The coordinator serves the hermetic widget bundle + demo at /widget/*
    # when NOVA_WIDGET_DIST_DIR is set (unset ⇒ 404). Same hermetic CSP as the
    # admin SPA (declared above). M13 may serve web/widget/dist directly here.
    location /widget {
        limit_req zone=nova_per_ip burst=30 nodelay;
        proxy_pass http://nova_coordinator;
    }
```

- [ ] **Step 7: Verify the codegen/drift gates are still green**

Run: `go build ./... && go test ./internal/api/handlers/ -run TestWidgetStatic`
Expected: PASS. (No sqlc/openapi schema change in M12, so `make sqlc-generate`/`oapi-codegen` are unaffected; the openapi edit is a description-only note.)

- [ ] **Step 8: Commit**

```bash
git add docs/ROADMAP.md docs/THREAT_MODEL.md docs/legal/OPERATOR_CHECKLIST.md docs/specs/openapi.yaml docs/superpowers/specs/2026-05-25-phase1-single-node-mvp-design.md nginx/nova.conf.example
git commit -m "docs(m12): roadmap, threat-model, operator runbook, openapi note, nginx, master plan (m12)"
```

---

## Final verification (before tagging)

- [ ] **Frontend:** `make widget-lint && make widget-test && make widget-build && make hermetic-widget` — all green.
- [ ] **Backend:** `go build ./... && go test ./internal/api/handlers/ -run TestWidgetStatic` — green.
- [ ] **Integration (Docker):** `go test ./internal/integration/ -run TestIntegrationM12UploadWidgetThroughNginx` — green.
- [ ] **Full Go suite (short):** `go test ./... -short` — green (no regressions from the seam wiring).
- [ ] **gofmt:** `gofmt -l internal/api/handlers/widget_static.go internal/api/server.go pkg/coordinator/coordinator.go cmd/coordinator/main.go internal/integration/m12_upload_widget_test.go` — prints nothing.
- [ ] **Exit criteria** (design §"Exit criteria") 1–6 each map to a passing test: serving + hermetic (Task 8/12), tus→finalize→CID + resume (Task 12), per-request token + 401 (Tasks 3/5/12), mount/mountAll/WeakMap (Task 7), single-`<script>` hermetic integration (Task 8), workspace + CI + unchanged upload contract (Tasks 1/9/12).

When all green, follow the milestone workflow: fast-forward merge `m12-upload-widget` into `main` and create the annotated tag `m12-upload-widget` (no remote push).
```
