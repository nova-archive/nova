import { useEffect, useMemo, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useAuth } from '../auth/AuthProvider'
import { useToast } from '../ui/toast'
import { ApiError } from '../api/client'
import type { ConfigResponse } from '../api/types'
import { Banner, Button, Card, Page, PageHeader } from '../ui/ui'
import {
  buildConfigPatch,
  draftFromConfig,
  webhooksConfigured,
  type SettingsDraft,
} from '../settings/mergePatch'
import { ParanoidSection } from '../settings/ParanoidSection'
import { CorsSection, corsBlocksSave } from '../settings/CorsSection'
import { UploadLimitsSection } from '../settings/UploadLimitsSection'
import { PublicUploadsSection, publicUploadsBlocksSave } from '../settings/PublicUploadsSection'
import { EffectiveConfigViewer } from '../settings/EffectiveConfigViewer'
import s from '../settings/settings.module.css'

export function SettingsScreen() {
  const { api, user } = useAuth()
  const toast = useToast()
  const qc = useQueryClient()
  const isOperator = user?.role === 'operator'

  const query = useQuery({
    queryKey: ['config'],
    queryFn: api.getConfig,
    enabled: isOperator,
  })

  const data = query.data
  const [draft, setDraft] = useState<SettingsDraft | null>(null)
  const [initial, setInitial] = useState<SettingsDraft | null>(null)
  const [restart, setRestart] = useState<string[]>([])
  const [conflict, setConflict] = useState(false)

  // Reseed the draft whenever a fresh config snapshot (new version) arrives.
  useEffect(() => {
    if (data) {
      const d = draftFromConfig(data.config)
      setDraft(d)
      setInitial(d)
    }
  }, [data?.version])

  const webhooks = data ? webhooksConfigured(data.config) : false
  const loadedRetention = initial?.retentionDays ?? 30

  const dirty = useMemo(
    () => (draft && initial ? JSON.stringify(draft) !== JSON.stringify(initial) : false),
    [draft, initial],
  )
  const blocked = draft ? corsBlocksSave(draft) || publicUploadsBlocksSave(draft) : false

  const save = useMutation({
    mutationFn: () => api.patchConfig(buildConfigPatch(draft!, initial!, webhooks), data!.version),
    onSuccess: (resp: ConfigResponse) => {
      toast.ok('Settings saved')
      setConflict(false)
      setRestart(resp.restart_required ?? [])
      qc.setQueryData(['config'], resp) // bumps version → effect reseeds draft
    },
    onError: (e: unknown) => {
      if (e instanceof ApiError && e.status === 409) {
        setConflict(true)
      } else if (e instanceof ApiError && e.status === 422) {
        toast.err('Invalid configuration', e.message)
      } else {
        toast.err('Save failed', e instanceof ApiError ? e.message : 'unexpected error')
      }
    },
  })

  if (!isOperator) {
    return (
      <Page>
        <PageHeader kicker="Configuration" title="Settings" />
        <Banner tone="warn">Settings are operator-only.</Banner>
      </Page>
    )
  }

  const patch = (p: Partial<SettingsDraft>) => setDraft((d) => (d ? { ...d, ...p } : d))

  return (
    <Page>
      <PageHeader
        kicker="Configuration"
        title="Settings"
        sub="Tune a running node. Live changes apply immediately; restart-class changes persist and take effect on the next coordinator restart."
      />

      {query.isError && <Banner>Failed to load configuration.</Banner>}
      {conflict && (
        <Banner tone="warn">
          Configuration changed since you opened this screen.{' '}
          <Button size="sm" onClick={() => void query.refetch()}>
            Reload
          </Button>
        </Banner>
      )}
      {restart.length > 0 && (
        <Banner tone="warn">Saved. A coordinator restart is required for: {restart.join(', ')}.</Banner>
      )}

      {draft && data && (
        <Card>
          <ParanoidSection
            draft={draft}
            loadedRetentionDays={loadedRetention}
            webhooksConfigured={webhooks}
            privacyWarnings={data.privacy_warnings}
            onChange={patch}
          />
          <CorsSection draft={draft} onChange={patch} />
          <UploadLimitsSection draft={draft} onChange={patch} />
          <PublicUploadsSection draft={draft} onChange={patch} />
          <EffectiveConfigViewer config={data.config} fields={data.fields} />

          <div className={s.footer}>
            <Button disabled={!dirty || save.isLoading} onClick={() => initial && setDraft(initial)}>
              Discard
            </Button>
            <Button
              variant="primary"
              disabled={!dirty || blocked || save.isLoading}
              onClick={() => save.mutate()}
            >
              {save.isLoading ? 'Saving…' : 'Save changes'}
            </Button>
          </div>
        </Card>
      )}
    </Page>
  )
}
