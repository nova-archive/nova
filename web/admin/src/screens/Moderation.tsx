import { useState } from 'react'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { useAuth } from '../auth/AuthProvider'
import { useToast } from '../ui/toast'
import { usePaged } from '../lib/useList'
import { ApiError } from '../api/client'
import { formatTime, shortCid } from '../lib/format'
import type { BlocklistEntry, DmcaCase, ModerationDecision } from '../api/types'
import {
  Banner,
  Button,
  Chip,
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
  TextInput,
  cls,
} from '../ui/ui'
import { ConfirmDialog, Modal } from '../ui/Dialog'
import s from './Moderation.module.css'

type Tab = 'queue' | 'dmca' | 'blocklist'
type RowAction = { cid: string; type: 'restore' | 'takedown' | 'counter' }

export function ModerationScreen() {
  const { api } = useAuth()
  const toast = useToast()
  const qc = useQueryClient()
  const [tab, setTab] = useState<Tab>('queue')

  // --- queries (one per tab; only the active tab fetches) -------------------
  const queue = usePaged<ModerationDecision>('moderation-queue', api.moderationQueue, {}, {
    enabled: tab === 'queue',
  })
  const dmca = usePaged<DmcaCase>('dmca', api.dmcaCases, {}, { enabled: tab === 'dmca' })
  const block = usePaged<BlocklistEntry>('blocklist', api.blocklist, {}, {
    enabled: tab === 'blocklist',
  })

  // --- action state ---------------------------------------------------------
  const [quarantineOpen, setQuarantineOpen] = useState(false)
  const [qCid, setQCid] = useState('')
  const [qReason, setQReason] = useState('')

  const [rowAction, setRowAction] = useState<RowAction | null>(null)
  const [raText, setRaText] = useState('')

  const [blockOpen, setBlockOpen] = useState(false)
  const [bCid, setBCid] = useState('')
  const [bReason, setBReason] = useState('')

  const [unblockCid, setUnblockCid] = useState<string | null>(null)

  const onErr = (label: string) => (e: unknown) =>
    toast.err(label, e instanceof ApiError ? e.message : 'unexpected error')
  const refreshQueue = () => void qc.invalidateQueries({ queryKey: ['moderation-queue'] })
  const refreshBlock = () => void qc.invalidateQueries({ queryKey: ['blocklist'] })

  const mQuarantine = useMutation({
    mutationFn: () => api.quarantine({ cid: qCid.trim(), reason: qReason.trim() }),
    onSuccess: () => {
      toast.ok('CID quarantined', qCid.trim())
      setQuarantineOpen(false)
      setQCid('')
      setQReason('')
      refreshQueue()
    },
    onError: onErr('Quarantine failed'),
  })

  const mRow = useMutation({
    mutationFn: () => {
      const a = rowAction!
      if (a.type === 'restore') return api.restore({ cid: a.cid, reason: raText.trim() || 'restored' })
      if (a.type === 'takedown') return api.takedown({ cid: a.cid, reason: raText.trim() || 'takedown' })
      return api.counterNotice({ cid: a.cid, notes: raText.trim() })
    },
    onSuccess: () => {
      toast.ok('Action applied', rowAction?.cid)
      setRowAction(null)
      setRaText('')
      refreshQueue()
    },
    onError: onErr('Action failed'),
  })

  const mBlock = useMutation({
    mutationFn: () => api.addBlocklist({ cid: bCid.trim(), reason: bReason.trim() }),
    onSuccess: () => {
      toast.ok('Added to blocklist', bCid.trim())
      setBlockOpen(false)
      setBCid('')
      setBReason('')
      refreshBlock()
    },
    onError: onErr('Add failed'),
  })

  const mUnblock = useMutation({
    mutationFn: () => api.removeBlocklist(unblockCid!),
    onSuccess: () => {
      toast.ok('Removed from blocklist', unblockCid ?? undefined)
      setUnblockCid(null)
      refreshBlock()
    },
    onError: onErr('Remove failed'),
  })

  // --- columns --------------------------------------------------------------
  const queueCols: Array<Column<ModerationDecision>> = [
    {
      key: 'cid',
      header: 'CID',
      render: (d) => (
        <>
          <Mono title={d.cid}>{shortCid(d.cid)}</Mono>
          <Copy value={d.cid} />
        </>
      ),
    },
    { key: 'rule', header: 'Rule', render: (d) => <Mono>{d.rule}</Mono> },
    { key: 'action', header: 'Action', render: (d) => <Mono>{d.action}</Mono> },
    {
      key: 'hold',
      header: 'Legal hold',
      render: (d) => (d.legal_hold ? <Chip tone="danger">held</Chip> : <Muted>—</Muted>),
    },
    {
      key: 'sched',
      header: 'Tombstone at',
      render: (d) =>
        d.scheduled_tombstone_at ? <Muted>{formatTime(d.scheduled_tombstone_at)}</Muted> : <Muted>—</Muted>,
    },
    {
      key: 'actions',
      header: '',
      render: (d) => (
        <div className={s.rowActions}>
          <Button size="sm" onClick={() => { setRowAction({ cid: d.cid, type: 'restore' }); setRaText('') }}>
            Restore
          </Button>
          <Button size="sm" onClick={() => { setRowAction({ cid: d.cid, type: 'counter' }); setRaText('') }}>
            Counter
          </Button>
          <Button size="sm" variant="danger" onClick={() => { setRowAction({ cid: d.cid, type: 'takedown' }); setRaText('') }}>
            Takedown
          </Button>
        </div>
      ),
    },
  ]

  const dmcaCols: Array<Column<DmcaCase>> = [
    {
      key: 'status',
      header: 'Status',
      render: (c) => (
        <Chip tone={c.status === 'actioned' ? 'ok' : c.status === 'rejected' ? 'mute' : 'slate'}>
          {c.status}
        </Chip>
      ),
    },
    { key: 'claimant', header: 'Claimant', render: (c) => <span>{c.claimant_name}</span> },
    {
      key: 'target',
      header: 'Target CID',
      render: (c) => (c.target_cid ? <Mono title={c.target_cid}>{shortCid(c.target_cid)}</Mono> : <Muted>—</Muted>),
    },
    { key: 'received', header: 'Received', render: (c) => <Muted>{formatTime(c.received_at)}</Muted> },
    {
      key: 'actioned',
      header: 'Actioned',
      render: (c) => (c.actioned_at ? <Muted>{formatTime(c.actioned_at)}</Muted> : <Muted>—</Muted>),
    },
  ]

  const blockCols: Array<Column<BlocklistEntry>> = [
    {
      key: 'cid',
      header: 'CID',
      render: (e) => (
        <>
          <Mono title={e.cid}>{shortCid(e.cid)}</Mono>
          <Copy value={e.cid} />
        </>
      ),
    },
    { key: 'reason', header: 'Reason', render: (e) => <span>{e.reason}</span> },
    { key: 'rule', header: 'Rule', render: (e) => <Mono>{e.rule}</Mono> },
    { key: 'created', header: 'Added', render: (e) => <Muted>{formatTime(e.created_at)}</Muted> },
    {
      key: 'actions',
      header: '',
      render: (e) => (
        <div className={s.rowActions}>
          <Button size="sm" variant="danger" onClick={() => setUnblockCid(e.cid)}>
            Remove
          </Button>
        </div>
      ),
    },
  ]

  const headerActions =
    tab === 'queue' ? (
      <Button variant="primary" onClick={() => setQuarantineOpen(true)}>
        Quarantine CID
      </Button>
    ) : tab === 'blocklist' ? (
      <Button variant="primary" onClick={() => setBlockOpen(true)}>
        Add to blocklist
      </Button>
    ) : undefined

  return (
    <Page>
      <PageHeader
        kicker="Trust & safety"
        title="Moderation"
        sub="Quarantine queue, DMCA cases, and the operator-curated CID blocklist."
        actions={headerActions}
      />

      <div className={s.tabs}>
        {(['queue', 'dmca', 'blocklist'] as Tab[]).map((t) => (
          <button key={t} className={cls(s.tab, tab === t && s.tabActive)} onClick={() => setTab(t)}>
            {t === 'queue' ? 'Queue' : t === 'dmca' ? 'DMCA cases' : 'Blocklist'}
          </button>
        ))}
      </div>

      {tab === 'queue' && (
        <>
          {queue.query.isError && <Banner>Failed to load the moderation queue.</Banner>}
          <DataTable
            columns={queueCols}
            rows={queue.query.data?.data ?? []}
            getKey={(d) => d.id}
            loading={queue.query.isLoading}
            empty={<Empty title="Queue is clear" hint="No pending moderation decisions." />}
          />
          <Pagination
            page={queue.page}
            perPage={queue.perPage}
            total={queue.query.data?.pagination.total ?? 0}
            onPage={queue.setPage}
          />
        </>
      )}

      {tab === 'dmca' && (
        <>
          {dmca.query.isError && <Banner>Failed to load DMCA cases.</Banner>}
          <DataTable
            columns={dmcaCols}
            rows={dmca.query.data?.data ?? []}
            getKey={(c) => c.id}
            loading={dmca.query.isLoading}
            empty={<Empty title="No DMCA cases" />}
          />
          <Pagination
            page={dmca.page}
            perPage={dmca.perPage}
            total={dmca.query.data?.pagination.total ?? 0}
            onPage={dmca.setPage}
          />
        </>
      )}

      {tab === 'blocklist' && (
        <>
          {block.query.isError && <Banner>Failed to load the blocklist.</Banner>}
          <DataTable
            columns={blockCols}
            rows={block.query.data?.data ?? []}
            getKey={(e) => e.cid}
            loading={block.query.isLoading}
            empty={<Empty title="Blocklist is empty" />}
          />
          <Pagination
            page={block.page}
            perPage={block.perPage}
            total={block.query.data?.pagination.total ?? 0}
            onPage={block.setPage}
          />
        </>
      )}

      {/* Quarantine */}
      <Modal
        open={quarantineOpen}
        onOpenChange={setQuarantineOpen}
        title="Quarantine a CID"
        footer={
          <>
            <Button onClick={() => setQuarantineOpen(false)} disabled={mQuarantine.isLoading}>
              Cancel
            </Button>
            <Button
              variant="primary"
              disabled={mQuarantine.isLoading || !qCid.trim() || !qReason.trim()}
              onClick={() => mQuarantine.mutate()}
            >
              {mQuarantine.isLoading ? 'Working…' : 'Quarantine'}
            </Button>
          </>
        }
      >
        <p>Blocks reads (451) and revokes signed URLs while preserving the bytes for review.</p>
        <div className={s.form}>
          <Field label="CID">
            <TextInput value={qCid} onChange={(e) => setQCid(e.target.value)} placeholder="bafy…" />
          </Field>
          <Field label="Reason">
            <TextInput value={qReason} onChange={(e) => setQReason(e.target.value)} />
          </Field>
        </div>
      </Modal>

      {/* Row action (restore / takedown / counter) */}
      <ConfirmDialog
        open={rowAction !== null}
        onOpenChange={(o) => !o && setRowAction(null)}
        title={
          rowAction?.type === 'restore'
            ? 'Restore CID'
            : rowAction?.type === 'takedown'
              ? 'Takedown CID'
              : 'Record counter-notice'
        }
        danger={rowAction?.type === 'takedown'}
        confirmLabel={rowAction?.type === 'takedown' ? 'Takedown' : rowAction?.type === 'restore' ? 'Restore' : 'Record'}
        busy={mRow.isLoading}
        onConfirm={() => mRow.mutate()}
      >
        {rowAction?.type === 'takedown' && (
          <p>
            Immediately tombstones and <strong>crypto-shreds</strong> the key (irreversible). Refused
            if the DEK is under legal hold.
          </p>
        )}
        {rowAction?.type === 'restore' && <p>Returns the quarantined CID to active.</p>}
        {rowAction?.type === 'counter' && <p>Records a counter-notice and pauses the scheduled tombstone.</p>}
        <p style={{ marginTop: 8 }}>
          <Mono>{rowAction?.cid}</Mono>
        </p>
        <div style={{ marginTop: 12 }}>
          <Field label={rowAction?.type === 'counter' ? 'Notes' : 'Reason'}>
            <textarea className={s.area} value={raText} onChange={(e) => setRaText(e.target.value)} />
          </Field>
        </div>
      </ConfirmDialog>

      {/* Blocklist add */}
      <Modal
        open={blockOpen}
        onOpenChange={setBlockOpen}
        title="Add to blocklist"
        footer={
          <>
            <Button onClick={() => setBlockOpen(false)} disabled={mBlock.isLoading}>
              Cancel
            </Button>
            <Button
              variant="primary"
              disabled={mBlock.isLoading || !bCid.trim() || !bReason.trim()}
              onClick={() => mBlock.mutate()}
            >
              {mBlock.isLoading ? 'Adding…' : 'Add'}
            </Button>
          </>
        }
      >
        <p>The exact CID is denied at both the read and import paths (451).</p>
        <div className={s.form}>
          <Field label="CID">
            <TextInput value={bCid} onChange={(e) => setBCid(e.target.value)} placeholder="bafy…" />
          </Field>
          <Field label="Reason">
            <TextInput value={bReason} onChange={(e) => setBReason(e.target.value)} />
          </Field>
        </div>
      </Modal>

      {/* Blocklist remove */}
      <ConfirmDialog
        open={unblockCid !== null}
        onOpenChange={(o) => !o && setUnblockCid(null)}
        title="Remove from blocklist"
        danger
        confirmLabel="Remove"
        busy={mUnblock.isLoading}
        onConfirm={() => mUnblock.mutate()}
      >
        <p>Allows the CID to be read and imported again.</p>
        <p style={{ marginTop: 8 }}>
          <Mono>{unblockCid}</Mono>
        </p>
      </ConfirmDialog>
    </Page>
  )
}
