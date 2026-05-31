'use client'

import { AlertCircle, ExternalLink } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { SessionLede } from './SessionLede'
import type { NeedsAttentionItem } from '@/types/import'

interface OrphanCardProps {
  item: NeedsAttentionItem
  busy?: boolean
  onLogImpromptu: (item: NeedsAttentionItem) => void
}

/**
 * An orphan card: a session with no time-overlap candidate at all. Offers
 * "Open Anarlog" (bare hyprnote:// deep link) and "Log as impromptu"
 * (the none_of_these promotion to the zero-candidate flow). Left rule
 * gray.
 */
export function OrphanCard({ item, busy, onLogImpromptu }: OrphanCardProps) {
  return (
    <div className="rounded-lg border border-gray-200 border-l-2 border-l-gray-300 bg-white p-5">
      <SessionLede
        title={item.title}
        meetingAt={item.meeting_at}
        summaryExcerpt={item.summary_excerpt}
      />
      <div className="mt-4 flex flex-wrap items-center justify-between gap-3 rounded-lg border border-gray-200 bg-gray-50 px-3.5 py-3">
        <span className="flex items-center gap-2 text-[0.8125rem] text-gray-600">
          <AlertCircle className="h-[15px] w-[15px] text-gray-400" />
          No calendar event or call matched this time. Usually fixed by tagging in Anarlog.
        </span>
        <div className="flex items-center gap-2">
          <Button
            size="sm"
            variant="ghost"
            disabled={busy}
            onClick={() => onLogImpromptu(item)}
            className="text-gray-500"
          >
            Log as impromptu
          </Button>
          {/* Bare hyprnote:// deep link — opens the Anarlog app. */}
          <a
            href="hyprnote://"
            className="inline-flex items-center justify-center rounded-md border border-gray-300 bg-white px-3 py-2 text-sm font-medium text-gray-700 transition-colors hover:bg-gray-50 focus:outline-none focus:ring-2 focus:ring-blue-500 focus:ring-offset-2"
          >
            <ExternalLink className="mr-1 h-3.5 w-3.5" />
            Open Anarlog
          </a>
        </div>
      </div>
    </div>
  )
}
