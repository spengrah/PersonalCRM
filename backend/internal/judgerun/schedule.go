package judgerun

import (
	"context"
	"fmt"
	"time"

	"personal-crm/backend/internal/threadctx"
)

// dayLayout is how a day is written.
const dayLayout = "2006-01-02"

// Days is a range of whole calendar days in the server's timezone
// (time.Local), the timezone the CRM reads calendar days in: from its first
// day's opening midnight to the midnight that closes its last.
type Days struct {
	start, end time.Time
}

// ParseDays reads the range from its first and last day, each written
// YYYY-MM-DD and both included. It refuses a last day before the first.
func ParseDays(first, last string) (Days, error) {
	start, err := parseDay(first)
	if err != nil {
		return Days{}, err
	}
	lastDay, err := parseDay(last)
	if err != nil {
		return Days{}, err
	}
	if lastDay.Before(start) {
		return Days{}, fmt.Errorf("day range %s to %s: the last day is before the first", first, last)
	}
	return Days{start: start, end: lastDay.AddDate(0, 0, 1)}, nil
}

func parseDay(s string) (time.Time, error) {
	day, err := time.ParseInLocation(dayLayout, s, time.Local)
	if err != nil {
		return time.Time{}, fmt.Errorf("day %q: want YYYY-MM-DD", s)
	}
	return day, nil
}

// MonthOf is the days of the calendar month containing t.
func MonthOf(t time.Time) Days {
	t = t.In(time.Local)
	start := time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.Local)
	return Days{start: start, end: start.AddDate(0, 1, 0)}
}

// Start is the first day's opening midnight.
func (d Days) Start() time.Time { return d.start }

// End is the midnight that closes the last day.
func (d Days) End() time.Time { return d.end }

func (d Days) String() string {
	return d.start.Format(dayLayout) + ".." + d.end.AddDate(0, 0, -1).Format(dayLayout)
}

// CutOffs are the range's simulated nightly cut-offs: the midnight that closes
// each of its days, the last being End.
func (d Days) CutOffs() []time.Time {
	var cutOffs []time.Time
	for day := d.start; day.Before(d.End()); day = day.AddDate(0, 0, 1) {
		cutOffs = append(cutOffs, day.AddDate(0, 0, 1))
	}
	return cutOffs
}

// previousCutOff is the nightly cut-off immediately before cutOff: the
// midnight a day earlier for a nightly cut-off, which for a range's first is
// the range's start.
func previousCutOff(cutOff time.Time) time.Time {
	local := cutOff.In(time.Local)
	midnight := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.Local)
	if midnight.Equal(cutOff) {
		return midnight.AddDate(0, 0, -1)
	}
	return midnight
}

// eligibility is the threads that gained content between a nightly cut-off's
// previous cut-off and the cut-off, up to and including it.
type eligibility struct {
	cutOff  time.Time
	threads []threadctx.VenueID
}

// eligible lists, at each cut-off of the range, the threads that gained
// content after the previous cut-off and up to and including this one. The
// first window opens at the range's start, so a thread whose only content
// precedes the range is never eligible. It is the one eligibility rule every
// run over a range uses.
func eligible(ctx context.Context, contexts *threadctx.Builder, days Days) ([]eligibility, error) {
	after := days.Start()
	var out []eligibility
	for _, cutOff := range days.CutOffs() {
		threads, err := contexts.ThreadsWithContentIn(ctx, after, cutOff)
		if err != nil {
			return nil, fmt.Errorf("threads with content up to %s: %w", cutOff.Format(time.RFC3339), err)
		}
		out = append(out, eligibility{cutOff: cutOff, threads: threads})
		after = cutOff
	}
	return out, nil
}
