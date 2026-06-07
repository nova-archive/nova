import { useState, type ReactNode } from 'react'
import { useNavigate, useParams } from 'react-router-dom'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useAuth } from '../auth/AuthProvider'
import { useToast } from '../ui/toast'
import { ApiError } from '../api/client'
import { formatBytes, formatTime } from '../lib/format'
import {
  Banner,
  Button,
  Card,
  Copy,
  DefinitionList,
  Mono,
  Muted,
  Page,
  PageHeader,
  StateChip,
} from '../ui/ui'
import { ConfirmDialog } from '../ui/Dialog'

export function BlobDetailScreen() {
  const { cid = '' } = useParams()
  const { api, user } = useAuth()
  const toast = useToast()
  const nav = useNavigate()
  const qc = useQueryClient()
  const [confirm, setConfirm] = useState(false)

  const q = useQuery({ queryKey: ['blob', cid], queryFn: () => api.getBlob(cid), enabled: !!cid })

  const del = useMutation({
    mutationFn: () => api.softDeleteBlob(cid),
    onSuccess: () => {
      toast.ok('Blob soft-deleted', cid)
      setConfirm(false)
      void qc.invalidateQueries({ queryKey: ['blob', cid] })
      void qc.invalidateQueries({ queryKey: ['blobs'] })
    },
    onError: (e: unknown) => {
      const msg =
        e instanceof ApiError
          ? e.code === 'not_active'
            ? 'Blob is not active.'
            : e.message
          : 'Soft-delete failed.'
      toast.err('Soft-delete failed', msg)
      setConfirm(false)
    },
  })

  const b = q.data
  const canDelete =
    !!b &&
    b.state === 'active' &&
    (user?.role === 'operator' || (!!b.owner_id && b.owner_id === user?.id))

  const rows: Array<[string, ReactNode]> = b
    ? [
        [
          'CID',
          <>
            <Mono>{b.cid}</Mono>
            <Copy value={b.cid} />
          </>,
        ],
        ['State', <StateChip kind="blob" value={b.state} />],
        ['Product', <Mono>{b.product}</Mono>],
        ['MIME', <Mono>{b.mime_type}</Mono>],
        ['Size', formatBytes(b.byte_size)],
        ['Owner', b.owner_id ? <Mono>{b.owner_id}</Mono> : <Muted>—</Muted>],
        ['Parent', b.parent_cid ? <Mono>{b.parent_cid}</Mono> : <Muted>original</Muted>],
        [
          'Derivative',
          b.derivative_preset ? (
            <Mono>
              {b.derivative_preset} · {b.derivative_format}
            </Mono>
          ) : (
            <Muted>—</Muted>
          ),
        ],
        ['Uploaded', formatTime(b.uploaded_at)],
        ['Soft-deleted', b.soft_deleted_at ? formatTime(b.soft_deleted_at) : <Muted>—</Muted>],
      ]
    : []

  return (
    <Page>
      <PageHeader
        kicker="Storage / Blob"
        title="Blob detail"
        sub={<Mono>{cid}</Mono>}
        actions={
          <>
            <Button onClick={() => nav(-1)}>← Back</Button>
            <Button variant="danger" disabled={!canDelete} onClick={() => setConfirm(true)}>
              Soft-delete
            </Button>
          </>
        }
      />

      {q.isError && <Banner>Blob not found or not accessible.</Banner>}

      {b && (
        <>
          {b.product === 'image' && b.state === 'active' && (
            <Card pad={false}>
              <img
                src={`/blob/${encodeURIComponent(b.cid)}`}
                alt={b.cid}
                style={{ display: 'block', maxWidth: '100%', maxHeight: 320, margin: '0 auto', padding: 18 }}
              />
            </Card>
          )}
          <Card>
            <DefinitionList rows={rows} />
          </Card>
        </>
      )}

      <ConfirmDialog
        open={confirm}
        onOpenChange={setConfirm}
        title="Soft-delete blob?"
        danger
        confirmLabel="Soft-delete"
        busy={del.isLoading}
        onConfirm={() => del.mutate()}
      >
        <p>
          The blob enters <strong>soft_deleted</strong> immediately (reads return <Mono>410</Mono>).
          After the grace window the lifecycle sweep <strong>tombstones</strong> it and{' '}
          <strong>crypto-shreds</strong> the encryption key — irreversible.
        </p>
        <p style={{ marginTop: 10 }}>
          <Mono>{cid}</Mono>
        </p>
      </ConfirmDialog>
    </Page>
  )
}
