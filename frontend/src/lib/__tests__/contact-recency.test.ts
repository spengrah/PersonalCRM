import { describe, expect, it } from 'vitest'
import { cadenceBaseDate, formatOverdueRecency } from '../contact-recency'

// spec: CAD-027[2]
// The overdue list's recency ordering answers "who have I gone longest without
// connecting with". A never-connected contact has waited since it was added, so it
// belongs in that ordering by its creation date — the same base the cadence engine
// uses (CAD-002[0]) — rather than sinking below everyone as an absent value.
describe('cadenceBaseDate', () => {
  it('interleaves never-connected contacts by how long they have waited', () => {
    const neverConnectedLongAgo = { created_at: '2026-01-01T00:00:00Z' }
    const connectedRecently = {
      created_at: '2025-01-01T00:00:00Z',
      last_contacted: '2026-07-01T00:00:00Z',
    }

    const order = [connectedRecently, neverConnectedLongAgo]
      .slice()
      .sort((a, b) => cadenceBaseDate(a) - cadenceBaseDate(b))

    expect(order[0]).toBe(neverConnectedLongAgo)
  })

  it('prefers the connection over the creation date when one exists', () => {
    expect(
      cadenceBaseDate({
        created_at: '2025-01-01T00:00:00Z',
        last_contacted: '2026-07-01T00:00:00Z',
      })
    ).toBe(new Date('2026-07-01T00:00:00Z').getTime())
  })
})

// spec: CAD-026[1]
// last_contacted records a two-way connection (CAD-006) and is unset until one
// happens (CON-001). The overdue card must not present a contact's creation instant
// as a connection — the app knew nothing had happened and said "Last contacted N ago"
// anyway, contradicting the same contact's detail page.
describe('formatOverdueRecency', () => {
  const now = new Date('2026-07-24T12:00:00Z')

  it('reports how long a never-connected contact has been on the list', () => {
    expect(formatOverdueRecency({ created_at: '2026-01-24T09:00:00Z' }, now)).toBe(
      'Added 6 months ago'
    )
  })

  it('reports the connection when one has happened', () => {
    expect(
      formatOverdueRecency(
        { created_at: '2026-01-24T09:00:00Z', last_contacted: '2026-07-17T09:00:00Z' },
        now
      )
    ).toBe('Last connected 7 days ago')
  })

  it('reads naturally on the same-day boundary', () => {
    expect(formatOverdueRecency({ created_at: '2026-07-24T01:00:00Z' }, now)).toBe('Added today')
    expect(
      formatOverdueRecency(
        { created_at: '2026-01-24T09:00:00Z', last_contacted: '2026-07-23T09:00:00Z' },
        now
      )
    ).toBe('Last connected yesterday')
  })

  it('returns nothing for an unparseable timestamp rather than rendering a broken phrase', () => {
    expect(formatOverdueRecency({ created_at: 'not-a-date' }, now)).toBe('')
  })
})
