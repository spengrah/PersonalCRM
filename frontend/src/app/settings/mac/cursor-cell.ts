export interface SourceHealthEntry {
  last_pushed_at?: string
  // Cursor fields are emitted by the daemon as the underlying JSON
  // type — number for sources with a numeric watermark (messages
  // ROWID) and string for composite cursors (phone_calls' `<ISO
  // ZDATE>:<Z_PK>` tuple). renderCursorCell handles both.
  observed_cursor?: string | number
  pushed_cursor?: string | number
  backfill_complete?: boolean
  last_error?: string
  [key: string]: unknown
}

/**
 * renderCursorCell decides what to display in the Cursor column for
 * a single source row (issue #327).
 *
 * The interim fix is narrow: when a source is in
 * `BACKFILL_PROGRESS_SOURCES` and its `backfill_complete` flag is
 * true, swap the dash for `<N> contacts ✓` where `N` is the live
 * external_contact row count for that host+source. Everything else
 * falls through to the previous `pushed_cursor ?? observed_cursor ??
 * '—'` rendering — so messages keeps its rowid, phone_calls renders
 * its own cursor, etc.
 *
 * Future sources that ship a 'caught up' indicator should add their
 * key to `BACKFILL_PROGRESS_SOURCES` rather than adding more
 * if-branches here.
 */
const BACKFILL_PROGRESS_SOURCES = new Set(['icloud_contacts'])

// Coerce a raw cursor value (string | number | undefined | null) to
// a displayable string. Returns undefined when there's nothing to
// show, so the caller can choose its own fallback.
function cursorToString(raw: unknown): string | undefined {
  if (raw === undefined || raw === null) return undefined
  if (typeof raw === 'string') return raw === '' ? undefined : raw
  if (typeof raw === 'number') return String(raw)
  return undefined
}

// Phone_calls writes its cursor as `<ISO ZDATE>:<Z_PK>` (e.g.
// `2026-05-19T02:31:34Z:1966`). Split off the ISO portion for a more
// operator-friendly display. Falls back to the raw cursor if parse
// fails — anything with a `T` and a `Z` followed by `:` is treated as
// a parseable composite.
function formatPhoneCallsCursor(raw: string): string {
  const match = raw.match(
    /^([0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(?:\.[0-9]+)?Z):.+$/
  )
  return match ? match[1] : raw
}

/**
 * cursorCellState classifies the Cursor cell for state-based test
 * assertions (exposed on the td as `data-state`): 'count' when a
 * backfill-complete source renders its live contact count, 'pending'
 * when a backfill-progress source has no count to show yet (backfill
 * incomplete, or counts not loaded), and 'cursor' for every other
 * source (raw cursor or dash rendering).
 */
export type CursorCellState = 'count' | 'pending' | 'cursor'

export function cursorCellState(
  source: string,
  entry: SourceHealthEntry,
  counts: Record<string, number> | undefined
): CursorCellState {
  if (BACKFILL_PROGRESS_SOURCES.has(source)) {
    if (entry.backfill_complete === true && typeof counts?.[source] === 'number') {
      return 'count'
    }
    return 'pending'
  }
  return 'cursor'
}

export function renderCursorCell(
  source: string,
  entry: SourceHealthEntry,
  counts: Record<string, number> | undefined
): string {
  if (BACKFILL_PROGRESS_SOURCES.has(source) && entry.backfill_complete === true) {
    const n = counts?.[source]
    if (typeof n === 'number') {
      return `${n} contacts ✓`
    }
    // counts not yet loaded / missing — graceful fallback to dash so
    // the row still renders. We deliberately don't fall through to
    // the pushed_cursor branch because an iCloud row's cursor is a
    // change-token, not a number (the whole point of #327).
    return '—'
  }
  const cursor = cursorToString(entry.pushed_cursor) ?? cursorToString(entry.observed_cursor)
  if (cursor === undefined) return '—'
  if (source === 'phone_calls') return formatPhoneCallsCursor(cursor)
  return cursor
}
