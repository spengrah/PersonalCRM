import type { ContactMethod, ContactMethodType } from '@/types/contact'
import type { ImportCandidate, MethodComparison, ConflictType, MethodState } from '@/types/import'
import { inferEmailType } from './email-type-inference'

/**
 * Normalize an email for comparison.
 * Lowercases and trims whitespace.
 */
export function normalizeEmail(email: string): string {
  return email.toLowerCase().trim()
}

/**
 * Normalize a phone number for comparison.
 * Strips all non-digit characters except leading +.
 */
export function normalizePhone(phone: string): string {
  if (!phone) return ''

  const hasLeadingPlus = phone.startsWith('+')
  const digits = phone.replace(/\D/g, '')

  return hasLeadingPlus ? `+${digits}` : digits
}

/**
 * Check if a value is an email address.
 */
function isEmail(value: string): boolean {
  return value.includes('@')
}

/**
 * Normalize any contact method value.
 */
function normalizeValue(value: string): string {
  return isEmail(value) ? normalizeEmail(value) : normalizePhone(value)
}

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
 * Preserves original type information for email type inference.
 */
export function extractExternalMethods(candidate: ImportCandidate): {
  emails: Array<{ value: string; type: string }>
  phones: Array<{ value: string; type: string }>
} {
  // The ImportCandidate only has string arrays, not typed methods
  // We'll need to infer types based on email domain for emails
  // For phones, we always use 'phone' type
  return {
    emails: candidate.emails.map(email => ({
      value: email,
      type: 'unknown', // Will be inferred
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

  // Build maps for CRM methods
  const crmByNormalizedValue = new Map<string, ContactMethod>()
  const crmByType = new Map<string, ContactMethod>()

  for (const method of crmMethods) {
    const normalized = normalizeValue(method.value)
    crmByNormalizedValue.set(normalized, method)
    crmByType.set(method.type, method)
  }

  // Process emails
  for (const email of candidate.emails) {
    const normalized = normalizeEmail(email)
    const suggestedType = inferEmailType(email)

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

      if (existingByValue.type === suggestedType) {
        conflictType = 'identical'
        state = 'unchanged'
      } else {
        conflictType = 'type_conflict'
        state = 'conflict'
      }
    } else {
      // Value doesn't exist - check if type slot is available
      const existingByType = crmByType.get(suggestedType)
      if (existingByType) {
        // Type slot is taken by a different value
        crmMethod = {
          id: existingByType.id || '',
          type: existingByType.type,
          value: existingByType.value,
        }
        conflictType = 'value_conflict'
        state = 'conflict'
      }
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
    const normalized = normalizePhone(phone)
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
      // Value doesn't exist - check if phone slot is taken
      const existingPhone = crmByType.get('phone')
      if (existingPhone) {
        crmMethod = {
          id: existingPhone.id || '',
          type: existingPhone.type,
          value: existingPhone.value,
        }
        conflictType = 'value_conflict'
        state = 'conflict'
      }
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
