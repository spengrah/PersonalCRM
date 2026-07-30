package synthetic

import (
	"testing"
	"time"
)

func TestBirthdayFixtureDate_LeapSafe(t *testing.T) {
	// A Feb-29 anchor: a today fixture must store Feb 29 (not roll to Mar 1 nor
	// clamp to Feb 28) so the page still classifies it as today, not celebrated.
	anchor := time.Date(2024, time.February, 29, 8, 0, 0, 0, time.UTC)
	bday := BirthdayFixtureDate(anchor, 0)
	if bday.Month() != time.February || bday.Day() != 29 {
		t.Fatalf("today fixture on a Feb-29 anchor stored %s, want Feb 29", bday.Format("01-02"))
	}
	if !isLeapYear(bday.Year()) {
		t.Errorf("birth year %d is not a leap year (Feb 29 would roll)", bday.Year())
	}
	if bday.Year() > anchor.Year()-30 {
		t.Errorf("birth year %d is younger than anchor.Year()-30 = %d", bday.Year(), anchor.Year()-30)
	}
	if got := BirthdayBucket(bday, anchor); got != "today" {
		t.Errorf("Feb-29 today fixture bucket = %q, want today", got)
	}
}

func TestBirthdayBucket_BoundaryOffsets(t *testing.T) {
	// Mid-year anchor so no annual-wrap or year-boundary interaction.
	anchor := time.Date(2026, time.June, 15, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		offset int
		want   string
	}{
		{0, "today"},
		{1, "week"},
		{7, "week"},
		{8, "distant"},
		{90, "distant"},
		{-3, "celebrated"},
	}
	for _, c := range cases {
		bday := BirthdayFixtureDate(anchor, c.offset)
		if got := BirthdayBucket(bday, anchor); got != c.want {
			t.Errorf("offset %d: bucket = %q, want %q (stored %s)", c.offset, got, c.want, bday.Format("01-02"))
		}
	}
}

func TestBirthdayFixtureDate_OffsetIsDateIndependent(t *testing.T) {
	// A fixture at offset o (0<=o<365) is always exactly o days out, regardless of
	// where in the year the anchor sits — which is what lets the pinned birthday
	// fixture's day count be stated as a contract rather than measured per reseed.
	// The PAGE SECTION is NOT date-independent: a +5 fixture seeded on Dec 27 wraps
	// to Jan 1, whose occurrence THIS year is already past, so the page files it
	// under "Already Celebrated This Year" though it is 5 days out. So we pin the
	// robust daysUntil==offset for forward fixtures (not a fragile section bucket),
	// and past-this-year for a backward one (which only holds when the offset stays
	// inside the anchor's calendar year, so it is checked only for anchors where it
	// does). The section→offset mapping is verified against the real page by the
	// frontend parity test. Anchors deliberately include the year-end boundary
	// (Dec 27–31, Jan 1) where the wrap bites, plus Feb 29 and a +90 that LANDS on
	// Feb 29.
	forward := []int{0, FixtureBirthdayOffsetDays, 1, 5, 7, 90, 150}
	for _, anchor := range []time.Time{
		time.Date(2026, time.January, 1, 12, 0, 0, 0, time.UTC),
		time.Date(2026, time.January, 2, 12, 0, 0, 0, time.UTC),
		time.Date(2026, time.March, 1, 12, 0, 0, 0, time.UTC),
		time.Date(2024, time.February, 29, 12, 0, 0, 0, time.UTC),
		time.Date(2026, time.July, 22, 12, 0, 0, 0, time.UTC),
		time.Date(2026, time.December, 20, 12, 0, 0, 0, time.UTC),
		time.Date(2026, time.December, 27, 12, 0, 0, 0, time.UTC),
		time.Date(2026, time.December, 31, 12, 0, 0, 0, time.UTC),
		time.Date(2023, time.December, 1, 12, 0, 0, 0, time.UTC), // +90 lands Feb 29 2024
	} {
		for _, offset := range forward {
			bday := BirthdayFixtureDate(anchor, offset)
			if got := BirthdayDaysUntil(bday, anchor); got != offset {
				t.Errorf("%s offset %d: daysUntil = %d, want %d",
					anchor.Format("2006-01-02"), offset, got, offset)
			}
		}
		// A backward offset reads as past-this-year only while it stays inside the
		// anchor's calendar year — in the first days of January a 3-days-ago birthday
		// belongs to the PREVIOUS year, so its next annual occurrence is ~362 days out
		// and the page classifies it distant. That date-dependence is the reason the
		// pinned fixture uses a forward offset.
		bday := BirthdayFixtureDate(anchor, -3)
		sameYear := anchor.UTC().AddDate(0, 0, -3).Year() == anchor.UTC().Year()
		if got := BirthdayIsPastThisYear(bday, anchor); got != sameYear {
			t.Errorf("%s offset -3: pastThisYear = %v, want %v (same calendar year = %v)",
				anchor.Format("2006-01-02"), got, sameYear, sameYear)
		}
	}
}
