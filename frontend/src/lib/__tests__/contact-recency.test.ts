import { describe, expect, it } from 'vitest'
import { cadenceBaseDate, formatOverdueRecency } from '../contact-recency'

// spec: CAD-027[2]
// The overdue list's recency ordering answers "who have I gone longest without
// connecting with". A never-connected contact has waited since it was added, so it
// belongs in that ordering by its creation date — the same base the cadence engine
// uses (CAD-002[0]) — rather than sinking below everyone as an absent value.
describe('cadenceBaseDate', () => {
  it('ranks a never-connected contact by its added date, interleaved among connected ones', () => {
    // The distinguishing claim: never-connected contacts are ordered BY when they
    // were added, not pinned first/last as a group. Mike (added Mar) must land
    // between Alice (connected Jan) and Bob (connected Jun) — a regression that
    // sorted null-last_contacted rows to an edge would fail this.
    const aliceConnectedJan = {
      created_at: '2024-01-01T00:00:00Z',
      last_contacted: '2026-01-01T00:00:00Z',
    }
    const mikeNeverConnectedMar = { created_at: '2026-03-01T00:00:00Z' }
    const bobConnectedJun = {
      created_at: '2024-01-01T00:00:00Z',
      last_contacted: '2026-06-01T00:00:00Z',
    }

    const order = [bobConnectedJun, mikeNeverConnectedMar, aliceConnectedJan]
      .slice()
      .sort((a, b) => cadenceBaseDate(a) - cadenceBaseDate(b))

    expect(order).toEqual([aliceConnectedJan, mikeNeverConnectedMar, bobConnectedJun])
  })

  it('prefers the connection over the creation date when one exists', () => {
    expect(
      cadenceBaseDate({
        created_at: '2025-01-01T00:00:00Z',
        last_contacted: '2026-07-01T00:00:00Z',
      })
    ).toBe(new Date('2026-07-01T00:00:00Z').getTime())
  })

  it('falls back to created_at when there is no connection', () => {
    expect(cadenceBaseDate({ created_at: '2026-03-01T00:00:00Z' })).toBe(
      new Date('2026-03-01T00:00:00Z').getTime()
    )
  })
})

// spec: CAD-026[1]
// last_contacted records a two-way connection (CAD-006) and is unset until one
// happens (CON-001). The overdue card must not present a contact's creation instant
// as a connection — the app knew nothing had happened and said "Last contacted N ago"
// anyway, contradicting the same contact's detail page.
describe('formatOverdueRecency', () => {
  const now = new Date('2026-07-24T12:00:00Z')
  // Noon-anchored fixtures: both endpoints sit near local midday, so the
  // local-calendar-day gap tracks the UTC gap within ±1 across all offsets. (It can
  // skew by 1 in offset ≥ +12h DST zones like Australia/Norfolk, where a 12:00Z
  // instant lands at 00:00 the next local day.) The N values are chosen bucket-distant
  // — far from the 7/30/365-day phrase boundaries — so a ±1 skew never flips the
  // rendered phrase, keeping these assertions offset-proof without a global TZ pin.
  // Keep that property when adding fixtures: do not pick N near a multiple of 7/30/365.
  const isoDaysAgo = (n: number) => new Date(now.getTime() - n * 86_400_000).toISOString()

  it('labels a never-connected contact by when it was ADDED, not as contact', () => {
    expect(formatOverdueRecency({ created_at: isoDaysAgo(200) }, now)).toBe('Added 6 months ago')
  })

  it('labels a connected contact by the connection, reading last_contacted not created_at', () => {
    // created 200d ago would read "6 months"; the phrase must reflect last_contacted (8d).
    expect(
      formatOverdueRecency({ created_at: isoDaysAgo(200), last_contacted: isoDaysAgo(8) }, now)
    ).toBe('Last connected 1 week ago')
  })

  it('renders grammatical singular units, never "1 weeks ago"', () => {
    // Regression guard for the grammar defect in the deleted inline formatter; the
    // card now delegates to the singular-aware shared formatRelativeTime.
    expect(formatOverdueRecency({ created_at: isoDaysAgo(8) }, now)).toBe('Added 1 week ago')
    expect(formatOverdueRecency({ created_at: isoDaysAgo(40) }, now)).toBe('Added 1 month ago')
  })

  it('reads naturally when the contact was added today', () => {
    expect(formatOverdueRecency({ created_at: isoDaysAgo(0) }, now)).toBe('Added today')
  })

  it('returns nothing for an unparseable timestamp rather than a broken phrase', () => {
    expect(formatOverdueRecency({ created_at: 'not-a-date' }, now)).toBe('')
  })
})
