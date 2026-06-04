/**
 * Parse a date string (YYYY-MM-DD or ISO format) as a local date without timezone conversion.
 *
 * This fixes the common bug where `new Date("2024-01-15T00:00:00Z")` gets converted
 * to the user's local timezone, potentially shifting the date by one day.
 *
 * @param dateString - Date string in YYYY-MM-DD or ISO format
 * @returns Date object representing the date in local timezone, or null if invalid
 */
export function parseDateOnly(dateString: string | undefined | null): Date | null {
  if (!dateString) return null

  // Extract just the date part (YYYY-MM-DD) regardless of format
  const datePart = dateString.split('T')[0]
  const [year, month, day] = datePart.split('-').map(Number)

  if (!year || !month || !day) return null

  // Create date using local timezone (month is 0-indexed)
  return new Date(year, month - 1, day)
}

/**
 * Format a date-only string for display without timezone issues.
 *
 * @param dateString - Date string in YYYY-MM-DD or ISO format
 * @param options - Intl.DateTimeFormat options
 * @returns Formatted date string, or empty string if invalid
 */
export function formatDateOnly(
  dateString: string | undefined | null,
  options: Intl.DateTimeFormatOptions = { year: 'numeric', month: 'short', day: 'numeric' }
): string {
  const date = parseDateOnly(dateString)
  if (!date) return ''
  return date.toLocaleDateString(undefined, options)
}

const MS_PER_DAY = 1000 * 60 * 60 * 24

export function getLocalCalendarDayDifference(date: Date, referenceDate: Date): number {
  const dateDay = Date.UTC(date.getFullYear(), date.getMonth(), date.getDate())
  const referenceDay = Date.UTC(
    referenceDate.getFullYear(),
    referenceDate.getMonth(),
    referenceDate.getDate()
  )

  return Math.round((referenceDay - dateDay) / MS_PER_DAY)
}

// Imported year-less birthdays use this sentinel year until the data model can represent them explicitly.
export const PLACEHOLDER_BIRTHDAY_YEAR = 1900

export function isPlaceholderBirthday(dateString: string | undefined | null): boolean {
  const date = parseDateOnly(dateString)
  return date?.getFullYear() === PLACEHOLDER_BIRTHDAY_YEAR
}

export function formatBirthday(
  dateString: string | undefined | null,
  options: Intl.DateTimeFormatOptions = { year: 'numeric', month: 'short', day: 'numeric' }
): string {
  if (!isPlaceholderBirthday(dateString)) {
    return formatDateOnly(dateString, options)
  }

  const monthDayOptions = { ...options }
  delete monthDayOptions.year

  if (!monthDayOptions.month && !monthDayOptions.day) {
    monthDayOptions.month = 'short'
    monthDayOptions.day = 'numeric'
  }

  return formatDateOnly(dateString, monthDayOptions)
}

/**
 * Format a cadence value for display.
 *
 * @param cadence - Cadence value (weekly, biweekly, monthly, quarterly, biannual, annual)
 * @returns Formatted cadence string with proper capitalization
 */
/**
 * Format a timestamp as a relative time string (e.g., "3 days ago", "today").
 *
 * @param dateString - ISO timestamp string
 * @returns Relative time string, or empty string if invalid
 */
export function formatRelativeTime(
  dateString: string | undefined | null,
  referenceDate: Date = new Date()
): string {
  if (!dateString) return ''
  const date = new Date(dateString)
  if (isNaN(date.getTime())) return ''

  const diffDays = getLocalCalendarDayDifference(date, referenceDate)

  if (diffDays < 0) return 'in the future'
  if (diffDays === 0) return 'today'
  if (diffDays === 1) return 'yesterday'
  if (diffDays < 7) return `${diffDays} days ago`
  if (diffDays < 14) return '1 week ago'
  if (diffDays < 30) return `${Math.floor(diffDays / 7)} weeks ago`
  if (diffDays < 60) return '1 month ago'
  if (diffDays < 365) return `${Math.floor(diffDays / 30)} months ago`
  return `${Math.floor(diffDays / 365)} year${Math.floor(diffDays / 365) > 1 ? 's' : ''} ago`
}

export function formatCadence(cadence: string | undefined | null): string {
  if (!cadence) return '-'
  const labels: Record<string, string> = {
    weekly: 'Weekly',
    biweekly: 'Bi-weekly',
    monthly: 'Monthly',
    quarterly: 'Quarterly',
    biannual: 'Bi-annual',
    annual: 'Annual',
  }
  return labels[cadence] || cadence
}
