import { defineConfig } from 'vitest/config'
import react from '@vitejs/plugin-react'

// Hermetic admin SPA build (M11). base '/admin/' so hashed assets resolve behind
// the coordinator's /admin/* mount (and M13's nginx). No external CDN: IBM Plex is
// self-hosted via @fontsource and bundled. The `hermetic-spa` CI gate greps dist
// for external origins. Dev proxies the API to a local coordinator on :9000.
export default defineConfig({
  base: '/admin/',
  plugins: [react()],
  build: {
    outDir: 'dist',
    sourcemap: false,
    chunkSizeWarningLimit: 1500,
  },
  server: {
    port: 5173,
    proxy: {
      '/api': 'http://127.0.0.1:9000',
      '/blob': 'http://127.0.0.1:9000',
      '/i': 'http://127.0.0.1:9000',
      '/legal': 'http://127.0.0.1:9000',
    },
  },
  test: {
    environment: 'jsdom',
    setupFiles: ['./src/test/setup.ts'],
    css: true,
  },
})
