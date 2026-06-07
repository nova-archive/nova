import { useEffect, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import type { ListQuery } from '../api/client'
import type { Page } from '../api/types'

const PER_PAGE = 25

function clean(filters: Record<string, string | undefined>): Record<string, string> {
  const out: Record<string, string> = {}
  for (const [k, v] of Object.entries(filters)) {
    if (v) out[k] = v
  }
  return out
}

// usePaged drives a paginated admin listing: it owns the page cursor, resets to
// page 1 when filters change, and keeps the previous page visible while the next
// loads (no table flicker).
export function usePaged<T>(
  key: string,
  fn: (q: ListQuery) => Promise<Page<T>>,
  filters: Record<string, string | undefined> = {},
  opts: { perPage?: number; enabled?: boolean } = {},
) {
  const perPage = opts.perPage ?? PER_PAGE
  const [page, setPage] = useState(1)
  const filterKey = JSON.stringify(filters)

  useEffect(() => {
    setPage(1)
  }, [filterKey])

  const query = useQuery({
    queryKey: [key, page, filterKey],
    queryFn: () => fn({ page, per_page: perPage, ...clean(filters) }),
    keepPreviousData: true,
    enabled: opts.enabled ?? true,
  })

  return { query, page, setPage, perPage }
}
