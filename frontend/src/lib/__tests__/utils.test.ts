import { describe, it, expect } from 'vitest'
import {
  parseDateOnly,
  formatDateOnly,
  formatBirthday,
  isPlaceholderBirthday,
  PLACEHOLDER_BIRTHDAY_YEAR,
  formatCadence,
  getLocalCalendarDayDifference,
  formatRelativeTime,
} from '../utils'

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

describe('isPlaceholderBirthday', () => {
  it('detects the sentinel placeholder birthday year', () => {
    expect(isPlaceholderBirthday(`${PLACEHOLDER_BIRTHDAY_YEAR}-01-15`)).toBe(true)
    expect(isPlaceholderBirthday(`${PLACEHOLDER_BIRTHDAY_YEAR}-01-15T00:00:00Z`)).toBe(true)
  })

  it('does not treat real birthday years or invalid dates as placeholders', () => {
    expect(isPlaceholderBirthday('1990-01-15')).toBe(false)
    expect(isPlaceholderBirthday('invalid-date')).toBe(false)
    expect(isPlaceholderBirthday(null)).toBe(false)
  })
})

describe('formatBirthday', () => {
  it('omits the year for placeholder-year birthdays', () => {
    const result = formatBirthday(`${PLACEHOLDER_BIRTHDAY_YEAR}-01-15`)

    expect(result).toMatch(/Jan/)
    expect(result).toMatch(/15/)
    expect(result).not.toMatch(String(PLACEHOLDER_BIRTHDAY_YEAR))
  })

  it('omits the year from custom date options for placeholder-year birthdays', () => {
    const result = formatBirthday(`${PLACEHOLDER_BIRTHDAY_YEAR}-01-15`, {
      year: '2-digit',
      month: 'numeric',
      day: 'numeric',
    })

    expect(result).toMatch(/^1\/15$/)
  })

  it('preserves the requested year format for real birthday years', () => {
    const result = formatBirthday('1990-01-15', {
      year: 'numeric',
      month: 'long',
      day: 'numeric',
    })

    expect(result).toMatch(/January/)
    expect(result).toMatch(/15/)
    expect(result).toMatch(/1990/)
  })
})

describe('getLocalCalendarDayDifference', () => {
  it('returns 0 for timestamps on the same local calendar day', () => {
    const date = new Date(2026, 5, 4, 0, 5)
    const referenceDate = new Date(2026, 5, 4, 23, 55)

    expect(getLocalCalendarDayDifference(date, referenceDate)).toBe(0)
  })

  it('returns 1 for yesterday even when less than 24 hours elapsed', () => {
    const date = new Date(2026, 5, 3, 23, 55)
    const referenceDate = new Date(2026, 5, 4, 0, 5)

    expect(getLocalCalendarDayDifference(date, referenceDate)).toBe(1)
  })

  it('returns negative days for future calendar dates', () => {
    const date = new Date(2026, 5, 6, 0, 5)
    const referenceDate = new Date(2026, 5, 4, 23, 55)

    expect(getLocalCalendarDayDifference(date, referenceDate)).toBe(-2)
  })

  it('counts calendar days across daylight saving time changes', () => {
    const date = new Date(2026, 2, 7, 12)
    const referenceDate = new Date(2026, 2, 9, 12)

    expect(getLocalCalendarDayDifference(date, referenceDate)).toBe(2)
  })
})

describe('formatRelativeTime', () => {
  it('returns today for timestamps earlier on the same calendar day', () => {
    expect(
      formatRelativeTime(new Date(2026, 5, 4, 0, 5).toISOString(), new Date(2026, 5, 4, 23, 55))
    ).toBe('today')
  })

  it('returns yesterday for timestamps on the previous calendar day', () => {
    expect(
      formatRelativeTime(new Date(2026, 5, 3, 23, 55).toISOString(), new Date(2026, 5, 4, 0, 5))
    ).toBe('yesterday')
  })
})

describe('formatCadence', () => {
  it('formats weekly correctly', () => {
    expect(formatCadence('weekly')).toBe('Weekly')
  })

  it('formats biweekly with hyphen', () => {
    expect(formatCadence('biweekly')).toBe('Bi-weekly')
  })

  it('formats monthly correctly', () => {
    expect(formatCadence('monthly')).toBe('Monthly')
  })

  it('formats quarterly correctly', () => {
    expect(formatCadence('quarterly')).toBe('Quarterly')
  })

  it('formats biannual with hyphen', () => {
    expect(formatCadence('biannual')).toBe('Bi-annual')
  })

  it('formats annual correctly', () => {
    expect(formatCadence('annual')).toBe('Annual')
  })

  it('returns dash for null input', () => {
    expect(formatCadence(null)).toBe('-')
  })

  it('returns dash for undefined input', () => {
    expect(formatCadence(undefined)).toBe('-')
  })

  it('returns dash for empty string', () => {
    expect(formatCadence('')).toBe('-')
  })

  it('returns original value for unknown cadence', () => {
    expect(formatCadence('daily')).toBe('daily')
    expect(formatCadence('unknown')).toBe('unknown')
  })
})
