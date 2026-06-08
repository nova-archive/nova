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
