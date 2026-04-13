import type { ContactMethod, ContactMethodType } from '@/types/contact'
import type { ImportCandidate, MethodComparison, ConflictType, MethodState } from '@/types/import'
import { normalizeContactMethodValueForComparison } from './contact-methods'

/**
 * Extract external contact methods from an import candidate.
 * Preserves original type information for conflict resolution.
 */
export function extractExternalMethods(candidate: ImportCandidate): {
  emails: Array<{ value: string; type: string }>
  phones: Array<{ value: string; type: string }>
} {
  // The ImportCandidate only has string arrays, not typed methods
  // Preserve the external type for error messaging and conflict handling.
  // For phones, we always use 'phone' type
  return {
    emails: candidate.emails.map(email => ({
      value: email,
      type: 'unknown',
    })),
    phones: candidate.phones.map(phone => ({
      value: phone,
      type: 'phone',
    })),
  }
}

/**
 * Detect conflicts between external methods and CRM methods.
 * Returns a comparison for each external method showing its state.
 */
export function detectMethodConflicts(
  candidate: ImportCandidate,
  crmMethods: ContactMethod[]
): MethodComparison[] {
  const comparisons: MethodComparison[] = []

  // Build map for CRM methods keyed by `type:normalized_value`. Type-scoping
  // mirrors the backend dedup shape (DB unique index on
  // `(contact_id, type, value_normalized)`) so a Telegram `@foo` is not
  // falsely marked as "same as CRM" when the only existing row is
  // `twitter:foo` — the two live in separate type buckets.
  const crmByTypedKey = new Map<string, ContactMethod>()
  const typedKey = (type: string, normalized: string) => `${type}:${normalized}`

  for (const method of crmMethods) {
    const normalized = normalizeContactMethodValueForComparison(method.type, method.value)
    crmByTypedKey.set(typedKey(method.type, normalized), method)
  }

  // Process emails
  for (const email of candidate.emails) {
    const suggestedType: ContactMethodType = 'email'
    const normalized = normalizeContactMethodValueForComparison(suggestedType, email)

    // Check if value already exists in CRM (type-scoped)
    const existingByValue = crmByTypedKey.get(typedKey(suggestedType, normalized))

    let conflictType: ConflictType = 'none'
    let state: MethodState = 'adding'
    let crmMethod: MethodComparison['crm_method'] | undefined

    if (existingByValue) {
      // Value exists - check type match
      crmMethod = {
        id: existingByValue.id || '',
        type: existingByValue.type,
        value: existingByValue.value,
      }

      conflictType = 'identical'
      state = 'unchanged'
    } else {
      conflictType = 'none'
      state = 'adding'
    }

    comparisons.push({
      external_value: email,
      external_type: 'email',
      suggested_crm_type: suggestedType,
      crm_method: crmMethod,
      conflict_type: conflictType,
      state,
    })
  }

  // Process phones
  for (const phone of candidate.phones) {
    const normalized = normalizeContactMethodValueForComparison('phone', phone)
    const suggestedType: ContactMethodType = 'phone'

    // Check if value already exists in CRM (type-scoped)
    const existingByValue = crmByTypedKey.get(typedKey(suggestedType, normalized))

    let conflictType: ConflictType = 'none'
    let state: MethodState = 'adding'
    let crmMethod: MethodComparison['crm_method'] | undefined

    if (existingByValue) {
      // Value exists
      crmMethod = {
        id: existingByValue.id || '',
        type: existingByValue.type,
        value: existingByValue.value,
      }
      conflictType = 'identical'
      state = 'unchanged'
    } else {
      conflictType = 'none'
      state = 'adding'
    }

    comparisons.push({
      external_value: phone,
      external_type: 'phone',
      suggested_crm_type: suggestedType,
      crm_method: crmMethod,
      conflict_type: conflictType,
      state,
    })
  }

  // Process Telegram @username from metadata (source-gated). Mirrors the
  // backend's buildMethodsAuto — a Telegram peer's handle is itself a
  // contact method that should appear in the Link modal's comparison UI.
  // The external_value is kept in display form ('@handle') so the modal's
  // methodSelections lookup (keyed by @handle) matches.
  if (candidate.source === 'telegram' && candidate.metadata?.username) {
    const rawHandle = candidate.metadata.username
    const displayValue = rawHandle.startsWith('@') ? rawHandle : `@${rawHandle}`
    const suggestedType: ContactMethodType = 'telegram'
    const normalized = normalizeContactMethodValueForComparison(suggestedType, displayValue)

    const existingByValue = crmByTypedKey.get(typedKey(suggestedType, normalized))

    let conflictType: ConflictType = 'none'
    let state: MethodState = 'adding'
    let crmMethod: MethodComparison['crm_method'] | undefined

    if (existingByValue) {
      crmMethod = {
        id: existingByValue.id || '',
        type: existingByValue.type,
        value: existingByValue.value,
      }
      conflictType = 'identical'
      state = 'unchanged'
    }

    comparisons.push({
      external_value: displayValue,
      external_type: 'telegram',
      suggested_crm_type: suggestedType,
      crm_method: crmMethod,
      conflict_type: conflictType,
      state,
    })
  }

  return comparisons
}

/**
 * Get the visual state class for a method comparison.
 */
export function getMethodStateClasses(state: MethodState): string {
  switch (state) {
    case 'unchanged':
      return 'bg-gray-50 border-gray-200'
    case 'adding':
      return 'bg-green-50 border-green-200'
    case 'conflict':
      return 'bg-red-50 border-red-200'
    case 'name_mismatch':
      return 'bg-amber-50 border-amber-200'
    default:
      return 'bg-gray-50 border-gray-200'
  }
}

/**
 * Get the badge text for a method state.
 */
export function getMethodStateBadgeText(state: MethodState): string {
  switch (state) {
    case 'unchanged':
      return 'Same as CRM'
    case 'adding':
      return 'New'
    case 'conflict':
      return 'Conflict'
    case 'name_mismatch':
      return 'Review'
    default:
      return ''
  }
}

/**
 * Get badge classes for a method state.
 */
export function getMethodStateBadgeClasses(state: MethodState): string {
  switch (state) {
    case 'unchanged':
      return 'bg-gray-100 text-gray-600'
    case 'adding':
      return 'bg-green-100 text-green-700'
    case 'conflict':
      return 'bg-red-100 text-red-700'
    case 'name_mismatch':
      return 'bg-amber-100 text-amber-700'
    default:
      return 'bg-gray-100 text-gray-600'
  }
}

/**
 * Calculate simple name similarity (0-1).
 * Uses Jaccard similarity on normalized tokens.
 */
export function calculateNameSimilarity(name1: string, name2: string): number {
  const normalize = (s: string) => s.toLowerCase().trim().split(/\s+/).filter(Boolean)

  const tokens1 = new Set(normalize(name1))
  const tokens2 = new Set(normalize(name2))

  if (tokens1.size === 0 || tokens2.size === 0) return 0

  const intersection = [...tokens1].filter(t => tokens2.has(t)).length
  const union = new Set([...tokens1, ...tokens2]).size

  return intersection / union
}

/**
 * Check if two names are similar enough to not require review.
 */
export function areNamesSimilar(name1: string, name2: string, threshold = 0.5): boolean {
  return calculateNameSimilarity(name1, name2) >= threshold
}
