package scheduler

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"
)

type stubCorrespondenceScanner struct {
	since  time.Time
	called int
	count  int
	err    error
}

func (s *stubCorrespondenceScanner) Run(_ context.Context, since time.Time) (int, error) {
	s.called++
	s.since = since
	return s.count, s.err
}

func TestGmailCorrespondenceScanWorker_ComputesRecentWindow(t *testing.T) {
	const window = 120 * 24 * time.Hour
	scanner := &stubCorrespondenceScanner{count: 3}
	w := NewGmailCorrespondenceScanWorker(scanner, window)

	before := time.Now().Add(-window)
	err := w.Work(context.Background(), &river.Job[GmailCorrespondenceScanArgs]{})
	after := time.Now().Add(-window)

	require.NoError(t, err)
	require.Equal(t, 1, scanner.called)
	// since = now - window, bounded by the wall-clock window around the call.
	require.False(t, scanner.since.Before(before.Add(-time.Second)))
	require.False(t, scanner.since.After(after.Add(time.Second)))
}

func TestGmailCorrespondenceScanWorker_ErrorWrapped(t *testing.T) {
	scanner := &stubCorrespondenceScanner{err: errors.New("boom")}
	w := NewGmailCorrespondenceScanWorker(scanner, time.Hour)

	err := w.Work(context.Background(), &river.Job[GmailCorrespondenceScanArgs]{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "gmail correspondence scan")
}

func TestGmailCorrespondenceScanArgs_Kind(t *testing.T) {
	require.Equal(t, "gmail_correspondence_scan", GmailCorrespondenceScanArgs{}.Kind())
}
