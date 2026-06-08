package replay

import (
	"context"
	"fmt"

	"personal-crm/backend/internal/google"
	"personal-crm/backend/internal/repository"
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
		h.identityService,
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

	accountID := spec.AccountID
	state := &repository.SyncState{Source: repository.InteractionSourceGCal, AccountID: &accountID}
	if _, err := provider.Sync(ctx, state, nil); err != nil {
		return GCalResult{}, fmt.Errorf("gcal sync: %w", err)
	}

	if spec.Intent == factory.MatchUnknown {
		// Unmatched attendee: no matched contact, so the provider publishes NO
		// calendar.attended event. The calendar_event + external_contact rows are
		// cleaned by ns-prefix, not event capture, so nothing to track here.
		predicate := func(ctx context.Context) (bool, error) {
			n, err := h.support.CountUnmatchedCalendarEventByGcalID(ctx, spec.GcalEventID)
			return n > 0, err
		}
		if err := h.Settle(ctx, predicate, ""); err != nil {
			return GCalResult{}, err
		}
		return GCalResult{GcalEventID: spec.GcalEventID, Matched: false}, nil
	}

	// Seeded path: the provider publishes calendar.attended per (event, contact);
	// those events carry contact_id and are captured by the contact-scoped read,
	// but track the source too as a backstop for any non-contact-bearing gcal
	// root event a future change might publish (its source_id is synth-prefixed).
	h.track(func(c *created) { c.addDirectSource(google.CalendarSourceName) })

	// Gate A: the calendar_event is processed with this contact matched.
	if err := h.Settle(ctx, h.gcalSettled(spec.GcalEventID, contactID), ""); err != nil {
		return GCalResult{}, err
	}
	h.trackContactInteractions(ctx, contactID)
	return GCalResult{ContactID: contactID, GcalEventID: spec.GcalEventID, Matched: true}, nil
}
