package replay

import (
	"context"
	"fmt"

	"personal-crm/backend/internal/google"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
)

// GCalSyncSession drives the REAL CalendarSyncProvider through the Element-1
// harness wiring (cutover bus + pool + the namespace-scoped calendar-repo
// wrapper + the injected fake fetcher) across MULTIPLE Sync calls, threading the
// returned sync token back in as the next cursor. The single-event ReplayGCal
// adapter cannot express the incremental / 410-fallback / decline / repeated-
// initial flows the E2 provider tests assert, so those tests build a session and
// call Sync per step.
//
// The session reuses the EXACT provider wiring ReplayGCal does (so namespace
// isolation via the prefix-scoped calendar repo is preserved); it differs only
// in exposing the fetcher behavior + cursor threading to the caller. Test-only —
// no production provider change.
type GCalSyncSession struct {
	h         *Harness
	provider  *google.CalendarSyncProvider
	accountID string
	cursor    string
}

// NewGCalSyncSession builds a session bound to this harness's bus/pool/repos for
// the given synthetic account id. The caller drives Sync (supplying a fetcher
// behavior per call) and then settles on a Gate-A predicate via the harness.
func (h *Harness) NewGCalSyncSession(accountID string) *GCalSyncSession {
	provider := google.NewCalendarSyncProvider(
		nil, // oauthService unused with the injected fetcher
		repository.NewCalendarEventRepository(h.database.Queries),
		h.contactRepo,
		h.identityService,
		h.externalRepo,
		h.bus,
		h.database.Pool,
	)
	// Confine the provider's DB-wide past-event enumeration to THIS harness's
	// events (the same prefix-scoped wrapper ReplayGCal installs), so a
	// concurrent test's calendar_events on the shared DB are never read,
	// marked, or published for.
	provider.SetCalendarRepoForTest(&namespaceScopedCalendarRepo{
		real:   repository.NewCalendarEventRepository(h.database.Queries),
		prefix: h.gen.Prefix(),
	})
	// Track the gcal source so the harness's cleanup event-id capture sweeps any
	// non-contact-bearing root events (mirrors ReplayGCal's backstop).
	h.track(func(c *created) { c.addDirectSource(google.CalendarSourceName) })
	return &GCalSyncSession{h: h, provider: provider, accountID: accountID}
}

// Sync runs ONE provider.Sync with the supplied fetcher behavior, threading the
// session's current cursor in and capturing the returned sync token as the next
// cursor. The fetcher's ListEvents closure receives the CalendarListOpts the
// provider builds (so the test can branch on PageToken / SyncToken to emulate
// initial vs incremental pages, a 410 on the sync-token call, etc.).
func (s *GCalSyncSession) Sync(ctx context.Context, funcs google.FakeCalendarFetcherFuncs) error {
	s.provider.SetFetcherFactoryForTest(google.NewFakeCalendarFetcherFactoryForTest(funcs))
	state := &repository.SyncState{Source: repository.InteractionSourceGCal, AccountID: &s.accountID}
	if s.cursor != "" {
		cursor := s.cursor
		state.SyncCursor = &cursor
	}
	res, err := s.provider.Sync(ctx, state, nil)
	if err != nil {
		return fmt.Errorf("gcal session sync: %w", err)
	}
	if res != nil && res.NewCursor != "" {
		s.cursor = res.NewCursor
	}
	return nil
}

// Cursor returns the most recent sync token the provider produced (the value a
// real scheduler would persist + feed to the next Sync). Tests assert it is
// non-empty after a settled sync.
func (s *GCalSyncSession) Cursor() string { return s.cursor }

// SettleMatched settles on the seeded Gate-A predicate (calendar_event for the
// gcal id has the contact in matched_contact_ids + last_contacted_updated) and
// tracks the contact's interactions for cleanup. Use after a Sync that should
// produce a matched, settled event.
func (s *GCalSyncSession) SettleMatched(ctx context.Context, gcalEventID string, contactID uuid.UUID) error {
	if err := s.h.Settle(ctx, s.h.gcalSettled(gcalEventID, contactID), ""); err != nil {
		return err
	}
	s.h.trackContactInteractions(ctx, contactID)
	return nil
}

// SettleUnmatched settles on the unmatched-attendee Gate-A predicate
// (calendar_event for the gcal id has an empty matched_contact_ids array).
func (s *GCalSyncSession) SettleUnmatched(ctx context.Context, gcalEventID string) error {
	predicate := func(ctx context.Context) (bool, error) {
		n, err := s.h.support.CountUnmatchedCalendarEventByGcalID(ctx, gcalEventID)
		return n > 0, err
	}
	return s.h.Settle(ctx, predicate, "")
}

// SettleDeclined settles on the decline TERMINAL state: no calendar_event row
// remains for the gcal id (the cutover decline branch deleted it) AND the
// calendar.declined River job has finalized (Gate B), so the real
// CalendarDeclineHandler consumer has soft-deleted the derived interaction.
func (s *GCalSyncSession) SettleDeclined(ctx context.Context, gcalEventID string, contactID uuid.UUID) error {
	predicate := func(ctx context.Context) (bool, error) {
		n, err := s.h.support.CountCalendarEventByGcalID(ctx, gcalEventID)
		return n == 0, err
	}
	// Gate B keys on this contact, so it waits out the calendar.declined job.
	return s.h.Settle(ctx, predicate, "")
}

// CountImportCandidateByEmailPrefix counts unmatched external_contact import
// candidates whose email is in this namespace (matched by the ns prefix on the
// stored email). Tests assert the GCal unmatched-attendee path created the
// import-candidate row.
func (s *GCalSyncSession) CountImportCandidateByEmailPrefix(ctx context.Context) (int64, error) {
	return s.h.support.CountUnmatchedExternalContactByEmailPrefix(ctx, s.h.gen.Prefix())
}
