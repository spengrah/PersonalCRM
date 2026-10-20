import type { ThreadVenue } from '@/types/generated/judgment'

/** The name a venue chip gives the app a thread lives in, keyed by the generated ThreadVenue. */
const VENUE_LABELS: Record<ThreadVenue, string> = {
  whatsapp: 'WhatsApp',
  telegram: 'Telegram',
  imessage: 'iMessage',
  google_chat: 'Google Chat',
  email: 'Email',
  calendar: 'Calendar',
  anarlog: 'Anarlog',
}

export function VenueChip({ venue }: { venue: ThreadVenue }) {
  return (
    <span
      data-venue-chip={venue}
      className="rounded bg-gray-100 px-2 py-0.5 text-xs font-medium text-gray-700"
    >
      {VENUE_LABELS[venue]}
    </span>
  )
}

export function GroupChip() {
  return (
    <span
      data-group-chip
      className="rounded-full bg-purple-50 px-2 py-0.5 text-xs font-medium text-purple-700"
    >
      Group
    </span>
  )
}
