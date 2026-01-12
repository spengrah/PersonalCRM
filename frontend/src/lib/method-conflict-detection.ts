import type { ContactMethod, ContactMethodType } from '@/types/contact'
import type { ImportCandidate, MethodComparison, ConflictType, MethodState } from '@/types/import'
import { normalizeContactMethodValueForComparison } from './contact-methods'

/**
 * Get the display name from an import candidate.
 */
export function getCandidateDisplayName(candidate: ImportCandidate): string {
  if (candidate.display_name) return candidate.display_name
  const parts = [candidate.first_name, candidate.last_name].filter(Boolean)
  return parts.join(' ') || 'Unknown'
}

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

  // Build map for CRM methods by normalized value
  const crmByNormalizedValue = new Map<string, ContactMethod>()

  for (const method of crmMethods) {
    const normalized = normalizeContactMethodValueForComparison(method.type, method.value)
    crmByNormalizedValue.set(normalized, method)
  }

  // Process emails
  for (const email of candidate.emails) {
    const suggestedType: ContactMethodType = 'email'
    const normalized = normalizeContactMethodValueForComparison(suggestedType, email)

    // Check if value already exists in CRM
    const existingByValue = crmByNormalizedValue.get(normalized)

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

    // Check if value already exists in CRM
    const existingByValue = crmByNormalizedValue.get(normalized)

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
