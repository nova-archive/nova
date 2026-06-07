import { useState } from 'react'
import { useAuth } from '../auth/AuthProvider'
import { usePaged } from '../lib/useList'
import { formatTime } from '../lib/format'
import type { Job } from '../api/types'
import {
  Banner,
  Button,
  type Column,
  DataTable,
  Empty,
  Field,
  Mono,
  Muted,
  Num,
  Page,
  PageHeader,
  Pagination,
  Select,
  Spacer,
  StateChip,
  TextInput,
  Toolbar,
} from '../ui/ui'

const STATE_OPTS = [
  { value: '', label: 'All states' },
  ...['pending', 'leased', 'completed', 'failed', 'dead'].map((v) => ({ value: v, label: v })),
]

export function JobsScreen() {
  const { api } = useAuth()
  const [state, setState] = useState('')
  const [kind, setKind] = useState('')
  const { query, page, setPage, perPage } = usePaged<Job>('jobs', api.listJobs, {
    state,
    kind: kind.trim(),
  })

  const columns: Array<Column<Job>> = [
    { key: 'kind', header: 'Kind', render: (j) => <Mono>{j.kind}</Mono> },
    { key: 'state', header: 'State', render: (j) => <StateChip kind="job" value={j.state} /> },
    {
      key: 'attempts',
      header: 'Attempts',
      align: 'right',
      render: (j) => (
        <Num>
          {j.attempts}/{j.max_attempts}
        </Num>
      ),
    },
    {
      key: 'error',
      header: 'Last error',
      render: (j) =>
        j.last_error ? (
          <Mono title={j.last_error}>
            {j.last_error.length > 52 ? j.last_error.slice(0, 52) + '…' : j.last_error}
          </Mono>
        ) : (
          <Muted>—</Muted>
        ),
    },
    { key: 'created', header: 'Created', render: (j) => <Muted>{formatTime(j.created_at)}</Muted> },
  ]

  return (
    <Page>
      <PageHeader
        kicker="Background"
        title="Jobs"
        sub="Queue introspection — stuck, failed, and recent work. Read-only."
      />
      <Toolbar>
        <Field label="State">
          <Select value={state} onChange={(e) => setState(e.target.value)} options={STATE_OPTS} />
        </Field>
        <Field label="Kind">
          <TextInput
            placeholder="derivative_prewarm…"
            value={kind}
            onChange={(e) => setKind(e.target.value)}
          />
        </Field>
        <Spacer />
        <Button size="sm" onClick={() => void query.refetch()}>
          Refresh
        </Button>
      </Toolbar>

      {query.isError && <Banner>Failed to load jobs.</Banner>}

      <DataTable
        columns={columns}
        rows={query.data?.data ?? []}
        getKey={(j) => j.id}
        loading={query.isLoading}
        empty={<Empty title="No jobs" hint="The queue is quiet." />}
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
