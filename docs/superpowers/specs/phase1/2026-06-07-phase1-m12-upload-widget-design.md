# Phase 1 M12 — Upload Widget Design

## Purpose and scope

M12 is the twelfth Phase-1 milestone and the **second human-facing surface** (M11 shipped the
operator command center; M12 ships the end-user uploader; M13 the setup wizard; M14 polishes). It
delivers Nova's **embeddable drag-and-drop upload widget**: a hermetic, framework-agnostic
JavaScript bundle under `web/widget/` that an operator drops into any HTML page with a single
`<script>` tag to let users upload images (and other blob products) straight into their Nova node.
`docs/ROADMAP.md` M12 row and the master breakdown
(`docs/superpowers/specs/phase1/2026-05-25-phase1-single-node-mvp-design.md` § "Walking-skeleton milestone
breakdown" → M12) commit exactly that surface: Uppy + tus.io, embeds via `<script src=".../widget.js">`,
calls `/api/v1/uploads/*` with a bearer token, hermetic build with no external CDN.

**Unlike M11, M12 is a frontend-dominant milestone.** Reading the committed upload handler
(`internal/api/handlers/upload.go`) against `docs/specs/openapi.yaml` shows **no spec-vs-router
drift**: the tus endpoints (`POST /api/v1/uploads`, `HEAD`/`PATCH`/`DELETE /api/v1/uploads/{id}`),
the Nova-specific `POST /api/v1/uploads/{id}/finalize`, and the `UploadResult` body are all mounted,
implemented, and documented (the openapi even names the widget as the downstream consumer of this
contract — `:359`–`:497`). The widget is a **pure client** of an upload contract that already exists.
The only backend addition is a small, optional **coordinator static-serving seam** (`/widget/*`,
gated by `NOVA_WIDGET_DIST_DIR`) that mirrors M11's admin-SPA seam so the bundle and a demo page can
be served and the milestone can be proved end-to-end through nginx.

So M12 = **the widget bundle (Uppy + tus + a Nova-aware finalize orchestrator) + a coordinator
static seam + a demo/example page + a frontend CI lane + an nginx-fronted integration proof.** The
long pole is not the drag-and-drop UI — it is the **transport→finalize boundary**: Uppy/tus reports
success when the last chunk lands (`offset == length`), but that is only a *transport* milestone; a
blob is not committed and has no CID until the widget calls `finalize`. Getting that boundary correct
(including auth, error mapping, and resume) is the milestone's load-bearing work.

### In scope

- **`web/widget/` — the embeddable widget.** A hermetic Vite **library-mode** build (IIFE, global
  `NovaUploadWidget`, stable entry filename `nova-upload-widget.js`, CSS injected at runtime so the
  whole integration is one `<script>` tag). Engine: `@uppy/core` + `@uppy/drag-drop` +
  `@uppy/tus` + `@uppy/status-bar` (the maintained progress plugin — **not** the deprecated
  `@uppy/progress-bar`), pinned to Uppy **3.x** (Node-16-safe, matching the `web/admin`
  Vite-4.5/Vitest-0.34 posture). Brand-tokened styles over Uppy's DragDrop/StatusBar. No Dashboard.
- **The host-facing embedding API.** A canonical JS constructor —
  `NovaUploadWidget.mount(target, options) → { destroy() }` plus
  `NovaUploadWidget.mountAll(root = document) → Handle[]` — and a **thin auto-bootstrap** that mounts
  **only** elements explicitly marked `data-nova-upload-widget` (reading *non-secret* config from
  `data-endpoint` / `data-product` / `data-collection`). One isolated Uppy/Nova instance per host
  element, tracked in a `WeakMap<Element, Instance>` so double-mounting an element returns the
  existing handle (never a duplicate uploader). No whole-page scanning, no implicit auth discovery.
- **The Nova finalize orchestrator.** The widget treats Uppy/tus `upload-success` (offset==length) as
  a transport milestone, then `POST {uploadURL}/finalize` (bearer) and parses the `UploadResult`
  (`{cid, byte_size, mime_type, product, urls}`). User-visible completion (`onComplete(result)`)
  fires **only after finalize returns** — the Nova completion point, not Uppy's transport-success.
- **The token model.** `getToken: () => string | null | Promise<string|null>` resolved **per request**
  (tus create/PATCH/HEAD/DELETE via the tus `onBeforeRequest` hook; the finalize `POST` via the
  widget's own fetch) so a long resumable upload survives the M6 15-minute access-token expiry. A
  `null`/empty token sends no `Authorization` header (works under the T1.20 public-uploads floor). A
  `token: string` option exists only as sugar for `getToken: () => token`; `getToken` wins if both
  are supplied.
- **Coordinator static-serving seam** — `internal/api/handlers/widget_static.go`, an optional handler
  that serves `web/widget/dist` (the bundle + a demo `index.html`) at `/widget/*` when
  `NOVA_WIDGET_DIST_DIR` is set, with a strict CSP, immutable caching for content-hashed assets and
  `no-cache` for the stable entry JS; `/widget/*` → `404` when unset (feature-gated like every other
  optional handler in `ServerConfig`). Simpler than the admin SPA's handler — **no SPA fallback**
  (the widget is not a router-driven app; it is a script + a demo page).
- **A demo/example host page** (`web/widget/public/index.html` — Vite copies `public/` → `dist/`, so
  it is served at `/widget/`) — a minimal HTML page that embeds the widget via the single-`<script>`
  integration and renders the returned CID (`urls.original` = `/blob/{cid}`; `/i/{cid}` for images),
  satisfying the Phase-1 demo criterion ("use the widget on a test HTML page; drop a file; observe
  progress; observe `201 + CID`; observe the content renders").
- **Workspace + build/CI plumbing** — re-add `web/widget` to the root `package.json` `workspaces`
  (removed since the admin-only M11 narrowing); Makefile `widget-{install,build,lint,test}` +
  `hermetic-widget`; a Node job (or extension of `web-admin`) in `ci.yml`; the dir-parametrized
  `scripts/hermetic-spa.sh` reused as `hermetic-spa.sh web/widget/dist`.
- Unit tests (Vitest for the orchestrator/auth/multi-mount/bootstrap; Go for `widget_static`) + an
  nginx-fronted integration test (`internal/integration/m12_upload_widget_test.go`) proving the exit
  criteria against the **real** upload lifecycle the widget drives.

### Out of scope (with the milestone/owner that holds each)

- **Cross-origin embedding + a first-class coordinator CORS posture** — **deferred (operator nginx
  concern / later milestone).** Per the M12 origin decision, Phase-1 embedding is **same-origin**: the
  host page is served from the Nova origin (behind the same nginx/coordinator). Nova's existing
  browser surfaces already assume same-origin (`web/admin/src/api/client.ts`), the upload lifecycle is
  a multi-verb authenticated flow (`POST`/`HEAD`/`PATCH`/`DELETE`/`finalize`) whose cross-origin
  support would require a full CORS policy across coordinator *and* nginx, and the hardened
  public/admin origin split is itself an **M13** deliverable. Cross-origin embedding is documented as
  an operator-added reverse-proxy/CORS concern; first-class CORS is revisited only once M13's host
  split and production packaging land. **No CORS code ships in M12.**
- **Production nginx templating, the two-vhost public/admin split, and Docker packaging** — **M13.**
  M12 serves the widget from the coordinator behind `/widget` and uses nginx's existing upload
  location + a `location /widget` proxy (mirroring `location /admin`) for its integration test.
- **A rich Uppy Dashboard / hosted upload application** — **out (revisitable later).** M12 ships the
  lean embeddable widget (DragDrop + StatusBar). A Dashboard-based hosted page, image cropping/editing,
  webcam/screen-capture/remote-source plugins (Google Drive, URL import, etc.) are explicitly not in
  scope; they can be added as an additional surface without reshaping the lean widget.
- **Preset/derivative URLs in the widget result** — **out (backend nicety, not pursued).** The tus
  `finalize` path returns `UploadResult` without `urls.presets` (only the multipart `/api/v1/images`
  path builds presets — `upload.go:287`–`:290`). The widget surfaces `urls.original` (`/blob/{cid}`)
  and the demo renders images via `/i/{cid}`; wiring preset URLs into the tus result is a later
  backend change, not an M12 deliverable.
- **An upload history / "my uploads" view, multi-file batch finalize, chunk-size tuning UI** —
  **out.** The widget reports each file's result via `onComplete`; persistence/listing is the host
  application's concern (and operator tooling is the admin SPA's).
- **`operator.yaml` decode for the M12 knob** — still deferred (M5–M11 precedent); env knob only
  (`NOVA_WIDGET_DIST_DIR`).

## Source of truth and required doc reconciliations

1. **`docs/specs/openapi.yaml` — note-only; the upload contract is unchanged.** The tus endpoints,
   `finalize`, and `UploadResult` are already specced and already name the widget as their consumer
   (`:359`–`:497`, `:2505` `UploadResult`). M12 adds **no** API path. Add a one-line non-API note
   that the coordinator optionally serves a static `/widget/*` surface (the bundle + demo), mirroring
   how M11 documented the static `/admin/*` surface. Keep the `oapi-codegen` drift gate green.
2. **`docs/ROADMAP.md` + the master plan — the M12 row.** Mark status, link this design + its
   implementation plan, record the `m12-upload-widget` tag on completion, and record the deferrals
   (cross-origin/CORS → operator nginx or later; production nginx/Docker → M13; Dashboard/rich UI →
   later; tus-result preset URLs → later backend change).
3. **`docs/THREAT_MODEL.md` — extend the hermetic-asset boundary to the widget bundle.** The admin
   SPA's Tier-1 "no third-party runtime requests, CI-enforced" commitment now also covers the widget
   bundle (fonts/scripts/styles bundled, no telemetry, CI `hermetic-widget` gate; the
   `nginx/nova.conf.example` CSP at `:113`–`:116` already names the widget bundle as hermetic). Record
   that Phase-1 widget embedding is **same-origin** behind the single Phase-1 origin, that cross-origin
   embedding requires operator-managed CORS (deferred), and that this is consistent with the hardened
   two-vhost split deferred to M13.
4. **`docs/legal/OPERATOR_CHECKLIST.md` — the widget runbook.** Document `NOVA_WIDGET_DIST_DIR` (build
   `web/widget/dist` + point the coordinator at it; unset ⇒ `/widget` disabled), the single-`<script>`
   embed snippet (constructor + `data-nova-upload-widget` auto-bootstrap), the token model (`getToken`
   provider; how it interacts with the public-uploads floor), the same-origin guidance, and the
   cross-origin/CORS caveat.
5. **`docs/specs/PRODUCT_MODULE_INTERFACE.md` (no change expected).** The widget is a pure API client;
   it introduces no product-interface change. (Listed for completeness; expected to need no edit.)

## Preconditions from M1–M11 (confirmed in committed code)

- **The upload contract is complete and mounted** (`internal/api/handlers/upload.go`,
  `internal/api/server.go:208`–`:230`). `CreateTus` requires `Tus-Resumable: 1.0.0` + `Upload-Length`,
  parses `Upload-Metadata` (`filename`, `mime_type`, `product`, `collection_id`), returns `201` +
  `Location: /api/v1/uploads/{id}` + `Upload-Offset: 0`. `HeadTus` returns `Upload-Offset` /
  `Upload-Length` (resume probe). `PatchTus` requires `Content-Type: application/offset+octet-stream`
  + `Upload-Offset`, returns `204` + new `Upload-Offset`. `DeleteTus` abandons. `FinalizeTus`
  (`POST .../finalize`) reassembles, validates MIME, commits, and returns `200 UploadResult`
  (`writeUploadResult`, `:316`: `{cid, byte_size, mime_type, product, urls{original, json}}` +
  headers `X-Nova-Cid`, `X-Nova-Envelope-Version: 1`). The routes are gated by `PublicUploads` (open
  group) **or** `bearer.RequireRole("uploader","moderator","operator")` (`server.go:224`–`:229`).
- **`writePutError` defines the finalize/commit error vocabulary** (`upload.go:294`): `413
  payload_too_large`, `400 mime_rejected`, `404 not_found` (collection), `503 server_busy`
  (`Retry-After`), `422 moderation_rejected`, `451 blocklisted`, `500 internal`; finalize also yields
  `409 upload_incomplete` and `404` for an unknown session. These map to the widget's `onError(code)`.
- **nginx already streams uploads and already anticipates the widget**
  (`nginx/nova.conf.example`). `location ~ ^/api/v1/(uploads|blobs|images)(/|$)` sets
  `proxy_request_buffering off; proxy_buffering off;` (`:167`–`:173`) — tus chunk streaming works
  today. `client_max_body_size 100m` (`:122`). The CSP (`:115`–`:116`) is `default-src 'self'; img-src
  'self' data: blob:; style-src 'self' 'unsafe-inline'; script-src 'self'; connect-src 'self';
  frame-ancestors 'none'; base-uri 'self'; form-action 'self'` with a comment (`:113`) that **the
  widget bundle and admin SPA are hermetic** — the widget's runtime needs (`blob:` previews,
  `connect-src 'self'` uploads, `script-src 'self'`) are already covered. The `location /admin` proxy
  (`:194`) is the pattern the integration test mirrors for `/widget`.
- **The admin-SPA static seam is the precedent to mirror** (M11). `internal/api/handlers/admin_spa.go`
  is nil when the dist dir is empty, serves a strict CSP, immutable-caches hashed assets, and
  SPA-falls-back to `index.html`; `ServerConfig.AdminDistDir` + `cmd/coordinator` reads
  `NOVA_ADMIN_DIST_DIR`; the router mounts `/admin/*` only when the handler is non-nil. M12's
  `widget_static.go` follows the same gating, **minus** the SPA fallback.
- **The hermetic gate is already dir-parametrized** (`scripts/hermetic-spa.sh`): `dist="${1:-
  web/admin/dist}"`; it greps `dist`'s HTML/CSS for `https?://` origins (excluding the `w3.org` XML
  namespace) and fails on any hit. M12 reuses it verbatim as `hermetic-spa.sh web/widget/dist` (a
  `hermetic-widget` Make target); the script's "admin" wording is generalized to "bundle".
