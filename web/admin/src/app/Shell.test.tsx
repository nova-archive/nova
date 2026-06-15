import { render, screen } from '@testing-library/react'
import { MemoryRouter } from 'react-router-dom'
import { describe, expect, it, vi } from 'vitest'

// Shell pulls the current user from the auth context; stub it so the nav
// renders (operator sees every entry, including the operator-only Keys link).
vi.mock('../auth/AuthProvider', () => ({
  useAuth: () => ({ user: { email: 'op@example.org', role: 'operator' }, logout: vi.fn() }),
}))

import { Shell } from './Shell'

// Regression guard for the M0.1/B1 double-prefix bug: with
// <BrowserRouter basename="/admin">, NavLink `to` values are resolved
// RELATIVE to the basename. If a nav entry hardcodes `/admin/...`, react-router
// prepends the basename again → `/admin/admin/...`, which matches no route and
// dumps the operator back on the catch-all (Blobs). Every nav target must
// resolve to exactly one `/admin` segment.
describe('Shell navigation', () => {
  it('resolves nav links under the /admin basename without double-prefixing', () => {
    render(
      <MemoryRouter basename="/admin" initialEntries={['/admin/blobs']}>
        <Shell />
      </MemoryRouter>,
    )

    const expected: Record<string, string> = {
      Blobs: '/admin/blobs',
      Moderation: '/admin/moderation',
      Integrity: '/admin/audits',
      Keys: '/admin/keys',
      Settings: '/admin/settings',
      Jobs: '/admin/jobs',
      'Audit log': '/admin/audit-log',
    }

    for (const [label, href] of Object.entries(expected)) {
      const link = screen.getByRole('link', { name: new RegExp(label) })
      expect(link.getAttribute('href')).toBe(href)
    }
  })
})
