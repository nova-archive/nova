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
