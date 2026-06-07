import { useState, type ButtonHTMLAttributes, type ReactNode } from 'react'
import s from './ui.module.css'

export function cls(...xs: Array<string | false | undefined | null>): string {
  return xs.filter(Boolean).join(' ')
}

// --- Page scaffold ----------------------------------------------------------

export function Page({ children }: { children: ReactNode }) {
  return <div className={s.page}>{children}</div>
}

export function PageHeader({
  kicker,
  title,
  sub,
  actions,
}: {
  kicker?: string
  title: string
  sub?: ReactNode
  actions?: ReactNode
}) {
  return (
    <header className={s.pageHead}>
      <div>
        {kicker && <div className={s.pageKicker}>{kicker}</div>}
        <h1 className={s.pageTitle}>{title}</h1>
        {sub && <div className={s.pageSub}>{sub}</div>}
      </div>
      {actions && <div className={s.headActions}>{actions}</div>}
    </header>
  )
}

export function Card({ children, pad = true }: { children: ReactNode; pad?: boolean }) {
  return <div className={cls(s.card, pad && s.cardPad)}>{children}</div>
}

// --- Button ------------------------------------------------------------------

type Variant = 'default' | 'primary' | 'danger' | 'ghost'

export function Button({
  variant = 'default',
  size,
  className,
  ...rest
}: ButtonHTMLAttributes<HTMLButtonElement> & { variant?: Variant; size?: 'sm' }) {
  const v = variant === 'primary' ? s.primary : variant === 'danger' ? s.danger : variant === 'ghost' ? s.ghost : ''
  return <button className={cls(s.btn, v, size === 'sm' && s.sm, className)} {...rest} />
}

// --- Inputs ------------------------------------------------------------------

export function Toolbar({ children }: { children: ReactNode }) {
  return <div className={s.toolbar}>{children}</div>
}
export function Spacer() {
  return <div className={s.spacer} />
}

export function Field({ label, children }: { label: string; children: ReactNode }) {
  return (
    <label className={s.field}>
      <span className={s.fieldLabel}>{label}</span>
      {children}
    </label>
  )
}

export function TextInput(props: React.InputHTMLAttributes<HTMLInputElement>) {
  return <input className={s.input} {...props} />
}

export function Select({
  options,
  ...props
}: React.SelectHTMLAttributes<HTMLSelectElement> & { options: Array<{ value: string; label: string }> }) {
  return (
    <select className={s.select} {...props}>
      {options.map((o) => (
        <option key={o.value} value={o.value}>
          {o.label}
        </option>
      ))}
    </select>
  )
}

// --- Mono / copy / chips -----------------------------------------------------

export function Mono({ children, title }: { children: ReactNode; title?: string }) {
  return (
    <span className={s.mono} title={title}>
      {children}
    </span>
  )
}

export function Muted({ children }: { children: ReactNode }) {
  return <span className={s.muted}>{children}</span>
}

export function Num({ children }: { children: ReactNode }) {
  return <span className={s.num}>{children}</span>
}

export function Copy({ value }: { value: string }) {
  const [done, setDone] = useState(false)
  return (
    <button
      className={s.copy}
      title="Copy"
      onClick={(e) => {
        e.stopPropagation()
        void navigator.clipboard?.writeText(value)
        setDone(true)
        setTimeout(() => setDone(false), 1200)
      }}
    >
      {done ? '✓' : 'copy'}
    </button>
  )
}

type Tone = 'ok' | 'nova' | 'slate' | 'danger' | 'warn' | 'mute'
const toneClass: Record<Tone, string> = {
  ok: s.chipOk,
  nova: s.chipNova,
  slate: s.chipSlate,
  danger: s.chipDanger,
  warn: s.chipWarn,
  mute: s.chipMute,
}

export function Chip({ tone, children }: { tone: Tone; children: ReactNode }) {
  return <span className={cls(s.chip, toneClass[tone])}>{children}</span>
}

