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
