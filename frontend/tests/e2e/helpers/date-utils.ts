/**
 * Get today's date in UTC formatted as M/D/YYYY.
 *
 * Use this when comparing dates in E2E tests. The backend stores timestamps in UTC,
 * and formatDateOnly extracts the UTC date portion (e.g., 2026-01-20T06:00:00Z → 1/20/2026).
 * Using local date methods like toLocaleDateString() will fail late at night when UTC
 * has already rolled over to the next day.
 */
export function getTodayUTC(): string {
  const now = new Date()
  return `${now.getUTCMonth() + 1}/${now.getUTCDate()}/${now.getUTCFullYear()}`
}
