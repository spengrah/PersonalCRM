import type { ImportCandidate } from '@/types/import'

/**
 * Compute the display name for a candidate, with Telegram @username and the
 * first email address as last-resort fallbacks before "Unknown". Telegram
 * candidates fall back to their stored '@handle'; gmail_participant
 * candidates (discovered from mail headers, so often no display name is
 * ever observed) fall back to their address so cards never render a
 * meaningless "Unknown" heading for a person we do have contact info for.
 */
export function getCandidateDisplayName(candidate: ImportCandidate): string {
  if (candidate.display_name) return candidate.display_name
  const name = [candidate.first_name, candidate.last_name].filter(Boolean).join(' ')
  if (name) return name
  if (candidate.source === 'telegram' && candidate.metadata?.username) {
    return candidate.metadata.username
  }
  if (candidate.source === 'gmail_participant' && candidate.emails[0]) {
    return candidate.emails[0]
  }
  return 'Unknown'
}

export function isUnresolvedTelegramCandidate(candidate: ImportCandidate): boolean {
  return (
    candidate.source === 'telegram' &&
    !candidate.display_name?.trim() &&
    !candidate.first_name?.trim() &&
    !candidate.last_name?.trim() &&
    !candidate.metadata?.username?.trim() &&
    candidate.emails.length === 0 &&
    candidate.phones.length === 0
  )
}
