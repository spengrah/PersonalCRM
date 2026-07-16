import { describe, expect, it } from 'vitest'

import { cursorCellState, renderCursorCell } from '../cursor-cell'

describe('renderCursorCell', () => {
  it('renders contact count for icloud_contacts when backfill_complete and counts loaded', () => {
    const out = renderCursorCell(
      'icloud_contacts',
      { backfill_complete: true },
      { icloud_contacts: 497 }
    )
    expect(out).toBe('497 contacts ✓')
  })

  it('falls back to dash for icloud_contacts when backfill_complete but counts missing', () => {
    const out = renderCursorCell('icloud_contacts', { backfill_complete: true }, undefined)
    expect(out).toBe('—')
  })

  it('falls back to dash for icloud_contacts when counts is loaded but key absent', () => {
    const out = renderCursorCell('icloud_contacts', { backfill_complete: true }, {})
    expect(out).toBe('—')
  })

  it('never renders the change-token for icloud_contacts while backfill is incomplete', () => {
    // Mid-backfill with a pushed cursor is the normal state the #327
    // complaint was about: the change-token must not be displayed.
    const out = renderCursorCell(
      'icloud_contacts',
      { backfill_complete: false, pushed_cursor: 'changeToken123' },
      { icloud_contacts: 99 }
    )
    expect(out).toBe('—')
  })

  it('renders the dash for icloud_contacts when backfill_complete is absent', () => {
    const out = renderCursorCell('icloud_contacts', { pushed_cursor: 'changeToken123' }, undefined)
    expect(out).toBe('—')
  })

  it('renders existing cursor for messages regardless of backfill_complete', () => {
    const out = renderCursorCell(
      'messages',
      { backfill_complete: true, pushed_cursor: '12345' },
      { messages: 999 } // count present but messages is NOT a backfill-progress source
    )
    expect(out).toBe('12345')
  })

  it('renders existing cursor for phone_calls regardless of backfill_complete', () => {
    const out = renderCursorCell(
      'phone_calls',
      { backfill_complete: true, pushed_cursor: 'zdate-z_pk-pair' },
      undefined
    )
    expect(out).toBe('zdate-z_pk-pair')
  })

  it('falls back to dash when no cursor and source not in backfill-progress set', () => {
    const out = renderCursorCell('messages', {}, undefined)
    expect(out).toBe('—')
  })

  it('prefers pushed_cursor over observed_cursor', () => {
    const out = renderCursorCell(
      'messages',
      { pushed_cursor: 'p', observed_cursor: 'o' },
      undefined
    )
    expect(out).toBe('p')
  })

  it('falls back to observed_cursor when pushed_cursor missing', () => {
    const out = renderCursorCell('messages', { observed_cursor: 'o' }, undefined)
    expect(out).toBe('o')
  })

  it('stringifies numeric pushed_cursor for messages (was rendering dash)', () => {
    const out = renderCursorCell('messages', { pushed_cursor: 207431 }, undefined)
    expect(out).toBe('207431')
  })

  it('renders only the ISO portion of phone_calls composite cursor', () => {
    const out = renderCursorCell(
      'phone_calls',
      { pushed_cursor: '2026-05-19T02:31:34Z:1966' },
      undefined
    )
    expect(out).toBe('2026-05-19T02:31:34Z')
  })

  it('falls back to raw cursor when phone_calls cursor cannot be parsed', () => {
    const out = renderCursorCell('phone_calls', { pushed_cursor: 'malformed-cursor' }, undefined)
    expect(out).toBe('malformed-cursor')
  })
})

describe('cursorCellState', () => {
  // State and rendering must agree: 'count' iff the count is rendered,
  // 'pending' iff the backfill-source placeholder is rendered, 'cursor'
  // for the ordinary cursor/dash rendering.
  it('is count exactly when the count renders', () => {
    const entry = { backfill_complete: true }
    const counts = { icloud_contacts: 3 }
    expect(cursorCellState('icloud_contacts', entry, counts)).toBe('count')
    expect(renderCursorCell('icloud_contacts', entry, counts)).toBe('3 contacts ✓')
  })

  it('is pending with the dash rendered while backfill is incomplete, even with a cursor', () => {
    const entry = { backfill_complete: false, pushed_cursor: 'changeToken123' }
    expect(cursorCellState('icloud_contacts', entry, undefined)).toBe('pending')
    expect(renderCursorCell('icloud_contacts', entry, undefined)).toBe('—')
  })

  it('is pending with the dash rendered when backfill is complete but counts are missing', () => {
    const entry = { backfill_complete: true }
    expect(cursorCellState('icloud_contacts', entry, {})).toBe('pending')
    expect(renderCursorCell('icloud_contacts', entry, {})).toBe('—')
  })

  it('is cursor for non-backfill sources', () => {
    expect(cursorCellState('messages', { pushed_cursor: '123' }, undefined)).toBe('cursor')
    expect(cursorCellState('messages', {}, undefined)).toBe('cursor')
  })
})
