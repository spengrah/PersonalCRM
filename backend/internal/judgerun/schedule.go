package judgerun

import (
	"context"
	"fmt"
	"time"

	"personal-crm/backend/internal/threadctx"
)

// Month is one calendar month in the server's timezone (time.Local), the
// timezone the CRM reads calendar days in.
type Month struct {
	start time.Time
}

// ParseMonth reads a month written YYYY-MM.
func ParseMonth(s string) (Month, error) {
	start, err := time.ParseInLocation("2006-01", s, time.Local)
	if err != nil {
		return Month{}, fmt.Errorf("month %q: want YYYY-MM", s)
	}
	return Month{start: start}, nil
}

// MonthOf is the month containing t.
func MonthOf(t time.Time) Month {
	t = t.In(time.Local)
	return Month{start: time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.Local)}
}

// Start is the month's first midnight.
func (m Month) Start() time.Time { return m.start }

// End is the next month's first midnight.
func (m Month) End() time.Time { return m.start.AddDate(0, 1, 0) }

func (m Month) String() string { return m.start.Format("2006-01") }

// CutOffs are the month's simulated nightly cut-offs: the midnight that closes
// each of its days, the last being End.
func (m Month) CutOffs() []time.Time {
	var cutOffs []time.Time
	for day := m.start; day.Before(m.End()); day = day.AddDate(0, 0, 1) {
		cutOffs = append(cutOffs, day.AddDate(0, 0, 1))
	}
	return cutOffs
}

// previousCutOff is the nightly cut-off immediately before cutOff: the
// midnight a day earlier for a nightly cut-off, which for a month's first is
// the month's start.
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

// eligible lists, at each cut-off of the month, the threads that gained
// content after the previous cut-off and up to and including this one. The
// first window opens at the month's start, so a thread whose only content
// precedes the month is never eligible. It is the one eligibility rule every
// run over a month uses.
func eligible(ctx context.Context, contexts *threadctx.Builder, month Month) ([]eligibility, error) {
	after := month.Start()
	var out []eligibility
	for _, cutOff := range month.CutOffs() {
		threads, err := contexts.ThreadsWithContentIn(ctx, after, cutOff)
		if err != nil {
			return nil, fmt.Errorf("threads with content up to %s: %w", cutOff.Format(time.RFC3339), err)
		}
		out = append(out, eligibility{cutOff: cutOff, threads: threads})
		after = cutOff
	}
	return out, nil
}
