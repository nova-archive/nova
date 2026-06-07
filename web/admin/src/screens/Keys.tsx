import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useAuth } from '../auth/AuthProvider'
import { useToast } from '../ui/toast'
import { ApiError } from '../api/client'
import { formatTime } from '../lib/format'
import type { VersionSummary } from '../api/types'
import {
  Banner,
  Button,
  Card,
  Chip,
  type Column,
  DataTable,
  Field,
  Mono,
  Muted,
  Num,
  Page,
  PageHeader,
  TextInput,
} from '../ui/ui'
import { ConfirmDialog, Modal } from '../ui/Dialog'

export function KeysScreen() {
  const { api, user } = useAuth()
  const toast = useToast()
  const qc = useQueryClient()
  const isOperator = user?.role === 'operator'

  const status = useQuery({
    queryKey: ['rotation-status'],
    queryFn: api.rotationStatus,
    refetchInterval: (data) => (data?.in_progress ? 2000 : false),
  })

  const [rotateOpen, setRotateOpen] = useState(false)
  const [fromV, setFromV] = useState('')
  const [toV, setToV] = useState('')
  const [signingOpen, setSigningOpen] = useState(false)
  const [grace, setGrace] = useState('86400')

  const rotateMaster = useMutation({
    mutationFn: () => api.rotateMaster({ from_version: fromV.trim(), to_version: toV.trim() }),
    onSuccess: () => {
      toast.ok('Master-key rotation started')
      setRotateOpen(false)
      void qc.invalidateQueries({ queryKey: ['rotation-status'] })
    },
    onError: (e: unknown) =>
      toast.err('Rotation failed', e instanceof ApiError ? e.message : 'unexpected error'),
  })

  const rotateSigning = useMutation({
    mutationFn: () => api.rotateSigning({ grace_seconds: Number(grace) || 86400 }),
    onSuccess: () => {
      toast.ok('Signing key rotated')
      setSigningOpen(false)
    },
    onError: (e: unknown) =>
      toast.err('Rotation failed', e instanceof ApiError ? e.message : 'unexpected error'),
  })

  const data = status.data
  const ip = data?.in_progress

  const columns: Array<Column<VersionSummary>> = [
    { key: 'label', header: 'Version', render: (v) => <Mono>{v.label}</Mono> },
    {
      key: 'state',
      header: 'State',
      render: (v) => (
        <Chip tone={v.state === 'active' ? 'ok' : v.state === 'rotating' ? 'nova' : 'mute'}>
          {v.state}
        </Chip>
      ),
    },
    { key: 'deks', header: 'DEKs', align: 'right', render: (v) => <Num>{v.dek_count}</Num> },
    {
      key: 'signing',
      header: 'Signing keys',
      align: 'right',
      render: (v) => <Num>{v.signing_count}</Num>,
    },
    {
      key: 'retired',
      header: 'Retired',
      render: (v) => (v.retired_at ? <Muted>{formatTime(v.retired_at)}</Muted> : <Muted>—</Muted>),
    },
  ]

  return (
    <Page>
      <PageHeader
        kicker="Cryptography"
        title="Keys"
        sub="Master-key versions and rotation. Re-wrapping is online; reads work against either version throughout a drain."
        actions={
          isOperator ? (
            <>
              <Button onClick={() => setSigningOpen(true)}>Rotate signing key</Button>
              <Button
                variant="primary"
                disabled={!!ip}
                onClick={() => {
                  setToV(data?.active ?? '')
                  setRotateOpen(true)
                }}
              >
                Rotate master key
              </Button>
            </>
          ) : undefined
        }
      />

      {!isOperator && <Banner tone="warn">Key rotation is operator-only — viewing status.</Banner>}
      {status.isError && <Banner>Failed to load rotation status.</Banner>}

      {ip && (
        <Card>
          <div style={{ display: 'flex', alignItems: 'center', gap: 12, flexWrap: 'wrap' }}>
            <Chip tone={ip.stalled ? 'danger' : 'nova'}>{ip.stalled ? 'stalled' : 'rotating'}</Chip>
            <span>
              Draining <Mono>{ip.from}</Mono> → <Mono>{data?.active}</Mono>
            </span>
            <Muted>
              · {ip.remaining_deks} DEKs, {ip.remaining_signing_keys} signing keys remaining
            </Muted>
          </div>
          {ip.stalled && (
            <div style={{ marginTop: 10 }}>
              <Banner>
                Rotation stalled: {ip.stall_reason ?? 'the source key may have been removed before the drain finished.'}
              </Banner>
            </div>
          )}
        </Card>
      )}

      <Card pad={false}>
        <DataTable
          columns={columns}
          rows={data?.versions ?? []}
          getKey={(v) => v.label}
          loading={status.isLoading}
        />
      </Card>

      <Modal
        open={rotateOpen}
        onOpenChange={setRotateOpen}
        title="Rotate master key"
        footer={
          <>
            <Button onClick={() => setRotateOpen(false)} disabled={rotateMaster.isLoading}>
              Cancel
            </Button>
            <Button
              variant="primary"
              disabled={rotateMaster.isLoading || !fromV.trim() || !toV.trim()}
              onClick={() => rotateMaster.mutate()}
            >
              {rotateMaster.isLoading ? 'Starting…' : 'Start rotation'}
            </Button>
          </>
        }
      >
        <p>
          Re-wraps every DEK and signing key from <strong>from</strong> to the active version{' '}
          <strong>to</strong>. The new key must already be deployed and active (the restart done) —{' '}
          <Mono>to</Mono> must equal the active label.
        </p>
        <div style={{ display: 'flex', gap: 12, marginTop: 14 }}>
          <Field label="From version">
            <TextInput value={fromV} onChange={(e) => setFromV(e.target.value)} placeholder="v1" />
          </Field>
          <Field label="To version (active)">
            <TextInput value={toV} onChange={(e) => setToV(e.target.value)} placeholder="v2" />
          </Field>
        </div>
      </Modal>

      <ConfirmDialog
        open={signingOpen}
        onOpenChange={setSigningOpen}
        title="Rotate signing key"
        confirmLabel="Rotate"
        busy={rotateSigning.isLoading}
        onConfirm={() => rotateSigning.mutate()}
      >
        <p>Mints a new signed-URL signing key. The previous key stays valid for the grace window.</p>
        <div style={{ marginTop: 12 }}>
          <Field label="Grace seconds">
            <TextInput type="number" value={grace} onChange={(e) => setGrace(e.target.value)} />
          </Field>
        </div>
      </ConfirmDialog>
    </Page>
  )
}
