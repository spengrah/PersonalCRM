import { describe, expect, it } from 'vitest'

import { renderCursorCell } from '../page'

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

  it('renders existing cursor for icloud_contacts when backfill_complete is false', () => {
    const out = renderCursorCell(
      'icloud_contacts',
      { backfill_complete: false, pushed_cursor: 'changeToken123' },
      { icloud_contacts: 99 }
    )
    expect(out).toBe('changeToken123')
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
})
