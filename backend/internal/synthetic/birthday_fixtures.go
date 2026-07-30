package synthetic

import (
	"time"

	"personal-crm/backend/internal/synthetic/factory"
)

// BirthdayFixtureDate is the stored birthday date-only value for a fixture: the
// anchor's day shifted by offsetDays, taken as month/day, on a historical LEAP
// birth year (the largest leap year ≤ anchor.Year()-30). The leap birth year keeps
// the rendered age plausible (~30-33) AND preserves a Feb-29 target as Feb 29 — a
// Feb-28 clamp would turn the today fixture on a Feb-29 anchor into an
// already-celebrated one, since the page compares month/day in the current year. No
// clamping/normalization; UTC throughout.
func BirthdayFixtureDate(anchor time.Time, offsetDays int) time.Time {
	target := anchor.UTC().AddDate(0, 0, offsetDays)
	return time.Date(factory.LeapSafeBirthYear(anchor), target.Month(), target.Day(), 0, 0, 0, 0, time.UTC)
}

// BirthdayDaysUntil mirrors birthdays/page.tsx (getNextBirthday +
// calculateDaysUntilBirthday): days from today to the birthday's next annual
// occurrence, 0 when the birthday is today. today is the pinned UTC anchor day.
func BirthdayDaysUntil(bday, today time.Time) int {
	cur := startOfUTCDay(today)
	next := birthdayInYear(bday, cur.Year())
	if next.Before(cur) {
		next = birthdayInYear(bday, cur.Year()+1)
	}
	// Both operands are midnight UTC and UTC has no DST, so the gap is an exact
	// whole number of days — integer division matches the page's Math.ceil.
	return int(next.Sub(cur) / (24 * time.Hour))
}

// BirthdayIsPastThisYear mirrors birthdays/page.tsx (isBirthdayPastThisYear): the
// birthday's occurrence in the current year is strictly before today.
func BirthdayIsPastThisYear(bday, today time.Time) bool {
	cur := startOfUTCDay(today)
	return birthdayInYear(bday, cur.Year()).Before(cur)
}

// BirthdayBucket mirrors the birthdays-page section split: celebrated (past this
// year) → today (daysUntil==0) → week (≤7) → distant. Returns one of "celebrated",
// "today", "week", "distant".
func BirthdayBucket(bday, today time.Time) string {
	if BirthdayIsPastThisYear(bday, today) {
		return "celebrated"
	}
	switch d := BirthdayDaysUntil(bday, today); {
	case d == 0:
		return "today"
	case d <= 7:
		return "week"
	default:
		return "distant"
	}
}

func startOfUTCDay(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}

// isLeapYear is the calendar rule the leap-safe birth year rests on. The rule
// itself lives in factory.LeapSafeBirthYear (where both this package and the
// declare vocabulary can reach it); this predicate stays here so the fixture
// tests can state the property they are checking directly.
func isLeapYear(y int) bool {
	return y%4 == 0 && (y%100 != 0 || y%400 == 0)
}

// birthdayInYear projects a stored birthday onto a given year, matching the page's
// new Date(year, month, day): a Feb-29 birthday in a non-leap year rolls to Mar 1
// exactly as JS does, so the mirror stays faithful.
func birthdayInYear(bday time.Time, year int) time.Time {
	b := bday.UTC()
	return time.Date(year, b.Month(), b.Day(), 0, 0, 0, 0, time.UTC)
}
