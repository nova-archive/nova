import '@testing-library/jest-dom/vitest'
import { webcrypto } from 'node:crypto'

// jsdom on Node 16 has getRandomValues but not crypto.subtle. Provide Node's
// webcrypto where subtle is missing so wizard crypto helpers (Task 10) have it.
if (!globalThis.crypto || !('subtle' in globalThis.crypto)) {
  Object.defineProperty(globalThis, 'crypto', {
    value: webcrypto,
    configurable: true,
    writable: true,
  })
}
