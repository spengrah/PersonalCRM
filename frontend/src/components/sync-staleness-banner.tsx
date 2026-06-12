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

// One banner line per breach. The watchdog refreshes details every tick
// (server-side, accelerated-time-aware), so the line needs no client-side
// age computation.
function breachLine(breach: StalenessBreach): string {
  const label = sourceLabel(breach.source)
  return breach.details ? `${label} — ${breach.details}` : label
}

/**
 * SyncStalenessBanner surfaces the sync-staleness watchdog's active breaches
 * on the settings page. sync_error breaches are excluded: the settings page
 * already shows per-source provider/OAuth errors, so the banner only carries
 * the classes nothing else surfaces (dead daemon heartbeat, silent pull
 * staleness, push staleness). It is deliberately fail-quiet: it renders
 * nothing while loading, on a fetch error, or when there is nothing to show,
 * so a flaky poll can never break or alarm the page. The watchdog's own logs
 * are the backend signal path; this banner is just the surface cue.
 */
export function SyncStalenessBanner() {
  const { data: breaches, isError, isLoading } = useSyncStaleness()

  if (isLoading || isError || !breaches) {
    return null
  }

  const visible = breaches.filter(breach => breach.breach_type !== 'sync_error')
  if (visible.length === 0) {
    return null
  }

  const count = visible.length

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
            {visible.map(breach => (
              <li key={breach.id}>{breachLine(breach)}</li>
            ))}
          </ul>
        </div>
      </div>
    </div>
  )
}
