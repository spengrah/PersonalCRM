package factory

import (
	"testing"
	"time"
)

func leapYear(y int) bool { return y%4 == 0 && (y%100 != 0 || y%400 == 0) }

// The year-unknown sentinel cannot express February 29 — 1900 is divisible by
// 100 and not by 400, so time.Date normalizes it to March 1. Storing a date
// other than the one asked for is a fixture lying about what it represents, and
// the panic is what makes that impossible to ship silently.
func TestWithBirthday1900Sentinel_PanicsOnFebruary29(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("WithBirthday1900Sentinel(February, 29) must panic — 1900 is not a leap year, so it would silently store March 1")
		}
	}()
	WithBirthday1900Sentinel(time.February, 29)
}

// Every date the sentinel CAN express must still round-trip untouched: a panic
// that fired on ordinary dates would be worse than the bug it replaces.
func TestWithBirthday1900Sentinel_EveryOtherDateRoundTrips(t *testing.T) {
	gen := NewGeneratorAt(DefaultSeed, "sentinel-ns", nameAnchor)
	for month := time.January; month <= time.December; month++ {
		daysInMonth := time.Date(1900, month+1, 0, 0, 0, 0, 0, time.UTC).Day()
		for day := 1; day <= daysInMonth; day++ {
			spec := gen.Contact(WithBirthday1900Sentinel(month, day))
			if spec.Birthday == nil {
				t.Fatalf("%s %d: no birthday stored", month, day)
			}
			if spec.Birthday.Month() != month || spec.Birthday.Day() != day {
				t.Errorf("%s %d stored as %s", month, day, spec.Birthday.Format("01-02"))
			}
			if spec.Birthday.Year() != sentinelBirthYear {
				t.Errorf("%s %d stored in year %d, want the year-unknown sentinel %d",
					month, day, spec.Birthday.Year(), sentinelBirthYear)
			}
		}
	}
}

func TestLeapSafeBirthYear(t *testing.T) {
	anchors := []time.Time{
		time.Date(2024, time.February, 29, 0, 0, 0, 0, time.UTC),
		time.Date(2026, time.July, 29, 0, 0, 0, 0, time.UTC),
		time.Date(2028, time.February, 29, 0, 0, 0, 0, time.UTC),
		// Century boundaries, where the divisible-by-100 rule bites: 1900 is not
		// a leap year but 2000 is.
		time.Date(1930, time.June, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2030, time.June, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2100, time.June, 1, 0, 0, 0, 0, time.UTC),
	}
	for _, anchor := range anchors {
		y := LeapSafeBirthYear(anchor)
		if !leapYear(y) {
			t.Errorf("anchor %s: birth year %d is not a leap year, so a Feb-29 birthday would roll", anchor.Format("2006-01-02"), y)
		}
		if y > anchor.Year()-30 {
			t.Errorf("anchor %s: birth year %d is younger than anchor-30 = %d", anchor.Format("2006-01-02"), y, anchor.Year()-30)
		}
		// LARGEST such leap year: nothing between y and the cutoff may be one.
		// The gap can legitimately exceed four years at a century boundary
		// (1900 is not a leap year, so an anchor of 1930 lands on 1896).
		for candidate := y + 1; candidate <= anchor.Year()-30; candidate++ {
			if leapYear(candidate) {
				t.Errorf("anchor %s: birth year %d skipped the nearer leap year %d",
					anchor.Format("2006-01-02"), y, candidate)
			}
		}
		// The property the whole thing exists for.
		bday := time.Date(y, time.February, 29, 0, 0, 0, 0, time.UTC)
		if bday.Month() != time.February || bday.Day() != 29 {
			t.Errorf("anchor %s: Feb 29 on birth year %d normalized to %s", anchor.Format("2006-01-02"), y, bday.Format("01-02"))
		}
	}
}
