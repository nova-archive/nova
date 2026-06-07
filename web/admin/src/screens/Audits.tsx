import { useState } from 'react'
import { useAuth } from '../auth/AuthProvider'
import { usePaged } from '../lib/useList'
import { formatTime, shortCid } from '../lib/format'
import type { IntegrityAudit } from '../api/types'
import {
  Banner,
  Button,
  type Column,
  Copy,
  DataTable,
  Empty,
  Field,
  Mono,
  Muted,
  Page,
  PageHeader,
  Pagination,
  Select,
  Spacer,
  StateChip,
  Toolbar,
} from '../ui/ui'

const RESULT_OPTS = [
  { value: 'fail', label: 'fail' },
  { value: '', label: 'all' },
  { value: 'pass', label: 'pass' },
  { value: 'skip', label: 'skip' },
]
const KIND_OPTS = [
  { value: '', label: 'All kinds' },
  ...[
    'envelope_decode',
    'key_unwrap',
    'sample_decrypt',
    'kubo_pin_present',
    'derivative_state_consistent',
    'block_hash_valid',
    'manifest_consistent',
  ].map((v) => ({ value: v, label: v })),
]

export function AuditsScreen() {
  const { api } = useAuth()
  const [result, setResult] = useState('fail')
  const [kind, setKind] = useState('')
  const { query, page, setPage, perPage } = usePaged<IntegrityAudit>(
    'audits',
    api.integrityAudits,
    { result, audit_kind: kind },
  )

  const columns: Array<Column<IntegrityAudit>> = [
    {
      key: 'cid',
      header: 'CID',
      render: (a) => (
        <>
          <Mono title={a.cid}>{shortCid(a.cid)}</Mono>
          <Copy value={a.cid} />
        </>
      ),
    },
    { key: 'kind', header: 'Audit', render: (a) => <Mono>{a.audit_kind}</Mono> },
    { key: 'result', header: 'Result', render: (a) => <StateChip kind="audit" value={a.result} /> },
    {
      key: 'error',
      header: 'Detail',
      render: (a) => (a.error ? <Mono title={a.error}>{a.error}</Mono> : <Muted>—</Muted>),
    },
    { key: 'at', header: 'Audited', render: (a) => <Muted>{formatTime(a.audited_at)}</Muted> },
  ]

  return (
    <Page>
      <PageHeader
        kicker="Fixity"
        title="Integrity audits"
        sub="Local integrity-audit results across the seven check kinds. Failures surface here for operator decision — they are never auto-remediated."
      />
      <Toolbar>
        <Field label="Result">
          <Select value={result} onChange={(e) => setResult(e.target.value)} options={RESULT_OPTS} />
        </Field>
        <Field label="Kind">
          <Select value={kind} onChange={(e) => setKind(e.target.value)} options={KIND_OPTS} />
        </Field>
        <Spacer />
        <Button size="sm" onClick={() => void query.refetch()}>
          Refresh
        </Button>
      </Toolbar>

      {query.isError && <Banner>Failed to load integrity audits.</Banner>}

      <DataTable
        columns={columns}
        rows={query.data?.data ?? []}
        getKey={(a) => String(a.id)}
        loading={query.isLoading}
        empty={<Empty title="No matching audits" hint="No failures — the archive is healthy." />}
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
