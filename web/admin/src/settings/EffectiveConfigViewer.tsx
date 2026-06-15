import { useState } from 'react'
import s from './settings.module.css'
import type { FieldEffect, FieldMeta, OperatorConfig } from '../api/types'
import { flattenConfig } from './registry'

const effectClass: Record<FieldEffect, string> = {
  live: s.effectLive,
  restart: s.effectRestart,
  'env-only-inert': s.effectInert,
}

export function EffectiveConfigViewer({
  config,
  fields,
}: {
  config: OperatorConfig
  fields: Record<string, FieldMeta>
}) {
  const [open, setOpen] = useState(false)
  const rows = flattenConfig(config)
  return (
    <div className={s.section}>
      <button className={s.viewerToggle} onClick={() => setOpen((o) => !o)} aria-expanded={open}>
        {open ? '▾' : '▸'} Effective config ({rows.length} fields, read-only)
      </button>
      {open && (
        <>
          <p className={s.sectionSub}>
            This screen edits high-impact settings. The full surface is editable via{' '}
            <code>novactl config set/apply</code>.
          </p>
          <table className={s.viewerTable}>
            <thead>
              <tr>
                <th>Path</th>
                <th>Value</th>
                <th>Effect</th>
                <th>Source</th>
              </tr>
            </thead>
            <tbody>
              {rows.map(({ path, value }) => {
                const meta = fields[path]
                const effect = meta?.effect ?? 'restart'
                return (
                  <tr key={path}>
                    <td className={s.viewerPath}>{path}</td>
                    <td className={s.viewerVal}>{JSON.stringify(value)}</td>
                    <td>
                      <span className={`${s.effectBadge} ${effectClass[effect]}`}>{effect}</span>
                    </td>
                    <td>
                      {meta?.source ?? 'yaml'}
                      {meta?.shadowed_by_env && <span className={s.envTag}>env-shadowed</span>}
                    </td>
                  </tr>
                )
              })}
            </tbody>
          </table>
        </>
      )}
    </div>
  )
}
