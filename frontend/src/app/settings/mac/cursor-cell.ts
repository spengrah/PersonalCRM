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
 * Sources in `BACKFILL_PROGRESS_SOURCES` never display their raw
 * cursor: it is an opaque change-token that misleads the operator
 * (the original #327 complaint). Once `backfill_complete` is true and
 * the live count has loaded, the cell shows `<N> contacts ✓`; in every
 * other state (backfill in progress, or counts not loaded yet) it
 * shows the neutral dash. All other sources keep the
 * `pushed_cursor ?? observed_cursor ?? '—'` rendering — messages keeps
 * its rowid, phone_calls renders its own cursor, etc.
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
 * incomplete, or counts not loaded — renderCursorCell shows the
 * neutral dash for exactly these), and 'cursor' for every other
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
  if (BACKFILL_PROGRESS_SOURCES.has(source)) {
    if (entry.backfill_complete === true) {
      const n = counts?.[source]
      if (typeof n === 'number') {
        return `${n} contacts ✓`
      }
    }
    // Backfill in progress, or counts not yet loaded — neutral dash.
    // We deliberately never fall through to the cursor branch for
    // these sources: an iCloud row's cursor is a change-token, not a
    // number, and displaying it misled the operator (the whole point
    // of #327).
    return '—'
  }
  const cursor = cursorToString(entry.pushed_cursor) ?? cursorToString(entry.observed_cursor)
  if (cursor === undefined) return '—'
  if (source === 'phone_calls') return formatPhoneCallsCursor(cursor)
  return cursor
}
