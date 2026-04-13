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

  const { data, error } = useQuery({
    queryKey: rematchKeys.job(jobId),
    queryFn: () => rematchApi.getJob(jobId),
    refetchInterval: query => {
      const status = query.state.data?.status
      const errStatus = getErrStatus(query.state.error)
      // Stop polling on any terminal state: completed/failed, or a 404 (the
      // server lost the job — e.g. after a restart). Poll while running or
      // before the first response arrives.
      if (errStatus === 404) return false
      return status === 'running' || status == null ? 1000 : false
    },
    // 404 is terminal — don't retry. Anything else gets a couple of retries.
    retry: (count, err: unknown) => {
      if (getErrStatus(err) === 404) return false
      return count < 2
    },
  })

  useEffect(() => {
    const errStatus = getErrStatus(error)
    const isTerminal = (data && data.status !== 'running') || errStatus === 404
    if (!isTerminal) return

    // On 404 we can't prove the rematch completed vs. was never started —
    // invalidate anyway so stale reads are corrected at the next render.
    queryClient.invalidateQueries({ queryKey: contactKeys.detail(contactId) })
    queryClient.invalidateQueries({ queryKey: contactKeys.lists() })
    queryClient.invalidateQueries({ queryKey: calendarKeys.forContact(contactId) })
    queryClient.invalidateQueries({ queryKey: calendarKeys.upcomingForContact(contactId) })
    if (invalidateImports) {
      queryClient.invalidateQueries({ queryKey: importKeys.all })
    }
    onDone()
  }, [data, error, contactId, invalidateImports, queryClient, onDone])

  return null
}

function getErrStatus(err: unknown): number | undefined {
  if (typeof err === 'object' && err !== null && 'status' in err) {
    return (err as { status?: number }).status
  }
  return undefined
}
