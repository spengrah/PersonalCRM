/* eslint-disable @typescript-eslint/no-explicit-any */
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, within } from '@testing-library/react'

// The backend seeds clock-anchored birthday fixtures by offset-in-days from the
// reseed clock (synthetic/birthday_fixtures.go). Its Go classifier mirror agreeing
// with itself is circular — this test closes the loop against the REAL page: it
// renders the production birthdays page (reads frontend/src, does not modify it) and
// asserts the offset→section contract at the imminent/celebrated boundary as the
// page classifies it. If the page's classification drifts from the seed's offsets,
// this fails.

vi.mock('@/hooks/use-contacts', () => ({
  useContacts: vi.fn(),
}))

vi.mock('@/hooks/use-accelerated-time', () => ({
  useAcceleratedTime: vi.fn(),
}))

vi.mock('@/components/layout/navigation', () => ({
  Navigation: () => <div>Navigation</div>,
}))

import BirthdaysPage from '@/app/birthdays/page'
import { useContacts } from '@/hooks/use-contacts'
import { useAcceleratedTime } from '@/hooks/use-accelerated-time'
import type { Contact } from '@/types/contact'

// A mid-year anchor so celebrated applies and there is no Nov/Dec gift-planning
// wrinkle. Matches the backend's parity-friendly window.
const CURRENT_TIME = new Date(2026, 5, 15) // 2026-06-15, local

function isLeapYear(y: number): boolean {
  return y % 4 === 0 && (y % 100 !== 0 || y % 400 === 0)
}

function largestLeapYearOnOrBefore(y: number): number {
  while (!isLeapYear(y)) y--
  return y
}

// TS mirror of the Go BirthdayFixtureDate: the anchor's day shifted by offsetDays,
// taken as month/day, on a historical leap birth year → 'YYYY-MM-DD'. parseDateOnly
// interprets that as a local date, matching the page's local day math.
function fixtureBirthday(anchor: Date, offsetDays: number): string {
  const target = new Date(anchor.getFullYear(), anchor.getMonth(), anchor.getDate() + offsetDays)
  const birthYear = largestLeapYearOnOrBefore(anchor.getFullYear() - 30)
  const mm = String(target.getMonth() + 1).padStart(2, '0')
  const dd = String(target.getDate()).padStart(2, '0')
  return `${birthYear}-${mm}-${dd}`
}

function contact(name: string, offsetDays: number): Contact {
  return {
    id: `id-${name}`,
    full_name: name,
    birthday: fixtureBirthday(CURRENT_TIME, offsetDays),
  } as unknown as Contact
}

function sectionByHeading(pattern: RegExp): HTMLElement {
  const heading = screen.getByText(pattern)
  const section = heading.closest('section')
  if (!section) throw new Error(`no <section> for heading ${pattern}`)
  return section
}

describe('birthdays page offset→section parity', () => {
  beforeEach(() => {
    ;(useAcceleratedTime as any).mockReturnValue({
      currentTime: CURRENT_TIME,
      isAccelerated: false,
    })
    ;(useContacts as any).mockReturnValue({
      data: {
        contacts: [
          contact('Fixture Today', 0),
          contact('Fixture Imminent', 1),
          contact('Fixture Distant', 90),
          contact('Fixture Celebrated', -3),
        ],
      },
      isLoading: false,
      error: null,
    })
  })

  it('places each fixture in the section its offset implies', () => {
    render(<BirthdaysPage />)

    const todaySection = sectionByHeading(/Today's Birthdays/)
    expect(within(todaySection).getByText('Fixture Today')).toBeInTheDocument()
    expect(within(todaySection).getByText('🎉 Today!')).toBeInTheDocument()

    const upcomingSection = sectionByHeading(/Upcoming Birthdays/)
    expect(within(upcomingSection).getByText('Fixture Imminent')).toBeInTheDocument()
    expect(within(upcomingSection).getByText('Fixture Distant')).toBeInTheDocument()

    // The distant fixture must NOT be highlighted as today.
    expect(within(todaySection).queryByText('Fixture Distant')).not.toBeInTheDocument()

    const celebratedSection = sectionByHeading(/Already Celebrated This Year/)
    expect(within(celebratedSection).getByText('Fixture Celebrated')).toBeInTheDocument()
    expect(within(celebratedSection).getByText(/Turned/)).toBeInTheDocument()
  })
})
