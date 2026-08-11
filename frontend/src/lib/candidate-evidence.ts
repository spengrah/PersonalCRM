import type { ImportCandidate } from '@/types/import'

function formatMessageCount(count: number): string {
  return `${count} ${count === 1 ? 'message' : 'messages'}`
}

function formatLastMessageAt(timestamp: string): string | null {
  const date = new Date(timestamp)
  if (Number.isNaN(date.getTime())) return null
  return `Last: ${date.toLocaleDateString('en-US', { month: 'short', day: 'numeric', year: 'numeric' })}`
}

/**
 * The counterpart segment: who or what this address's evidence is anchored
 * to, per source. Telegram carries no counterpart segment here — its
 * identity (name or handle) already renders in the card's heading and/or
 * username chip, and repeating it in the evidence line would show the same
 * identity twice.
 */
function counterpartSegment(candidate: ImportCandidate): string | null {
  const metadata = candidate.metadata
  if (!metadata) return null

  if (metadata.co_occurring_contact?.name) {
    return `Seen with ${metadata.co_occurring_contact.name}`
  }

  if (metadata.trusted_sender) {
    if (metadata.trusted_sender.self) return 'Sent by you'
    const label = metadata.trusted_sender.name ?? metadata.trusted_sender.address
    return label ? `From ${label}` : null
  }

  if (candidate.source === 'whatsapp' && metadata.push_name) {
    return metadata.push_name
  }

  return null
}

/**
 * The queue's generic evidence line: up to three segments (counterpart,
 * message count, recency) joined with ' · '. Returns null when the
 * candidate's metadata carries none of the recognized evidence keys, so
 * cards with nothing to show render no pill.
 */
export function candidateEvidenceLabel(candidate: ImportCandidate): string | null {
  const metadata = candidate.metadata
  if (!metadata) return null

  const segments = [
    counterpartSegment(candidate),
    metadata.message_count ? formatMessageCount(metadata.message_count) : null,
    metadata.last_message_at ? formatLastMessageAt(metadata.last_message_at) : null,
  ].filter((segment): segment is string => Boolean(segment))

  return segments.length > 0 ? segments.join(' · ') : null
}
