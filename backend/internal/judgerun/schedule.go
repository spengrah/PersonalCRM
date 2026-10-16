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

// eachEligible builds, at each cut-off of the month, the context of every
// thread that gained content after the previous cut-off and up to and
// including this one, over the thread's whole content up to the cut-off, and
// hands it to fn. The first window opens at the month's start, so a thread
// whose only content precedes the month is never eligible. It is the one
// eligibility rule every run over a month uses.
func eachEligible(ctx context.Context, contexts *threadctx.Builder, month Month, fn func(threadctx.Context) error) error {
	after := month.Start()
	for _, cutOff := range month.CutOffs() {
		threads, err := contexts.ThreadsWithContentIn(ctx, after, cutOff)
		if err != nil {
			return fmt.Errorf("threads with content up to %s: %w", cutOff.Format(time.RFC3339), err)
		}
		for _, thread := range threads {
			c, err := contexts.Build(ctx, thread, cutOff)
			if err != nil {
				return fmt.Errorf("build thread %s at %s: %w", thread, cutOff.Format(time.RFC3339), err)
			}
			if err := fn(c); err != nil {
				return fmt.Errorf("thread %s at %s: %w", thread, cutOff.Format(time.RFC3339), err)
			}
		}
		after = cutOff
	}
	return nil
}
