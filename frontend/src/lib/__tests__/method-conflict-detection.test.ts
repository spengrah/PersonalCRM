import { describe, it, expect } from 'vitest'
import {
  normalizeEmail,
  normalizePhone,
  detectMethodConflicts,
  getCandidateDisplayName,
  calculateNameSimilarity,
  areNamesSimilar,
  getMethodStateClasses,
  getMethodStateBadgeText,
  getMethodStateBadgeClasses,
} from '../method-conflict-detection'
import type { ImportCandidate } from '@/types/import'
import type { ContactMethod } from '@/types/contact'

describe('method-conflict-detection', () => {
  describe('normalizeEmail', () => {
    it('lowercases email addresses', () => {
      expect(normalizeEmail('John@Example.COM')).toBe('john@example.com')
    })

    it('trims whitespace', () => {
      expect(normalizeEmail('  john@example.com  ')).toBe('john@example.com')
    })

    it('handles already normalized emails', () => {
      expect(normalizeEmail('john@example.com')).toBe('john@example.com')
    })

    it('handles empty string', () => {
      expect(normalizeEmail('')).toBe('')
    })
  })

  describe('normalizePhone', () => {
    it('strips non-digit characters except leading +', () => {
      expect(normalizePhone('+1 (555) 123-4567')).toBe('+15551234567')
    })

    it('preserves leading + for international numbers', () => {
      expect(normalizePhone('+44 20 7946 0958')).toBe('+442079460958')
    })

    it('handles numbers without leading +', () => {
      expect(normalizePhone('555-123-4567')).toBe('5551234567')
    })

    it('handles already normalized numbers', () => {
      expect(normalizePhone('+15551234567')).toBe('+15551234567')
    })

    it('strips whitespace from phone numbers', () => {
      // Note: implementation checks startsWith('+') before stripping non-digits,
      // so leading whitespace means the + is not at position 0
      expect(normalizePhone('  1555123  ')).toBe('1555123')
      expect(normalizePhone('+1 555 123')).toBe('+1555123')
    })

    it('handles empty string', () => {
      expect(normalizePhone('')).toBe('')
    })
  })

  describe('getCandidateDisplayName', () => {
    it('uses display_name if available', () => {
      const candidate = {
        id: '1',
        display_name: 'John Doe',
        first_name: 'Johnny',
        last_name: 'Doer',
        emails: [],
        phones: [],
        source: 'test',
      } as ImportCandidate

      expect(getCandidateDisplayName(candidate)).toBe('John Doe')
    })

    it('combines first and last name if no display_name', () => {
      const candidate = {
        id: '1',
        first_name: 'John',
        last_name: 'Doe',
        emails: [],
        phones: [],
        source: 'test',
      } as ImportCandidate

      expect(getCandidateDisplayName(candidate)).toBe('John Doe')
    })

    it('uses only first name if no last name', () => {
      const candidate = {
        id: '1',
        first_name: 'John',
        emails: [],
        phones: [],
        source: 'test',
      } as ImportCandidate

      expect(getCandidateDisplayName(candidate)).toBe('John')
    })

    it('returns Unknown if no name parts', () => {
      const candidate = {
        id: '1',
        emails: [],
        phones: [],
        source: 'test',
      } as ImportCandidate

      expect(getCandidateDisplayName(candidate)).toBe('Unknown')
    })
  })

  describe('calculateNameSimilarity', () => {
    it('returns 1 for identical names', () => {
      expect(calculateNameSimilarity('John Doe', 'John Doe')).toBe(1)
    })

    it('returns 1 for case-insensitive match', () => {
      expect(calculateNameSimilarity('john doe', 'JOHN DOE')).toBe(1)
    })

    it('returns high score for similar names', () => {
      // "John Doe" vs "John Michael Doe" = 2/3 overlap = 0.67
      const score = calculateNameSimilarity('John Doe', 'John Michael Doe')
      expect(score).toBeGreaterThan(0.5)
    })

    it('returns low score for different names', () => {
      const score = calculateNameSimilarity('John Doe', 'Jane Smith')
      expect(score).toBeLessThan(0.5)
    })

    it('handles empty strings', () => {
      expect(calculateNameSimilarity('', '')).toBe(0)
      expect(calculateNameSimilarity('John', '')).toBe(0)
    })

    it('handles single word names', () => {
      const score = calculateNameSimilarity('John', 'John')
      expect(score).toBe(1)
    })
  })

  describe('areNamesSimilar', () => {
    it('returns true for identical names', () => {
      expect(areNamesSimilar('John Doe', 'John Doe')).toBe(true)
    })

    it('returns true for similar names', () => {
      // "John Doe" vs "John Doe Smith" - 2/3 overlap = 0.67 > 0.5 threshold
      expect(areNamesSimilar('John Doe', 'John Doe Smith')).toBe(true)
    })

    it('returns false for different names', () => {
      expect(areNamesSimilar('John Doe', 'Jane Smith')).toBe(false)
    })

    it('uses custom threshold', () => {
      // "John Doe" vs "John Doe Smith" has similarity 2/3 = 0.67
      // With high threshold, requires more similarity
      expect(areNamesSimilar('John Doe', 'John Doe Smith', 0.8)).toBe(false)
      // With low threshold, accepts less similarity
      expect(areNamesSimilar('John Doe', 'John Doe Smith', 0.5)).toBe(true)
    })
  })

  describe('detectMethodConflicts', () => {
    const createCandidate = (emails: string[] = [], phones: string[] = []): ImportCandidate => ({
      id: 'ext-1',
      source: 'gcontacts',
      display_name: 'Test User',
      emails,
      phones,
    })

    const createMethod = (type: string, value: string): ContactMethod => ({
      id: 'method-1',
      contact_id: 'contact-1',
      type: type as ContactMethod['type'],
      value,
      is_primary: false,
      created_at: '2024-01-01',
      updated_at: '2024-01-01',
    })

    it('returns adding state for new emails not in CRM', () => {
      const candidate = createCandidate(['new@gmail.com']) // Free domain -> personal
      const crmMethods: ContactMethod[] = []

      const result = detectMethodConflicts(candidate, crmMethods)

      expect(result).toHaveLength(1)
      expect(result[0]).toMatchObject({
        external_value: 'new@gmail.com',
        external_type: 'email',
        conflict_type: 'none',
        state: 'adding',
        suggested_crm_type: 'email_personal',
      })
    })

    it('returns adding state for work emails not in CRM', () => {
      const candidate = createCandidate(['new@company.com']) // Non-free domain -> work
      const crmMethods: ContactMethod[] = []

      const result = detectMethodConflicts(candidate, crmMethods)

      expect(result).toHaveLength(1)
      expect(result[0]).toMatchObject({
        external_value: 'new@company.com',
        external_type: 'email',
        conflict_type: 'none',
        state: 'adding',
        suggested_crm_type: 'email_work',
      })
    })

    it('returns adding state for new phones not in CRM', () => {
      const candidate = createCandidate([], ['+15551234567'])
      const crmMethods: ContactMethod[] = []

      const result = detectMethodConflicts(candidate, crmMethods)

      expect(result).toHaveLength(1)
      expect(result[0]).toMatchObject({
        external_value: '+15551234567',
        external_type: 'phone',
        conflict_type: 'none',
        state: 'adding',
        suggested_crm_type: 'phone',
      })
    })

    it('returns identical state for matching email with same type', () => {
      // Use gmail.com which is a free domain -> inferred as personal
      const candidate = createCandidate(['john@gmail.com'])
      const crmMethods = [createMethod('email_personal', 'john@gmail.com')]

      const result = detectMethodConflicts(candidate, crmMethods)

      expect(result).toHaveLength(1)
      expect(result[0]).toMatchObject({
        external_value: 'john@gmail.com',
        conflict_type: 'identical',
        state: 'unchanged',
      })
      expect(result[0].crm_method?.value).toBe('john@gmail.com')
    })

    it('detects type conflict for same value with different type', () => {
      // john@gmail.com would infer as email_personal, but CRM has it as work
      const candidate = createCandidate(['john@gmail.com'])
      const crmMethods = [createMethod('email_work', 'john@gmail.com')]

      const result = detectMethodConflicts(candidate, crmMethods)

      expect(result).toHaveLength(1)
      expect(result[0]).toMatchObject({
        external_value: 'john@gmail.com',
        conflict_type: 'type_conflict',
        state: 'conflict',
      })
      expect(result[0].crm_method?.type).toBe('email_work')
    })

    it('detects value conflict for same type with different value', () => {
      // Both emails infer as personal, but different values
      const candidate = createCandidate(['new@gmail.com'])
      const crmMethods = [createMethod('email_personal', 'old@gmail.com')]

      const result = detectMethodConflicts(candidate, crmMethods)

      expect(result).toHaveLength(1)
      expect(result[0]).toMatchObject({
        external_value: 'new@gmail.com',
        conflict_type: 'value_conflict',
        state: 'conflict',
        suggested_crm_type: 'email_personal',
      })
      expect(result[0].crm_method?.value).toBe('old@gmail.com')
    })

    it('handles multiple emails with mixed states', () => {
      const candidate = createCandidate([
        'existing@gmail.com', // Gmail is free email domain -> personal
        'new@company.com', // Company domain -> work type
      ])
      const crmMethods = [createMethod('email_personal', 'existing@gmail.com')]

      const result = detectMethodConflicts(candidate, crmMethods)

      expect(result).toHaveLength(2)
      // First email matches (identical)
      expect(result[0].conflict_type).toBe('identical')
      // Second email is new (different type slot - work)
      expect(result[1].conflict_type).toBe('none')
      expect(result[1].state).toBe('adding')
    })

    it('detects value conflict when type slot is taken', () => {
      const candidate = createCandidate([
        'new@gmail.com', // Personal email (gmail is free domain)
      ])
      const crmMethods = [createMethod('email_personal', 'old@gmail.com')]

      const result = detectMethodConflicts(candidate, crmMethods)

      expect(result).toHaveLength(1)
      expect(result[0].conflict_type).toBe('value_conflict')
      expect(result[0].state).toBe('conflict')
    })

    it('handles phones with normalized matching', () => {
      const candidate = createCandidate([], ['+1 (555) 123-4567'])
      const crmMethods = [createMethod('phone', '+15551234567')]

      const result = detectMethodConflicts(candidate, crmMethods)

      expect(result).toHaveLength(1)
      expect(result[0].conflict_type).toBe('identical')
    })

    it('handles empty candidate methods', () => {
      const candidate = createCandidate()
      const crmMethods = [createMethod('email_personal', 'john@gmail.com')]

      const result = detectMethodConflicts(candidate, crmMethods)

      expect(result).toHaveLength(0)
    })

    it('handles empty CRM methods', () => {
      const candidate = createCandidate(['john@gmail.com'], ['+15551234567'])
      const crmMethods: ContactMethod[] = []

      const result = detectMethodConflicts(candidate, crmMethods)

      expect(result).toHaveLength(2)
      expect(result.every(r => r.conflict_type === 'none')).toBe(true)
      expect(result.every(r => r.state === 'adding')).toBe(true)
    })
  })

  describe('getMethodStateClasses', () => {
    it('returns correct classes for unchanged state', () => {
      const classes = getMethodStateClasses('unchanged')
      expect(classes).toContain('bg-gray')
    })

    it('returns correct classes for adding state', () => {
      const classes = getMethodStateClasses('adding')
      expect(classes).toContain('bg-green')
    })

    it('returns correct classes for conflict state', () => {
      const classes = getMethodStateClasses('conflict')
      expect(classes).toContain('bg-red')
    })

    it('returns correct classes for name_mismatch state', () => {
      const classes = getMethodStateClasses('name_mismatch')
      expect(classes).toContain('bg-amber')
    })
  })

  describe('getMethodStateBadgeText', () => {
    it('returns Same as CRM for unchanged state', () => {
      expect(getMethodStateBadgeText('unchanged')).toBe('Same as CRM')
    })

    it('returns New for adding state', () => {
      expect(getMethodStateBadgeText('adding')).toBe('New')
    })

    it('returns Conflict for conflict state', () => {
      expect(getMethodStateBadgeText('conflict')).toBe('Conflict')
    })

    it('returns Review for name_mismatch state', () => {
      expect(getMethodStateBadgeText('name_mismatch')).toBe('Review')
    })
  })

  describe('getMethodStateBadgeClasses', () => {
    it('returns correct classes for adding state', () => {
      const classes = getMethodStateBadgeClasses('adding')
      expect(classes).toContain('bg-green')
      expect(classes).toContain('text-green')
    })

    it('returns correct classes for conflict state', () => {
      const classes = getMethodStateBadgeClasses('conflict')
      expect(classes).toContain('bg-red')
      expect(classes).toContain('text-red')
    })

    it('returns correct classes for name_mismatch state', () => {
      const classes = getMethodStateBadgeClasses('name_mismatch')
      expect(classes).toContain('bg-amber')
      expect(classes).toContain('text-amber')
    })
  })
})