- **The `web/admin` build conventions to mirror** (`web/admin/package.json`, `vite.config.ts`,
  `tsconfig.json`): `type: module`; scripts `build`/`lint`(`tsc --noEmit`)/`test`(`vitest`); Vite
  `^4.5`, Vitest `^0.34`, TypeScript `^5.3`, jsdom `^22.1` (the Node-16-safe pins); `tsconfig`
  `target ES2020`, `moduleResolution bundler`, `strict`, `noUnusedLocals/Parameters`. The widget
  diverges only where it must: **library mode** (not an app), **IIFE global** (not ESM SPA), and
  **vanilla TS** (not React) — because it embeds in arbitrary host pages.
- **Root workspace + CI** (`package.json` `workspaces: ["web/admin"]`; `.github/workflows/ci.yml`
  `web-admin` job: `actions/setup-node@v4` Node 20, `npm ci`, `make admin-{lint,test,build}` +
  `hermetic-spa`). M12 re-adds `web/widget` to `workspaces` and adds the parallel widget steps.
- **The integration harness** (`internal/integration`): `startCoordinatorWithNginxCfg`
  (`m10_master_key_rotation_test.go:279`) boots the coordinator behind a per-test nginx config string;
  M11's `startNginxM11` (`m11_admin_spa_test.go:223`) adds `location /admin`. `m6Login` / `doJSONAuth`
  / `seedAuthUser` / `internal/blobfixture` / `internal/dbtest` (Postgres testcontainer) are the
  reusable primitives; the M12 test adds `location /widget` and drives a real tus→finalize lifecycle.

