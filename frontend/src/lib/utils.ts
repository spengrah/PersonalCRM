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
