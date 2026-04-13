'use client'

import { useEffect } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { rematchApi } from '@/lib/rematch-api'
import { calendarKeys, contactKeys, importKeys, rematchKeys } from '@/lib/query-keys'

interface Props {
  jobId: string
  contactId: string
  invalidateImports?: boolean
  onDone: () => void
}

/**
 * Render-nothing component that polls a single rematch job and invalidates
 * the relevant caches once it reaches a terminal status. Mounted (and
 * unmounted) by RematchJobsProvider — never rendered directly by feature
 * code. Surviving navigation is the whole reason this lives at app root
 * rather than per-page.
 */
export function RematchJobWatcher({ jobId, contactId, invalidateImports, onDone }: Props) {
  const queryClient = useQueryClient()

  const { data } = useQuery({
    queryKey: rematchKeys.job(jobId),
    queryFn: () => rematchApi.getJob(jobId),
    refetchInterval: query => {
      const status = query.state.data?.status
      // Poll while running or while we don't yet have a status; stop on any
      // terminal state (completed, failed) or on a 404 (server lost the job).
      return status === 'running' || status == null ? 1000 : false
    },
    // 404 is terminal — don't keep retrying when the job is gone.
    retry: (count, err: unknown) => {
      if (typeof err === 'object' && err !== null && 'status' in err) {
        const status = (err as { status?: number }).status
        if (status === 404) {
          return false
        }
      }
      return count < 2
    },
  })

  useEffect(() => {
    if (!data || data.status === 'running') return

    queryClient.invalidateQueries({ queryKey: contactKeys.detail(contactId) })
    queryClient.invalidateQueries({ queryKey: contactKeys.lists() })
    queryClient.invalidateQueries({ queryKey: calendarKeys.forContact(contactId) })
    queryClient.invalidateQueries({ queryKey: calendarKeys.upcomingForContact(contactId) })
    if (invalidateImports) {
      queryClient.invalidateQueries({ queryKey: importKeys.all })
    }
    onDone()
  }, [data, contactId, invalidateImports, queryClient, onDone])

  return null
}
