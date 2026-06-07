import '@testing-library/jest-dom/vitest'
import { webcrypto } from 'node:crypto'

// jsdom on Node 16 has getRandomValues but not crypto.subtle; the PKCE helpers
// need SHA-256. Provide Node's webcrypto where subtle is missing.
if (!globalThis.crypto || !('subtle' in globalThis.crypto)) {
  Object.defineProperty(globalThis, 'crypto', {
    value: webcrypto,
    configurable: true,
    writable: true,
  })
}
