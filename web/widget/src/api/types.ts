// Mirrors docs/specs/openapi.yaml UploadResult + Error. Kept minimal: the widget
// only consumes the upload surface.
export interface UploadResult {
  cid: string
  byte_size: number
  mime_type: string
  product?: string
  urls: { original: string; json: string; presets?: Record<string, string> }
}

export interface WidgetError {
  code: string
  message?: string
}
