package google

import (
	"context"
	"errors"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/repository"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/api/calendar/v3"
)

// fakeCalendarEvent builds a minimal accepted, timed *calendar.Event the
// keep-path will store (the user is an accepted attendee, not all-day, not
// cancelled). startOffset is relative to the accelerated anchor.
func fakeCalendarEvent(id, accountID string, startOffset time.Duration) *calendar.Event {
	start := accelerated.GetCurrentTime().Add(startOffset)
	return &calendar.Event{
		Id:      id,
		Summary: "synthetic meeting " + id,
		Status:  "confirmed",
		Start:   &calendar.EventDateTime{DateTime: start.Format(time.RFC3339)},
		End:     &calendar.EventDateTime{DateTime: start.Add(time.Hour).Format(time.RFC3339)},
		Attendees: []*calendar.EventAttendee{
			{Email: accountID, Self: true, ResponseStatus: "accepted"},
		},
	}
}

// newFetcherTestProvider builds a provider whose calendar repo is mocked (so we
// can assert upserts) and whose eventBus is nil (off mode — no publish, so the
// past-event hook is a no-op). The fake fetcher is injected via the new seam.
func newFetcherTestProvider(calRepo calendarRepoInterface) *CalendarSyncProvider {
	return &CalendarSyncProvider{
		calendarRepo: calRepo,
		contactRepo:  &mockContactRepo{},
	}
}

// TestCalendarFetcherSeam_InitialSync_PagesAllEvents proves the extracted
// calendarFetcher seam drives initialSync without OAuth: a two-page initial
// sync processes every event and records the final sync token as the cursor.
func TestCalendarFetcherSeam_InitialSync_PagesAllEvents(t *testing.T) {
	ctx := context.Background()
	const accountID = "user@example.com"

	mockCal := &mockCalendarRepo{}
	provider := newFetcherTestProvider(mockCal)

	var calls []CalendarListOpts
	provider.SetFetcherFactoryForTest(NewFakeCalendarFetcherFactoryForTest(FakeCalendarFetcherFuncs{
		ListEvents: func(_ context.Context, calendarID string, opts CalendarListOpts) ([]*calendar.Event, string, string, error) {
			assert.Equal(t, "primary", calendarID)
			calls = append(calls, opts)
			switch opts.PageToken {
			case "":
				return []*calendar.Event{
					fakeCalendarEvent("synth-ev-1", accountID, time.Hour),
					fakeCalendarEvent("synth-ev-2", accountID, 2*time.Hour),
				}, "page-2", "", nil
			case "page-2":
				return []*calendar.Event{
					fakeCalendarEvent("synth-ev-3", accountID, 3*time.Hour),
				}, "", "sync-token-final", nil
			default:
				t.Fatalf("unexpected page token %q", opts.PageToken)
				return nil, "", "", nil
			}
		},
	}))

	acct := accountID
	result, err := provider.Sync(ctx, &repository.SyncState{
		Source:    CalendarSourceName,
		AccountID: &acct,
	}, nil)
	require.NoError(t, err)
	assert.Equal(t, 3, result.ItemsProcessed)
	assert.Equal(t, "sync-token-final", result.NewCursor)
	// First page carried the time window + page token threading.
	require.Len(t, calls, 2)
	assert.True(t, calls[0].SingleEvents)
	assert.Equal(t, "startTime", calls[0].OrderBy)
	assert.Equal(t, int64(250), calls[0].MaxResults)
	assert.Equal(t, "", calls[0].PageToken)
	assert.Equal(t, "page-2", calls[1].PageToken)
	assert.True(t, mockCal.upsertCalled)
}

// TestCalendarFetcherSeam_IncrementalSync_UsesSyncToken proves incrementalSync
// threads the stored sync token and records the new one.
func TestCalendarFetcherSeam_IncrementalSync_UsesSyncToken(t *testing.T) {
	ctx := context.Background()
	const accountID = "user@example.com"

	mockCal := &mockCalendarRepo{}
	provider := newFetcherTestProvider(mockCal)

	var seenSyncToken string
	provider.SetFetcherFactoryForTest(NewFakeCalendarFetcherFactoryForTest(FakeCalendarFetcherFuncs{
		ListEvents: func(_ context.Context, _ string, opts CalendarListOpts) ([]*calendar.Event, string, string, error) {
			seenSyncToken = opts.SyncToken
			return []*calendar.Event{
				fakeCalendarEvent("synth-inc-1", accountID, time.Hour),
			}, "", "next-sync-token", nil
		},
	}))

	acct := accountID
	cursor := "prior-sync-token"
	result, err := provider.Sync(ctx, &repository.SyncState{
		Source:     CalendarSourceName,
		AccountID:  &acct,
		SyncCursor: &cursor,
	}, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, result.ItemsProcessed)
	assert.Equal(t, "prior-sync-token", seenSyncToken)
	assert.Equal(t, "next-sync-token", result.NewCursor)
}

// TestCalendarFetcherSeam_IncrementalSync_410FallsBackToInitial proves the
// 410/fullSyncRequired fallback path is preserved: a 410 on the incremental
// call re-runs the initial window sync.
func TestCalendarFetcherSeam_IncrementalSync_410FallsBackToInitial(t *testing.T) {
	ctx := context.Background()
	const accountID = "user@example.com"

	mockCal := &mockCalendarRepo{}
	provider := newFetcherTestProvider(mockCal)

	var sawInitialWindow bool
	provider.SetFetcherFactoryForTest(NewFakeCalendarFetcherFactoryForTest(FakeCalendarFetcherFuncs{
		ListEvents: func(_ context.Context, _ string, opts CalendarListOpts) ([]*calendar.Event, string, string, error) {
			if opts.SyncToken != "" {
				// Incremental attempt: simulate an expired sync token.
				return nil, "", "", errors.New("googleapi: Error 410: Sync token is no longer valid, fullSyncRequired")
			}
			// Fallback initial window: TimeMin/TimeMax are set, SyncToken empty.
			sawInitialWindow = true
			assert.NotEmpty(t, opts.TimeMin)
			assert.NotEmpty(t, opts.TimeMax)
			return []*calendar.Event{
				fakeCalendarEvent("synth-fb-1", accountID, time.Hour),
			}, "", "fresh-sync-token", nil
		},
	}))

	acct := accountID
	cursor := "expired-token"
	result, err := provider.Sync(ctx, &repository.SyncState{
		Source:     CalendarSourceName,
		AccountID:  &acct,
		SyncCursor: &cursor,
	}, nil)
	require.NoError(t, err)
	assert.True(t, sawInitialWindow, "410 should fall back to the initial window sync")
	assert.Equal(t, 1, result.ItemsProcessed)
	assert.Equal(t, "fresh-sync-token", result.NewCursor)
}
