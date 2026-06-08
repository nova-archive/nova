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
