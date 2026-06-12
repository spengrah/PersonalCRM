'use client'

import { AlertTriangle } from 'lucide-react'
import { useSyncStaleness } from '@/hooks/use-sync-staleness'
import type { StalenessBreach } from '@/types/sync'

// Human-readable labels for the sources the watchdog reports. Pull sources,
// push sources, and the mac_host heartbeat all flow through here; anything
// not listed falls back to the raw source string.
const SOURCE_LABELS: Record<string, string> = {
  email: 'Gmail',
  gcal: 'Google Calendar',
  gcontacts: 'Google Contacts',
  gchat: 'Google Chat',
  todoist: 'Todoist',
  telegram: 'Telegram',
  messages: 'Messages',
  icloud_contacts: 'iCloud Contacts',
  phone_calls: 'Phone & FaceTime',
  anarlog_sessions: 'Meeting notes',
  anarlog_humans: 'Meeting people',
  mac_host: 'Mac daemon',
}

function sourceLabel(source: string): string {
  return SOURCE_LABELS[source] ?? source
}

// Relative age of a stale reference timestamp, computed client-side so the
// banner ages without the server rewriting rows. Mirrors the coarse buckets
// used elsewhere (formatSyncTime).
function formatAge(staleSince: string, now: Date): string {
  const since = new Date(staleSince)
  if (Number.isNaN(since.getTime())) return ''

  const diffMs = now.getTime() - since.getTime()
  if (diffMs < 0) return 'just now'

  const diffMins = Math.floor(diffMs / (1000 * 60))
  const diffHours = Math.floor(diffMs / (1000 * 60 * 60))
  const diffDays = Math.floor(diffMs / (1000 * 60 * 60 * 24))

  if (diffMins < 1) return 'just now'
  if (diffMins < 60) return `${diffMins}m`
  if (diffHours < 24) return `${diffHours}h`
  return `${diffDays}d`
}

function breachLine(breach: StalenessBreach, now: Date): string {
  const label = sourceLabel(breach.source)
  const age = formatAge(breach.stale_since, now)
  const ageText = age ? ` — stale ${age}` : ''
  const detailText = breach.details ? ` (${breach.details})` : ''
  return `${label}${ageText}${detailText}`
}

/**
 * SyncStalenessBanner surfaces the sync-staleness watchdog's active breaches
 * (#480) as a persistent amber banner. It is deliberately fail-quiet: it
 * renders nothing while loading, on a fetch error, or when there are no
 * breaches, so a flaky poll can never break or alarm the page. The watchdog's
 * own logs are the backend signal path; this banner is just the surface cue.
 */
export function SyncStalenessBanner() {
  const { data: breaches, isError, isLoading } = useSyncStaleness()

  if (isLoading || isError || !breaches || breaches.length === 0) {
    return null
  }

  const now = new Date()
  const count = breaches.length

  return (
    <div
      role="status"
      className="mb-6 rounded-md border border-amber-200 bg-amber-50 p-4"
      data-testid="sync-staleness-banner"
    >
      <div className="flex">
        <div className="flex-shrink-0">
          <AlertTriangle className="h-5 w-5 text-amber-500" aria-hidden="true" />
        </div>
        <div className="ml-3">
          <h3 className="text-sm font-medium text-amber-800">
            {count} sync {count === 1 ? 'source' : 'sources'} may be stalled
          </h3>
          <ul className="mt-2 space-y-1 text-sm text-amber-700">
            {breaches.map(breach => (
              <li key={breach.id}>{breachLine(breach, now)}</li>
            ))}
          </ul>
        </div>
      </div>
    </div>
  )
}