## Architecture

```
web/widget/  (hermetic Vite IIFE library; global NovaUploadWidget; one <script> integration)
  src/index.ts      NovaUploadWidget.{ mount(target, opts): Handle, mountAll(root): Handle[] }
                    + auto-bootstrap: on DOMContentLoaded, mountAll over [data-nova-upload-widget]
                    + WeakMap<Element, Instance> registry (double-mount ⇒ existing handle)
  src/widget.ts     per-instance: Uppy(core) + DragDrop + StatusBar wiring; mount/destroy lifecycle
  src/uploader.ts   @uppy/tus config (endpoint, allowedMetaFields, onBeforeRequest) +
                    the Nova finalize orchestrator (upload-success ⇒ POST .../finalize ⇒ UploadResult)
  src/auth.ts       resolveToken(): await getToken() per request; token: t ⇒ getToken: () => t
  src/config.ts     normalize options + parse data-endpoint/data-product/data-collection
  src/api/types.ts  UploadResult, ErrorBody (tracked to docs/specs/openapi.yaml)
  src/widget.css    brand-tokened overrides for Uppy DragDrop/StatusBar
  public/index.html demo/example host page (Vite copies public/ → dist/; served at /widget/) — renders /blob/{cid}

coordinator  (the only backend change — feature-gated, mirrors M11)
  internal/api/handlers/widget_static.go   serve web/widget/dist + demo index at /widget/*
                                           (strict CSP; immutable hashed assets; no-cache entry JS;
                                            404 when NOVA_WIDGET_DIST_DIR unset; NO SPA fallback)
  internal/api/server.go                   ServerConfig.WidgetDistDir; mount /widget/* (nil-gated)
  pkg/coordinator/coordinator.go           build WidgetStatic handler; Widget config section
  cmd/coordinator/main.go                  NOVA_WIDGET_DIST_DIR
```

