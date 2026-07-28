package declare

import (
	"fmt"
	"time"
)

// Amount is a domain-unit duration: whole days plus whole cadence periods. It
// resolves to a wall duration only against the active environment's cadence
// table (facts.go), so the same declaration means "3 days" in production and in
// the compressed CRM_ENV=testing world.
//
// The zero Amount is a valid "no offset" value; Days and Periods are the
// constructors and they compose additively via Plus.
type Amount struct {
	days    int
	periods int
}

// Days is n whole days, where a day is the active table's weekly period / 7.
func Days(n int) Amount { return Amount{days: n} }

// Periods is n whole periods of the contact's OWN cadence. It requires the
// contact to declare a cadence; Register rejects a period-bearing Amount on a
// cadence-less contact.
func Periods(n int) Amount { return Amount{periods: n} }

// Plus adds two amounts (e.g. Periods(1).Plus(Days(3))).
func (a Amount) Plus(b Amount) Amount {
	return Amount{days: a.days + b.days, periods: a.periods + b.periods}
}

// needsCadence reports whether resolving this amount requires a cadence.
func (a Amount) needsCadence() bool { return a.periods != 0 }

// negative reports whether either component is negative. A negative amount
// would date a fixture forward rather than back, which is always a mis-stated
// declaration, so Register rejects it.
func (a Amount) negative() bool { return a.days < 0 || a.periods < 0 }

// resolve converts the amount to a wall duration against the active cadence
// table. cadence may be empty only when the amount carries no periods.
func (a Amount) resolve(cadence string) time.Duration {
	return time.Duration(a.days)*dayLength() + time.Duration(a.periods)*period(cadence)
}

// String renders the amount in its declared domain units (never resolved), so
// error messages read the way the declaration was written.
func (a Amount) String() string {
	switch {
	case a.days != 0 && a.periods != 0:
		return fmt.Sprintf("%dd+%dp", a.days, a.periods)
	case a.periods != 0:
		return fmt.Sprintf("%dp", a.periods)
	default:
		return fmt.Sprintf("%dd", a.days)
	}
}
