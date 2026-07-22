package synthetic

import (
	"testing"
	"time"
)

func TestBirthdayFixturePlan_Ordering(t *testing.T) {
	anchor := time.Date(2026, time.June, 15, 9, 0, 0, 0, time.UTC)
	plan := BirthdayFixturePlan(anchor)

	// The strict {today, +1, distant} triple is always the first three, in order.
	if len(plan) < 3 {
		t.Fatalf("plan must carry the strict triple, got %d fixtures", len(plan))
	}
	want := []struct {
		offset int
		role   string
	}{
		{0, BirthdayRoleToday},
		{1, BirthdayRoleImminent},
		{90, BirthdayRoleDistant},
		{5, BirthdayRoleThisWeek},
		{150, BirthdayRoleDistant2},
		{-3, BirthdayRoleCelebrated},
	}
	for i, w := range want {
		if plan[i].OffsetDays != w.offset || plan[i].Role != w.role {
			t.Errorf("plan[%d] = {%d, %q}, want {%d, %q}", i, plan[i].OffsetDays, plan[i].Role, w.offset, w.role)
		}
	}
}

func TestBirthdayFixturePlan_CelebratedDateGating(t *testing.T) {
	// In the first three days of January a 3-days-ago birthday falls in the
	// previous calendar year, so celebrated is dropped and the plan is length 5.
	for _, day := range []int{1, 2, 3} {
		anchor := time.Date(2026, time.January, day, 12, 0, 0, 0, time.UTC)
		if got := len(BirthdayFixturePlan(anchor)); got != 5 {
			t.Errorf("Jan %d: plan length = %d, want 5 (celebrated gated out)", day, got)
		}
	}
	// From January 4 onward (and every other day of the year) celebrated applies.
	for _, anchor := range []time.Time{
		time.Date(2026, time.January, 4, 12, 0, 0, 0, time.UTC),
		time.Date(2026, time.June, 15, 12, 0, 0, 0, time.UTC),
		time.Date(2026, time.December, 31, 12, 0, 0, 0, time.UTC),
	} {
		if got := len(BirthdayFixturePlan(anchor)); got != 6 {
			t.Errorf("%s: plan length = %d, want 6 (celebrated applies)", anchor.Format("2006-01-02"), got)
		}
	}
}

func TestBirthdayFixtureDate_LeapSafe(t *testing.T) {
	// A Feb-29 anchor: the today fixture must store Feb 29 (not roll to Mar 1 nor
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

func TestBirthdaylessCatalogCount(t *testing.T) {
	cases := map[int]int{5: 3, 18: 13, 150: 119}
	for n, want := range cases {
		if got := BirthdaylessCatalogCount(n); got != want {
			t.Errorf("BirthdaylessCatalogCount(%d) = %d, want %d", n, got, want)
		}
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

func TestBirthdayFixtureDate_PlanRolesLandAsIntended(t *testing.T) {
	// Every fixture's next-occurrence distance is DATE-INDEPENDENT: a fixture at
	// offset o (0<=o<365) is always exactly o days out, regardless of where in the
	// year the anchor sits. The PAGE SECTION is NOT date-independent — a +5 fixture
	// seeded on Dec 27 wraps to Jan 1, whose occurrence THIS year is already past, so
	// the page files it under "Already Celebrated This Year" though it is 5 days out.
	// So we pin the robust daysUntil==offset for the forward fixtures (not a fragile
	// section bucket), and past-this-year for the celebrated fixture (which its
	// date-gating keeps same-year). The section→offset mapping is verified against the
	// real page by the frontend parity test. Anchors deliberately include the
	// year-end boundary (Dec 27–31, Jan 1) where the wrap bites.
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
		for _, f := range BirthdayFixturePlan(anchor) {
			bday := BirthdayFixtureDate(anchor, f.OffsetDays)
			if f.OffsetDays >= 0 {
				if got := BirthdayDaysUntil(bday, anchor); got != f.OffsetDays {
					t.Errorf("%s role %q: daysUntil = %d, want %d (offset)",
						anchor.Format("2006-01-02"), f.Role, got, f.OffsetDays)
				}
			} else if !BirthdayIsPastThisYear(bday, anchor) {
				t.Errorf("%s role %q (offset %d): want past-this-year (celebrated)",
					anchor.Format("2006-01-02"), f.Role, f.OffsetDays)
			}
		}
	}
}
