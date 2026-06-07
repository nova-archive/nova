import { useState } from 'react'
import { useAuth } from '../auth/AuthProvider'
import { usePaged } from '../lib/useList'
import { formatTime, shortCid } from '../lib/format'
import type { AuditLogEntry } from '../api/types'
import {
  Banner,
  Button,
  type Column,
  DataTable,
  Empty,
  Field,
  Mono,
  Muted,
  Page,
  PageHeader,
  Pagination,
  Spacer,
  TextInput,
  Toolbar,
} from '../ui/ui'

export function AuditLogScreen() {
  const { api } = useAuth()
  const [action, setAction] = useState('')
  const { query, page, setPage, perPage } = usePaged<AuditLogEntry>('audit-log', api.auditLog, {
    action: action.trim(),
  })

  const columns: Array<Column<AuditLogEntry>> = [
    { key: 'action', header: 'Action', render: (e) => <Mono>{e.action}</Mono> },
    {
      key: 'target',
      header: 'Target',
      render: (e) => (
        <Mono title={e.target_id}>
          {e.target_type}:{shortCid(e.target_id, 8, 6)}
        </Mono>
      ),
    },
    {
      key: 'actor',
      header: 'Actor',
      render: (e) =>
        e.actor_id ? <Mono title={e.actor_id}>{shortCid(e.actor_id, 8, 4)}</Mono> : <Muted>system</Muted>,
    },
    { key: 'at', header: 'When', render: (e) => <Muted>{formatTime(e.created_at)}</Muted> },
  ]

  return (
    <Page>
      <PageHeader
        kicker="Forensics"
        title="Audit log"
        sub="Append-only record of privileged operator actions (never deleted)."
      />
      <Toolbar>
        <Field label="Action">
          <TextInput
            placeholder="e.g. blob.tombstoned"
            value={action}
            onChange={(e) => setAction(e.target.value)}
          />
        </Field>
        <Spacer />
        <Button size="sm" onClick={() => void query.refetch()}>
          Refresh
        </Button>
      </Toolbar>

      {query.isError && <Banner>Failed to load the audit log.</Banner>}

      <DataTable
        columns={columns}
        rows={query.data?.data ?? []}
        getKey={(e) => e.id}
        loading={query.isLoading}
        empty={<Empty title="No entries" />}
      />
      <Pagination
        page={page}
        perPage={perPage}
        total={query.data?.pagination.total ?? 0}
        onPage={setPage}
      />
    </Page>
  )
}
