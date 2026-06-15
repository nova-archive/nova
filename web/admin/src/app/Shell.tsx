import { NavLink, Outlet } from 'react-router-dom'
import { useAuth } from '../auth/AuthProvider'
import type { Role } from '../api/types'
import { Button, Chip, cls } from '../ui/ui'
import s from './Shell.module.css'

interface NavEntry {
  to: string
  label: string
  glyph: string
  roles?: Role[]
}

// Paths are relative to the router basename ("/admin"); react-router prepends
// it when resolving NavLink hrefs. Hardcoding "/admin/…" here would
// double-prefix to "/admin/admin/…" (matches no route → catch-all → Blobs).
const NAV: NavEntry[] = [
  { to: '/blobs', label: 'Blobs', glyph: '◆' },
  { to: '/moderation', label: 'Moderation', glyph: '⚑' },
  { to: '/audits', label: 'Integrity', glyph: '◇' },
  { to: '/keys', label: 'Keys', glyph: '⚿', roles: ['operator'] },
  { to: '/settings', label: 'Settings', glyph: '⚙', roles: ['operator'] },
  { to: '/jobs', label: 'Jobs', glyph: '↻' },
  { to: '/audit-log', label: 'Audit log', glyph: '☰' },
]

export function Shell() {
  const { user, logout } = useAuth()
  const role = user?.role
  const visible = NAV.filter((n) => !n.roles || (role && n.roles.includes(role)))

  return (
    <div className={s.shell}>
      <aside className={s.rail}>
        <div className={s.brand}>
          <span className={s.mark}>nova</span>
          <span className={s.brandTag}>console</span>
        </div>
        <nav className={s.nav}>
          <div className={s.navSection}>Operations</div>
          {visible.map((n) => (
            <NavLink
              key={n.to}
              to={n.to}
              className={({ isActive }) => cls(s.navItem, isActive && s.navActive)}
            >
              <span className={s.navGlyph}>{n.glyph}</span>
              {n.label}
            </NavLink>
          ))}
        </nav>
        <div className={s.railFoot}>Nova · operator surface</div>
      </aside>

      <div className={s.right}>
        <header className={s.header}>
          {user && (
            <>
              <div className={s.who}>
                <div className={s.whoEmail}>{user.email}</div>
                <div className={s.whoRole}>{user.role}</div>
              </div>
              <Chip tone={user.role === 'operator' ? 'nova' : 'slate'}>{user.role}</Chip>
              <Button size="sm" variant="ghost" onClick={() => void logout()}>
                Sign out
              </Button>
            </>
          )}
        </header>
        <main className={s.main}>
          <div className={s.mainInner}>
            <Outlet />
          </div>
        </main>
      </div>
    </div>
  )
}
