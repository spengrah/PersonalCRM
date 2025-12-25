import { describe, it, expect } from 'vitest'
import { parseDateOnly, formatDateOnly } from '../utils'

describe('parseDateOnly', () => {
  it('parses YYYY-MM-DD format correctly', () => {
    const result = parseDateOnly('2024-01-15')
    expect(result).toBeInstanceOf(Date)
    expect(result?.getFullYear()).toBe(2024)
    expect(result?.getMonth()).toBe(0) // 0-indexed (January)
    expect(result?.getDate()).toBe(15)
  })

  it('parses ISO format and extracts date part only', () => {
    const result = parseDateOnly('2024-03-20T14:30:00Z')
    expect(result).toBeInstanceOf(Date)
    expect(result?.getFullYear()).toBe(2024)
    expect(result?.getMonth()).toBe(2) // 0-indexed (March)
    expect(result?.getDate()).toBe(20)
  })

  it('returns null for null input', () => {
    const result = parseDateOnly(null)
    expect(result).toBeNull()
  })

  it('returns null for undefined input', () => {
    const result = parseDateOnly(undefined)
    expect(result).toBeNull()
  })

  it('returns null for empty string', () => {
    const result = parseDateOnly('')
    expect(result).toBeNull()
  })

  it('returns null for invalid format', () => {
    const result = parseDateOnly('not-a-date')
    expect(result).toBeNull()
  })
})

describe('formatDateOnly', () => {
  it('formats valid date string with default options', () => {
    const result = formatDateOnly('2024-01-15')
    // Default format: { year: 'numeric', month: 'short', day: 'numeric' }
    expect(result).toMatch(/Jan/)
    expect(result).toMatch(/15/)
    expect(result).toMatch(/2024/)
  })

  it('returns empty string for null input', () => {
    const result = formatDateOnly(null)
    expect(result).toBe('')
  })

  it('returns empty string for undefined input', () => {
    const result = formatDateOnly(undefined)
    expect(result).toBe('')
  })

  it('returns empty string for invalid date', () => {
    const result = formatDateOnly('invalid-date')
    expect(result).toBe('')
  })

  it('respects custom format options', () => {
    const result = formatDateOnly('2024-01-15', {
      year: 'numeric',
      month: 'long',
      day: '2-digit',
    })
    expect(result).toMatch(/January/)
    expect(result).toMatch(/15/)
    expect(result).toMatch(/2024/)
  })

  it('handles empty string input', () => {
    const result = formatDateOnly('')
    expect(result).toBe('')
  })
})
