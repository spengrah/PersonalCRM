import type { ImportCandidate } from '@/types/import'
import { getCandidateDisplayName } from '@/lib/candidate-display'

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
 * to, per source. Telegram's username is shown here only when the card's
 * existing username chip (imports/page.tsx) would be suppressed — i.e. when
 * the candidate has no name fields and the display name already fell back to
 * the username itself — so the chip and the evidence line never repeat the
 * same identity.
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

  if (candidate.source === 'telegram' && metadata.username) {
    const displayName = getCandidateDisplayName(candidate)
    return metadata.username === displayName ? metadata.username : null
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
