import { defineConfig } from 'vitest/config'
import react from '@vitejs/plugin-react'

// Hermetic setup-wizard SPA build (M13). base '/setup/' so hashed assets resolve
// behind the coordinator's /setup/* mount during first-run (and M13's nginx). No
// external CDN: IBM Plex is self-hosted via @fontsource and bundled. The
// `hermetic-spa` CI gate greps dist for external origins. Dev proxies the API to
// a local coordinator on :9000.
export default defineConfig({
  base: '/setup/',
  plugins: [react()],
  build: {
    outDir: 'dist',
    sourcemap: false,
    chunkSizeWarningLimit: 1500,
  },
  server: {
    port: 5174,
    proxy: {
      '/setup': 'http://127.0.0.1:9000',
    },
  },
  test: {
    environment: 'jsdom',
    setupFiles: ['./src/test/setup.ts'],
    css: true,
  },
})
