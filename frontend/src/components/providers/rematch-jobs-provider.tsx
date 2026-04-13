'use client'

import { createContext, useCallback, useContext, useState } from 'react'
import { RematchJobWatcher } from './rematch-job-watcher'

interface PendingJob {
  jobId: string
  contactId: string
  invalidateImports?: boolean
}

interface RematchJobsContextValue {
  registerJob: (job: PendingJob) => void
}

const RematchJobsContext = createContext<RematchJobsContextValue | null>(null)

/**
 * Tracks rematch jobs across the whole app so polling continues even when the
 * user navigates away from the page that triggered the rematch. Each job runs
 * its own RematchJobWatcher (a render-nothing component) which polls the API
 * once per second and invalidates the affected query caches on terminal
 * status.
 *
 * Why global, not a per-page hook: a useRematchJob hook scoped to a contact
 * detail page would unmount when the user navigates away, and React Query
 * would garbage-collect the polling query after gcTime — leaving stale
 * calendar data in the cache for the user's next visit. The provider is
 * mounted at app root so it survives page transitions.
 */
export function RematchJobsProvider({ children }: { children: React.ReactNode }) {
  const [jobs, setJobs] = useState<PendingJob[]>([])

  const registerJob = useCallback((job: PendingJob) => {
    setJobs(prev => (prev.some(p => p.jobId === job.jobId) ? prev : [...prev, job]))
  }, [])

  const removeJob = useCallback((jobId: string) => {
    setJobs(prev => prev.filter(p => p.jobId !== jobId))
  }, [])

  return (
    <RematchJobsContext.Provider value={{ registerJob }}>
      {children}
      {jobs.map(job => (
        <RematchJobWatcher
          key={job.jobId}
          jobId={job.jobId}
          contactId={job.contactId}
          invalidateImports={job.invalidateImports}
          onDone={() => removeJob(job.jobId)}
        />
      ))}
    </RematchJobsContext.Provider>
  )
}

/**
 * Returns a function that mutation hooks call inside their onSuccess handler
 * to register a freshly-created rematch job with the global watcher. The
 * provider takes ownership of polling + invalidation from there.
 */
export function useRegisterRematchJob() {
  const ctx = useContext(RematchJobsContext)
  if (!ctx) {
    throw new Error('useRegisterRematchJob must be used inside RematchJobsProvider')
  }
  return ctx.registerJob
}
