import type { ImportCandidate } from '@/types/import'

/**
 * Compute the display name for a candidate, with Telegram @username as the
 * last-resort fallback before "Unknown". Returns the stored '@handle' when
 * all name fields are empty for a Telegram candidate so cards never render
 * a meaningless "Unknown" heading for peers we actually have a handle for.
 */
export function getCandidateDisplayName(candidate: ImportCandidate): string {
  if (candidate.display_name) return candidate.display_name
  const name = [candidate.first_name, candidate.last_name].filter(Boolean).join(' ')
  if (name) return name
  if (candidate.source === 'telegram' && candidate.metadata?.username) {
    return candidate.metadata.username
  }
  return 'Unknown'
}
