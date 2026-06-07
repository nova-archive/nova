import { useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { useAuth } from '../auth/AuthProvider'
import { usePaged } from '../lib/useList'
import { formatBytes, formatTime, shortCid } from '../lib/format'
import type { AdminBlob } from '../api/types'
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
  { value: 'active', label: 'active' },
  { value: 'soft_deleted', label: 'soft_deleted' },
  { value: 'quarantined', label: 'quarantined' },
  { value: 'tombstoned', label: 'tombstoned' },
]
const PRODUCT_OPTS = [
  { value: '', label: 'All products' },
  { value: 'image', label: 'image' },
  { value: 'raw', label: 'raw' },
  { value: 'video', label: 'video' },
  { value: 'audio', label: 'audio' },
  { value: 'document', label: 'document' },
  { value: 'archive', label: 'archive' },
]

export function BlobsScreen() {
  const { api } = useAuth()
  const nav = useNavigate()
  const [state, setState] = useState('')
  const [product, setProduct] = useState('')
  const [owner, setOwner] = useState('')

  const { query, page, setPage, perPage } = usePaged<AdminBlob>('blobs', api.listBlobs, {
    state,
    product,
    owner_id: owner.trim(),
  })

  const columns: Array<Column<AdminBlob>> = [
    {
      key: 'cid',
      header: 'CID',
      render: (b) => (
        <>
          <Mono title={b.cid}>{shortCid(b.cid)}</Mono>
          <Copy value={b.cid} />
        </>
      ),
    },
    { key: 'product', header: 'Product', render: (b) => <Mono>{b.product}</Mono> },
    { key: 'state', header: 'State', render: (b) => <StateChip kind="blob" value={b.state} /> },
    {
      key: 'size',
      header: 'Size',
      align: 'right',
      render: (b) => <Num>{formatBytes(b.byte_size)}</Num>,
    },
    {
      key: 'owner',
      header: 'Owner',
      render: (b) =>
        b.owner_id ? <Mono title={b.owner_id}>{shortCid(b.owner_id, 8, 4)}</Mono> : <Muted>—</Muted>,
    },
    { key: 'uploaded', header: 'Uploaded', render: (b) => <Muted>{formatTime(b.uploaded_at)}</Muted> },
  ]

  return (
    <Page>
      <PageHeader
        kicker="Storage"
        title="Blobs"
        sub="Every object in the archive — browse, inspect, and soft-delete."
      />
      <Toolbar>
        <Field label="State">
          <Select value={state} onChange={(e) => setState(e.target.value)} options={STATE_OPTS} />
        </Field>
        <Field label="Product">
          <Select
            value={product}
            onChange={(e) => setProduct(e.target.value)}
            options={PRODUCT_OPTS}
          />
        </Field>
        <Field label="Owner ID">
          <TextInput placeholder="uuid…" value={owner} onChange={(e) => setOwner(e.target.value)} />
        </Field>
        <Spacer />
        <Button size="sm" onClick={() => void query.refetch()}>
          Refresh
        </Button>
      </Toolbar>

      {query.isError && <Banner>Failed to load blobs.</Banner>}

      <DataTable
        columns={columns}
        rows={query.data?.data ?? []}
        getKey={(b) => b.cid}
        loading={query.isLoading}
        onRowClick={(b) => nav(`/blobs/${encodeURIComponent(b.cid)}`)}
        empty={<Empty title="No blobs match" hint="Adjust the filters above." />}
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
