package replay

import (
	"context"
	"fmt"

	"personal-crm/backend/internal/google"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"
	"personal-crm/backend/internal/synthetic/factory"

	"github.com/google/uuid"
	calendarapi "google.golang.org/api/calendar/v3"
)

// GCalResult is the settled outcome of a GCal replay.
type GCalResult struct {
	ContactID   uuid.UUID
	GcalEventID string
	Matched     bool // false for MatchUnknown (unmatched attendee)
}

// ReplayGCal feeds a synthetic past calendar event through the REAL
// CalendarSyncProvider via the new calendarFetcher seam (fake fetcher, no OAuth)
// and settles. For MatchSeeded the seeded contact's email matches an attendee →
// calendar_event with the contact in matched_contact_ids + a calendar.attended
// interaction. For MatchUnknown the attendee is unmatched →
// matched_contact_ids='{}' + an external_contact import candidate.
//
// contactID is the seeded contact this event targets (for MatchSeeded). The
// caller must seed it with an email matching the attendee.
func (h *Harness) ReplayGCal(ctx context.Context, contactID uuid.UUID, spec factory.GCalEventSpec) (GCalResult, error) {
	// Calendar provider in cutover mode (real bus + pool) so past-event
	// calendar.attended publishes happen and the interaction_recorder writes the
	// interaction.
	provider := google.NewCalendarSyncProvider(
		nil, // oauthService unused with the injected fetcher
		repository.NewCalendarEventRepository(h.database.Queries),
		h.contactRepo,
		// identityService concrete is required by the constructor signature.
		h.gcalIdentityService(),
		h.externalRepo,
		h.bus,
		h.database.Pool,
	)
	provider.SetFetcherFactoryForTest(google.NewFakeCalendarFetcherFactoryForTest(google.FakeCalendarFetcherFuncs{
		ListEvents: func(_ context.Context, _ string, opts google.CalendarListOpts) ([]*calendarapi.Event, string, string, error) {
			if opts.PageToken != "" {
				return nil, "", "synth-sync-token", nil
			}
			return []*calendarapi.Event{spec.Event}, "", "synth-sync-token", nil
		},
	}))

	// calendar.attended root events are published by the provider per
	// (event, contact); they carry contact_id so the contact-scoped capture
	// covers them. The external_contact candidate (unknown path) writes no
	// contact-bearing event, so track the gcal source for direct capture too.
	h.track(func(c *created) { c.addDirectSource(google.CalendarSourceName) })

	accountID := spec.AccountID
	state := &repository.SyncState{Source: repository.InteractionSourceGCal, AccountID: &accountID}
	if _, err := provider.Sync(ctx, state, nil); err != nil {
		return GCalResult{}, fmt.Errorf("gcal sync: %w", err)
	}

	if spec.Intent == factory.MatchUnknown {
		predicate := func(ctx context.Context) (bool, error) {
			n, err := h.support.CountUnmatchedCalendarEventByGcalID(ctx, spec.GcalEventID)
			return n > 0, err
		}
		if err := h.Settle(ctx, predicate, ""); err != nil {
			return GCalResult{}, err
		}
		return GCalResult{GcalEventID: spec.GcalEventID, Matched: false}, nil
	}

	predicate := func(ctx context.Context) (bool, error) {
		return h.contactHasInteractionSource(ctx, contactID, repository.InteractionSourceGCal)
	}
	if err := h.Settle(ctx, predicate, ""); err != nil {
		return GCalResult{}, err
	}
	h.trackContactInteractions(ctx, contactID)
	return GCalResult{ContactID: contactID, GcalEventID: spec.GcalEventID, Matched: true}, nil
}

// gcalIdentityService returns the concrete IdentityService the calendar provider
// constructor requires (the harness holds it as *service.IdentityService).
func (h *Harness) gcalIdentityService() *service.IdentityService {
	return h.identityService
}