const blobTone: Record<string, Tone> = {
  active: 'ok',
  soft_deleted: 'warn',
  quarantined: 'nova',
  tombstoned: 'mute',
}
const jobTone: Record<string, Tone> = {
  pending: 'slate',
  leased: 'slate',
  completed: 'ok',
  failed: 'danger',
  dead: 'danger',
}
const auditTone: Record<string, Tone> = { pass: 'ok', fail: 'danger', skip: 'mute' }

export function StateChip({ kind, value }: { kind: 'blob' | 'job' | 'audit'; value: string }) {
  const map = kind === 'blob' ? blobTone : kind === 'job' ? jobTone : auditTone
  return <Chip tone={map[value] ?? 'mute'}>{value}</Chip>
}

// --- Table -------------------------------------------------------------------

export interface Column<T> {
  key: string
  header: string
  render: (row: T) => ReactNode
  align?: 'right'
}

export function DataTable<T>({
  columns,
  rows,
  getKey,
  onRowClick,
  loading,
  empty,
}: {
  columns: Array<Column<T>>
  rows: T[]
  getKey: (row: T) => string
  onRowClick?: (row: T) => void
  loading?: boolean
  empty?: ReactNode
}) {
  return (
    <div className={s.tableWrap}>
      <table className={s.table}>
        <thead>
          <tr>
            {columns.map((c) => (
              <th key={c.key} style={c.align === 'right' ? { textAlign: 'right' } : undefined}>
                {c.header}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {loading ? (
            <tr>
              <td colSpan={columns.length}>
                <div className={s.loadingRow}>
                  <span className={s.spinner} /> Loading…
                </div>
              </td>
            </tr>
          ) : rows.length === 0 ? (
            <tr>
              <td colSpan={columns.length}>{empty ?? <Empty title="Nothing here yet" />}</td>
            </tr>
          ) : (
            rows.map((row) => (
              <tr
                key={getKey(row)}
                className={onRowClick ? s.rowClickable : undefined}
                onClick={onRowClick ? () => onRowClick(row) : undefined}
              >
                {columns.map((c) => (
                  <td key={c.key} style={c.align === 'right' ? { textAlign: 'right' } : undefined}>
                    {c.render(row)}
                  </td>
                ))}
              </tr>
            ))
          )}
        </tbody>
      </table>
    </div>
  )
}

// --- States ------------------------------------------------------------------

export function Empty({ title, hint }: { title: string; hint?: ReactNode }) {
  return (
    <div className={s.empty}>
      <div className={s.emptyTitle}>{title}</div>
      {hint && <div>{hint}</div>}
    </div>
  )
}

export function Banner({ tone = 'err', children }: { tone?: 'err' | 'warn'; children: ReactNode }) {
  return <div className={cls(s.banner, tone === 'warn' ? s.bannerWarn : s.bannerErr)}>{children}</div>
}

export function Pagination({
  page,
  perPage,
  total,
  onPage,
}: {
  page: number
  perPage: number
  total: number
  onPage: (p: number) => void
}) {
  const last = Math.max(1, Math.ceil(total / perPage))
  const from = total === 0 ? 0 : (page - 1) * perPage + 1
  const to = Math.min(total, page * perPage)
  return (
    <div className={s.pager}>
      <span className={s.pagerInfo}>
        {from}–{to} of {total}
      </span>
      <div className={s.pagerBtns}>
        <Button size="sm" disabled={page <= 1} onClick={() => onPage(page - 1)}>
          ← Prev
        </Button>
        <Button size="sm" disabled={page >= last} onClick={() => onPage(page + 1)}>
          Next →
        </Button>
      </div>
    </div>
  )
}

export function DefinitionList({ rows }: { rows: Array<[string, ReactNode]> }) {
  return (
    <dl className={s.dl}>
      {rows.map(([k, v], i) => (
        <div key={i} style={{ display: 'contents' }}>
          <dt>{k}</dt>
          <dd>{v}</dd>
        </div>
      ))}
    </dl>
  )
}