The widget holds **no privilege the API does not already enforce**: it sends bearer tokens (or none,
under the public-uploads floor) to endpoints whose authorization the coordinator owns. The static
seam is a thin, optional coordinator concern (the converging path to M13's nginx-direct serving),
gated exactly like the admin SPA. `/widget` (static) and `/api/v1/uploads` (API) never collide.

### Package boundaries

| Unit | Responsibility | Depends on |
|---|---|---|
| `web/widget/src/index.ts` | global entry: `mount`/`mountAll`, auto-bootstrap, `WeakMap` registry | `widget`, `config` |
| `web/widget/src/widget.ts` | one Uppy(core)+DragDrop+StatusBar instance per element; `destroy()` | `@uppy/*`, `uploader`, `config` |
| `web/widget/src/uploader.ts` | tus config + the finalize orchestrator (the transport→finalize boundary) | `@uppy/tus`, `auth`, `api/types` |
| `web/widget/src/auth.ts` | per-request token resolution; `token`→`getToken` sugar | — |
| `web/widget/src/config.ts` | option normalization + `data-*` parsing | — |
| `internal/api/handlers` (`widget_static.go`) | optional `/widget/*` static (dist + demo; CSP; caching) | `net/http`, `httputil` |
| `internal/api` (`server.go`) | mount `/widget/*`; new `ServerConfig.WidgetDistDir` | `handlers` |
| `pkg/coordinator` | build the WidgetStatic handler; wire `NOVA_WIDGET_DIST_DIR` | `handlers` |

Each `web/widget` unit answers the three isolation questions: `uploader.ts` *does the tus+finalize
flow* (use: give it options + an Uppy file lifecycle; depends on `@uppy/tus` + `auth`); `auth.ts`
*resolves a token per request* (use: `await resolveToken(opts)`; depends on nothing); `index.ts`
*manages instances on the page* (use: `mount`/`mountAll`; depends on `widget`). The split keeps the
load-bearing finalize logic (`uploader.ts`) testable in isolation from the DOM/registry concerns.

## The transport→finalize boundary (the key logic)

Standard tus completes an upload when the last `PATCH` brings `offset == length`; tus-js-client
fires `onSuccess` and Uppy emits `upload-success`. **Nova does not commit a blob at that point** — the
bytes sit in `nova-tmp-uploads/{id}/` until `POST /api/v1/uploads/{id}/finalize` reassembles,
validates the declared MIME against magic bytes, encrypts, imports to IPFS, and writes the metadata
row. Only `finalize` yields a CID. The widget bridges the gap:

```ts
// web/widget/src/uploader.ts (shape, not final code)
const uppy = new Uppy({ autoProceed: true, restrictions: { maxFileSize } })
  .use(DragDrop, { target })
  .use(StatusBar, { target })
  .use(Tus, {
    endpoint,                                   // /api/v1/uploads
    chunkSize,                                  // aligned to the server's expectations
    allowedMetaFields: ['filename', 'mime_type', 'product', 'collection_id'],
    async onBeforeRequest(req) {                // per-request auth for create/PATCH/HEAD/DELETE
      const tok = await resolveToken(opts);
      if (tok) req.setHeader('Authorization', `Bearer ${tok}`);
    },
  });

uppy.on('file-added', (file) => uppy.setFileMeta(file.id, {
  filename: file.name, mime_type: file.type, product, ...(collectionId && { collection_id: collectionId }),
}));

uppy.on('upload-success', async (file, resp) => {            // TRANSPORT milestone only
  const tok = await resolveToken(opts);
  const r = await fetch(`${resp.uploadURL}/finalize`, {       // NOVA completion point
    method: 'POST', headers: tok ? { Authorization: `Bearer ${tok}` } : {},
  });
  if (!r.ok) return onError(mapError(await r.json().catch(() => ({}))));
  onComplete(await r.json() as UploadResult);                 // { cid, byte_size, mime_type, product, urls }
});
```

Consequences captured by the tests:

- **Completion is finalize, not transport.** `onComplete` never fires on `upload-success` alone; a
  finalize failure (`409 upload_incomplete`, `400 mime_rejected`, `422 moderation_rejected`, `451
  blocklisted`, `503 server_busy`, …) surfaces via `onError(code)` with the file shown errored.
- **Auth is per request.** `resolveToken` is awaited on every tus request (via `onBeforeRequest`) and
  on the finalize `fetch`, so a token rotated mid-upload (M6's 15-minute access TTL) is honored; a
  `null` token sends no `Authorization` header (public-uploads floor).
- **Resume is the server's offset.** On retry, tus-js-client `HEAD`s the session and continues from
  the server `Upload-Offset`; the widget adds nothing — but the integration test exercises a
  `HEAD`-probed resume to prove the chunked path end-to-end.

## The host-facing embedding API

The bundle exposes a global `NovaUploadWidget` with the constructor as the canonical API and a thin,
explicit auto-bootstrap as convenience:

```ts
type NovaUploadWidgetHandle = { destroy(): void };

interface MountOptions {
  endpoint?: string;          // default '/api/v1/uploads' (same-origin)
  product?: string;           // 'image' | 'raw' | … (Upload-Metadata product); default 'raw'
  collectionId?: string;      // UUID → Upload-Metadata collection_id
  maxFileSize?: number;       // client-side restriction (server still enforces)
  getToken?: () => string | null | Promise<string | null>;
  token?: string;             // sugar for getToken: () => token (getToken wins if both)
  onComplete?: (r: UploadResult) => void;   // fires AFTER finalize
  onError?: (e: { code: string; message?: string }) => void;
  onProgress?: (p: { bytesUploaded: number; bytesTotal: number }) => void;
}

window.NovaUploadWidget = {
  mount(target: string | Element, options?: MountOptions): NovaUploadWidgetHandle,
  mountAll(root?: ParentNode): NovaUploadWidgetHandle[],   // over [data-nova-upload-widget]
};
```

- **Auto-bootstrap is explicit and narrow.** On `DOMContentLoaded` the bundle calls
  `mountAll(document)`, which mounts **only** elements bearing `data-nova-upload-widget`, reading
  *non-secret* config from `data-endpoint` / `data-product` / `data-collection`. There is **no
  whole-page scan** and **no implicit auth discovery** — a token is wired only through JS (`getToken`),
  never a `data-*` attribute (a bearer token in the DOM would be a secret leak). The zero-JS path
  therefore works only under the public-uploads floor; any authenticated embed uses the constructor.
- **Multi-mount is isolated.** Each mount creates one independent Uppy/Nova instance. A
  `WeakMap<Element, Instance>` keyed on the resolved target rejects a double-mount (returns the
  existing handle) so a page never spawns duplicate uploaders, duplicate listeners, or double
  uploads. `destroy()` closes the Uppy instance, removes listeners, and deletes the registry entry.

The single-`<script>` integration (CSS injected at runtime by the bundle) is the whole story:

```html
<!-- zero-JS, public-uploads floor -->
<div data-nova-upload-widget data-product="image"></div>
<script src="/widget/nova-upload-widget.js" defer></script>

<!-- authenticated / callbacks -->
<div id="up"></div>
<script src="/widget/nova-upload-widget.js"></script>
<script>
  NovaUploadWidget.mount('#up', {
    product: 'image',
    getToken: async () => authStore.getAccessToken(),  // refreshed per request
    onComplete: (r) => render(`/i/${r.cid}`),
  });
</script>
```

## The build (hermetic by construction)

- **Library mode, IIFE, stable entry.** `vite.config.ts` sets `build.lib = { entry: 'src/index.ts',
  name: 'NovaUploadWidget', formats: ['iife'], fileName: () => 'nova-upload-widget.js' }` and
  `base: '/widget/'`. The entry filename is **stable** (not content-hashed) so
  `<script src="/widget/nova-upload-widget.js">` is a durable embed URL operators can hardcode;
  any code-split sub-assets remain hashed + immutable.
- **CSS injected into the JS.** Uppy's `@uppy/core`/`@uppy/drag-drop`/`@uppy/status-bar` CSS plus the
  brand overrides are injected at runtime by the bundle (a build-time-only dev dependency,
  `vite-plugin-css-injected-by-js`, pinned Node-16-safe), so a single `<script>` tag is the complete
  integration — no sibling stylesheet to link. This is a *build tool*, not a runtime asset; the bundle
  remains hermetic.
- **No third-party runtime requests.** Uppy + tus-js-client are bundled; there are no CDN fonts,
  scripts, or analytics. Uppy's icons are inline SVG (no external `url()`); the `hermetic-widget` gate
  (`scripts/hermetic-spa.sh web/widget/dist`) greps `dist`'s HTML/CSS for external origins and fails
  the build on any hit. The coordinator's strict CSP and nginx's `default-src 'self'` are the runtime
  backstops.
- **Node-16-safe pins.** Uppy **3.x** (`@uppy/core`/`drag-drop`/`tus`/`status-bar` `^3`); Vite `^4.5`,
  Vitest `^0.34`, TypeScript `^5.3`, jsdom `^22.1` — matching `web/admin` so `npm ci` works on the
  local Node-16 host and CI's Node 20 alike.

## Coordinator static serving (the widget seam)

`ServerConfig` gains `WidgetDistDir string`. When non-empty the router mounts a `/widget/*` static
handler (`internal/api/handlers/widget_static.go`); when empty `/widget/*` 404s. The handler:

- serves files from `<dist>` — `nova-upload-widget.js`, any hashed sub-assets, and the demo
  `index.html` (served for `/widget/` and `/widget`);
- sets `Cache-Control: public, max-age=31536000, immutable` for content-hashed assets,
  `Cache-Control: no-cache` for the **stable** `nova-upload-widget.js` (so bundle updates propagate),
  and `no-store` for the demo `index.html`;
- emits the same strict CSP as the admin seam / nginx (`default-src 'self'; img-src 'self' data:
  blob:; style-src 'self' 'unsafe-inline'; script-src 'self'; connect-src 'self'; frame-ancestors
  'none'; base-uri 'none'`) plus `X-Content-Type-Options: nosniff`. (`'unsafe-inline'` for styles only
  — the runtime-injected CSS; scripts are `'self'`.)
- has **no SPA fallback**: an unknown `/widget/...` path is a plain `404` (the widget is not a routed
  app). `/widget` and `/api/v1/uploads` are disjoint prefixes.

This is the M12-testable serving path. M13 templates nginx to serve `web/widget/dist` directly; the
coordinator handler remains the dev/self-contained path, exactly as the admin seam converges.

## HTTP contract (unchanged — the widget is a client)

M12 mounts **no new API path**. The widget drives the existing, already-specced contract:

| Method | Path | Auth | Widget use |
|---|---|---|---|
| POST | `/api/v1/uploads` | bearer (or public floor) | create tus session (`Upload-Length`, `Upload-Metadata`) → `201` + `Location` |
| HEAD | `/api/v1/uploads/{id}` | bearer (or public floor) | resume probe → `Upload-Offset` |
| PATCH | `/api/v1/uploads/{id}` | bearer (or public floor) | append chunk → `204` + `Upload-Offset` |
| DELETE | `/api/v1/uploads/{id}` | bearer (or public floor) | abandon |
| POST | `/api/v1/uploads/{id}/finalize` | bearer (or public floor) | **commit** → `200 UploadResult {cid, …, urls}` |

The only new HTTP surface is the static `/widget/*` (non-API, `200` bundle/demo or `404` when
`NOVA_WIDGET_DIST_DIR` unset). Error → `onError` mapping follows `writePutError`
(`upload.go:294`): `413 payload_too_large`, `400 mime_rejected`, `404 not_found`, `503 server_busy`,
`422 moderation_rejected`, `451 blocklisted`, `409 upload_incomplete`, `401 unauthenticated`.

## Configuration

The widget knob follows the **exact M11 `AdminSPAConfig` precedent**: the config struct lives in
`pkg/coordinator/coordinator.go` (not the deferred `operator.yaml` decode in
`internal/config/types.go`), and `cmd/coordinator/main.go` reads the env var into it.

```go
// pkg/coordinator/coordinator.go (sibling to AdminSPAConfig)
type WidgetConfig struct {
    DistDir string // NOVA_WIDGET_DIST_DIR; empty ⇒ /widget/* unmounted
}
// Config gains:  Widget WidgetConfig
```

`cmd/coordinator/main.go` sets `Widget: coordinator.WidgetConfig{DistDir: os.Getenv("NOVA_WIDGET_DIST_DIR")}`
(unset ⇒ `/widget` disabled) — the M7–M11 env precedent. `operator.yaml` decode stays deferred. No
other knob: the size/auth machinery the widget touches is already configured upstream (M4 upload
limits, M6 auth, the T1.20 public-uploads floor).

## Security and privacy considerations

- **Hermetic widget bundle (enforced).** No third-party runtime requests — Uppy/tus bundled, CSS
  injected locally, no telemetry — and the `hermetic-widget` CI gate fails the build on any external
  origin in `dist`. The coordinator's strict CSP and nginx `default-src 'self'` are the runtime
  backstops. This extends the Tier-1 "no third-party CDN assets at runtime" commitment to the widget.
- **Same-origin Phase-1 posture (deliberate).** The widget assumes same-origin (no CORS surface
  added). Cross-origin embedding is an operator-managed reverse-proxy concern, deferred with the M13
  host split. This avoids broadening the authenticated browser write path before the production origin
  topology is finalized.
- **Tokens never touch the DOM.** A bearer token reaches the widget only through `getToken` (JS),
  never a `data-*` attribute; the auto-bootstrap path carries no secret. Tokens are held by the host
  application, not persisted by the widget; the widget keeps no token in storage of its own.
- **Per-request auth bounds long uploads.** `getToken` resolved per request means a rotated/short-TTL
  token is honored across a multi-minute resumable upload; a stale token captured once at mount (the
  rejected static-only model) would fail exactly the long-upload/resume cases tus exists to handle.
- **Server remains the authority.** Client-side `maxFileSize`/MIME hints are convenience only; the
  coordinator enforces size (`413`), decode-based MIME (`400 mime_rejected`), moderation (`422`),
  blocklist (`451`), and authorization (`401`/role) regardless of the widget. The widget can never
  grant an upload the API would refuse.

## Exit criteria

1. With `NOVA_WIDGET_DIST_DIR` set, `GET /widget/` (through nginx) returns the demo `index.html` with
   the strict CSP and **no external-origin references** in the bundle (`make hermetic-widget` passes);
   `GET /widget/nova-upload-widget.js` returns the IIFE bundle (`no-cache`). With it unset,
   `/widget/*` → `404`.
2. The upload lifecycle the widget drives works **end-to-end through nginx**: create tus session
   (`POST /api/v1/uploads`, `Upload-Metadata`) → `PATCH` the chunk(s) → `POST .../finalize` →
   `200 UploadResult` with a real CID → the content renders (`/blob/{cid}`; `/i/{cid}` for images,
   the latter already proven by m5). A `HEAD`-probed resume continues from the server offset. (The
   Vitest suite proves the JS orchestration fires `finalize` after transport-success and surfaces the
   CID via `onComplete`.)
3. `getToken()` is resolved per request: a token rotated mid-upload is honored; `null` ⇒ no
   `Authorization` header (no-token upload succeeds under the public-uploads floor; `401` when the
   floor is off and no token is supplied).
4. `NovaUploadWidget.mount` returns a working handle with `destroy()`; `mountAll` / auto-bootstrap
   mount **only** `data-nova-upload-widget` elements (reading `data-*` config); double-mounting one
   element returns the existing instance (no duplicate uploads); `destroy()` tears the instance down.
5. The integration is a single `<script>` tag (CSS injected at runtime); Uppy + tus + StatusBar are
   bundled locally; the deprecated `@uppy/progress-bar` is not used.
6. `web/widget` is a root workspace; `make widget-{build,lint,test}` + `hermetic-widget` and the CI
   lane are green; the upload openapi contract is unchanged (no new API path).

## Testing strategy

### Frontend (Vitest, jsdom)

- **`uploader` (the boundary):** with a mocked tus/Uppy lifecycle and a mocked `fetch`, an
  `upload-success` triggers a `POST {uploadURL}/finalize` carrying the bearer, and `onComplete` fires
  with the parsed `UploadResult` (and **not** on `upload-success` alone); a non-2xx finalize maps to
  `onError(code)` for each documented code (413/400/422/451/409/401).
- **`auth`:** `resolveToken` is awaited per request; a `getToken` that returns a *different* token on
  successive calls is honored (the rotation case); `null`/empty ⇒ no `Authorization` header;
  `token: t` behaves as `getToken: () => t` and `getToken` wins when both are set.
- **`index`/multi-mount:** `mount` returns a handle; a second `mount` on the same element returns the
  existing handle (WeakMap dedupe, no second Uppy); `destroy()` removes listeners + registry entry;
  `mountAll` mounts **only** `data-nova-upload-widget` elements and parses
  `data-endpoint`/`data-product`/`data-collection`; a non-marked element is never mounted.
- **`config`:** option normalization + `data-*` parsing (defaults, UUID collection, endpoint
  override).

### Go (unit)

- **`widget_static`:** asset vs demo `index.html` resolution; CSP + `X-Content-Type-Options` headers;
  caching (`immutable` hashed asset, `no-cache` entry JS, `no-store` demo HTML); `404` for an unknown
  `/widget/...` path (no SPA fallback); `NOVA_WIDGET_DIST_DIR` unset ⇒ handler nil ⇒ `/widget/*`
  `404`.

### Integration (`internal/integration/m12_upload_widget_test.go`, nginx-fronted, testcontainers)

Boot the coordinator with `WidgetDistDir` pointed at a freshly built `web/widget/dist` + nginx
(`location /widget` mirroring `location /admin`, plus the existing upload location), then exercise the
surface the widget depends on **end-to-end through nginx** (mirroring M11's
`startCoordinatorWithNginxCfg` / `m6Login` / `doJSONAuth` / `seedAuthUser`):

1. **Serving (Exit #1):** `GET /widget/` → `200` demo `index.html` with strict CSP;
   `GET /widget/nova-upload-widget.js` → `200` JS; the bundle references no external origin (assert
   against `dist` / `make hermetic-widget`); coordinator booted with `WidgetDistDir=""` → `/widget/`
   `404`.
2. **Upload lifecycle (Exit #2):** authenticated (uploader role) `POST /api/v1/uploads`
   (`Upload-Metadata`, `product=raw`, `text/plain`) → `PATCH` the bytes → `POST .../finalize` →
   `200 UploadResult`; `GET /blob/{cid}` returns the bytes. A second run that `HEAD`s the session and
   resumes from the server offset finalizes to a CID. (Image upload + `/i/{cid}` rendering is the
   M5-proven path; M12's integration uses a raw blob to prove the tus→finalize boundary without a
   libvips/JPEG dependency.)
3. **Token/floor (Exit #3):** the coordinator boots with the public-uploads floor **off**, so the
   uploader-token lifecycle above exercises the widget's authenticated `getToken` path and a no-token
   `POST /api/v1/uploads` → `401`. (The anonymous-floor happy path — no token under
   `NOVA_PUBLIC_UPLOADS=true` — is the M4/M5-proven path.)

(The integration test exercises the server-side lifecycle the widget performs over HTTP — like M11, it
does not drive a headless browser; the JS orchestration is proven in Vitest. The two layers together
cover the transport→finalize boundary from both sides.)

### CI

- New widget steps (extending `web-admin` or a sibling `web-widget` job): `make widget-lint &&
  widget-test && widget-build && hermetic-widget`.
- `web/widget` added to root `workspaces`; `package-lock.json` regenerated; `npm ci` green on Node 20.
- `-short`-skippable integration like M2–M11; gofmt only the Go files M12 touches (toolchain-skew
  rule); `golangci-lint`/`eslint` are CI-side.

## File structure

### Created in M12

```
web/widget/package.json                         @nova/widget; Uppy 3.x + Vite 4.5/Vitest 0.34 pins; build/lint/test scripts
web/widget/vite.config.ts                       library mode (IIFE, NovaUploadWidget, stable entry); CSS-injected; base '/widget/'
web/widget/tsconfig.json                         mirrors web/admin (ES2020, bundler, strict)
web/widget/public/index.html                     demo/example host page (Vite copies public/ → dist/; served at /widget/)
web/widget/src/index.ts                          NovaUploadWidget.{mount,mountAll}; auto-bootstrap; WeakMap registry
web/widget/src/widget.ts                         per-instance Uppy(core)+DragDrop+StatusBar; mount/destroy
web/widget/src/uploader.ts                       @uppy/tus config + the Nova finalize orchestrator
web/widget/src/auth.ts                           per-request getToken resolution; token→getToken sugar
web/widget/src/config.ts                         option normalization + data-* parsing
web/widget/src/api/types.ts                      UploadResult, ErrorBody (tracked to openapi)
web/widget/src/widget.css                        brand-tokened Uppy overrides
web/widget/src/*.test.ts                         Vitest: uploader (boundary), auth, multi-mount, config
internal/api/handlers/widget_static.go           /widget/* static (dist + demo; CSP; caching; 404 when unset; no SPA fallback)
internal/api/handlers/widget_static_test.go
internal/integration/m12_upload_widget_test.go   nginx-fronted end-to-end (the exit criteria)
docs/superpowers/specs/phase1/2026-06-07-phase1-m12-upload-widget-design.md   (this file)
docs/superpowers/plans/phase1/2026-06-07-phase1-m12-upload-widget.md          (the implementation plan)
```

### Modified in M12

```
internal/api/server.go              mount /widget + /widget/* static; new ServerConfig.WidgetStatic field
pkg/coordinator/coordinator.go      WidgetConfig struct + Config.Widget; build WidgetStatic handler (sibling to AdminSPAConfig)
cmd/coordinator/main.go             NOVA_WIDGET_DIST_DIR → coordinator.WidgetConfig
Makefile                            widget-install/build/lint/test, hermetic-widget, web aggregate
.github/workflows/ci.yml            widget lane (lint/test/build/hermetic-widget)
scripts/hermetic-spa.sh             generalize wording admin→bundle (dir already parametrized)
package.json                        re-add web/widget to workspaces
package-lock.json                   regenerated for the new workspace
nginx/nova.conf.example             add a `location /widget` proxy note (mirrors `location /admin`)
docs/ROADMAP.md                     M12 status + tag + deferrals (reconciliation #2)
docs/THREAT_MODEL.md                hermetic widget bundle + same-origin embedding / CORS-deferred (reconciliation #3)
docs/legal/OPERATOR_CHECKLIST.md    widget runbook: NOVA_WIDGET_DIST_DIR, embed snippet, getToken, floor, CORS caveat (reconciliation #4)
docs/specs/openapi.yaml             note-only: /widget/* static surface; upload contract unchanged (reconciliation #1)
docs/superpowers/specs/phase1/2026-05-25-phase1-single-node-mvp-design.md   M12 status/links
```

### Reused unchanged

```
internal/api/handlers/upload.go     the tus + finalize contract the widget drives (no change)
internal/upload/*                   the session store / committer (no change)
internal/api/handlers/admin_spa.go  the static-serving pattern widget_static.go mirrors
scripts/hermetic-spa.sh             the dir-parametrized hermetic gate (reused for web/widget/dist)
nginx/nova.conf.example             upload location (proxy_request_buffering off) + the CSP already covering the widget
internal/integration (harness)      startCoordinatorWithNginxCfg, blobfixture, dbtest, m6Login/doJSONAuth/seedAuthUser
web/admin/{package.json,vite.config.ts,tsconfig.json}   the build conventions the widget mirrors (library-mode divergence aside)
```

## Risks and notes

- **The finalize boundary is the long pole, not the drag-and-drop UI (sequence accordingly).** Land
  and prove `uploader.ts` (finalize-after-transport, per-request auth, error mapping) before UI
  polish. The DragDrop/StatusBar wiring is comparatively mechanical; the boundary is where regressions
  hide, which is why both Vitest and the Go integration test target it.
- **Uppy/tus completion semantics (decided).** The widget deliberately does **not** rely on tus
  "success" as user-visible completion. Anyone extending the widget must preserve "success = transport,
  finalize = done"; surfacing a CID before finalize would be a correctness bug.
- **Same-origin Phase-1 (accepted, documented).** No CORS ships. If an operator embeds cross-origin
  today they must add CORS at their reverse proxy; first-class CORS waits for M13's host split. The
  doc and runbook state this plainly so the constraint is not a surprise.
- **CSS-inject build dependency (bounded).** Single-`<script>` embedding needs the CSS injected into
  the JS (`vite-plugin-css-injected-by-js`, a build-time dev dep). It is not a runtime asset and does
  not weaken hermeticity; if it ever complicates the Node-16 build, the fallback is a sibling
  `nova-upload-widget.css` the demo + docs link — **single-tag is the goal, local-only is the
  invariant.**
- **Uppy 3.x pin (decided).** Uppy 3.x is the Node-16-safe line matching `web/admin`'s Vite/Vitest
  pins; Uppy 4.x assumes newer toolchains. The pin is revisited in M12+ alongside the wider Node-20
  workspace move, not now.
- **Public-uploads floor interaction (accepted).** The zero-JS `data-nova-upload-widget` path works
  only when the operator has enabled the public-uploads floor; authenticated embeds use the
  constructor's `getToken`. The runbook makes the dependency explicit so an operator does not ship a
  zero-JS embed against an auth-gated upload path and see silent `401`s.
- **Demo page is a deliverable, not polish (accepted).** Phase-1 defines widget success in terms of a
  test HTML page; the served demo (`/widget/`) is the deterministic place that proves
  drag-drop → resumable transport → finalize → CID → render, and the integration test boots against it.

## Cross-references

- `docs/ROADMAP.md` M12 row + `docs/superpowers/specs/phase1/2026-05-25-phase1-single-node-mvp-design.md`
  § "Walking-skeleton milestone breakdown" (M12) + the upload data-flow (`:388`–`:399`) — the
  committed surface and the `web/widget` layout.
- `docs/specs/openapi.yaml` `/api/v1/uploads*` + `UploadResult` — the tus + finalize contract the
  widget drives (the openapi already names the widget as its consumer).
- `internal/api/handlers/upload.go` — `CreateTus`/`HeadTus`/`PatchTus`/`DeleteTus`/`FinalizeTus`,
  `writeUploadResult`, `writePutError` (the result shape + error vocabulary the widget consumes).
- M11 design (`2026-06-04-phase1-m11-admin-spa-design.md`) — the hermetic-build discipline, the
  static-serving seam (`admin_spa.go` / `NOVA_ADMIN_DIST_DIR`), the `hermetic-spa` gate, and the
  nginx-fronted integration pattern M12 mirrors.
- M6 design (`2026-05-30-phase1-m6-auth-design.md`) — the 15-minute access-token TTL + rotation that
  motivates per-request `getToken`; the resource-server boundary the widget relies on.
- `nginx/nova.conf.example` — the upload location (`proxy_request_buffering off`) and the CSP already
  declaring the widget bundle hermetic; the `location /admin` proxy the test mirrors for `/widget`.
- `docs/superpowers/plans/phase1/2026-06-07-phase1-m12-upload-widget.md` — the implementation plan.
```
