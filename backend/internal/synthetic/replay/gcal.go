package replay

import (
	"context"
	"fmt"
	"time"

	"personal-crm/backend/internal/google"
	"personal-crm/backend/internal/identity"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/synthetic/factory"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	calendarapi "google.golang.org/api/calendar/v3"
)

// GCalResult is the settled outcome of a GCal replay.
type GCalResult struct {
	ContactID   uuid.UUID
	GcalEventID string
	Matched     bool // false for MatchUnknown (unmatched attendee)
}

// namespaceScopedCalendarRepo wraps the real calendar repository for the GCal
// replay so the provider's past-event publish loop is naturally scoped to THIS
// harness's events. The provider's updateLastContactedForPastEvents calls
// ListPastEventsNeedingUpdate to enumerate every past confirmed calendar_event
// DB-wide and publishes calendar.attended for each; on the shared test DB this
// races with other tests' calendar_events. This wrapper scopes that enumeration
// to events whose gcal_event_id carries the harness's synthetic prefix, so a
// replay in one namespace can never read, mark, or publish for another
// namespace's (or a real) event. All other methods pass through unchanged.
// Test-only — no production provider change.
//
// The scope is applied in SQL, not to the production query's result. That is a
// correctness requirement rather than an optimization: the production query's
// LIMIT binds BEFORE any Go-side filter could run, so a shared database holding a
// page's worth of older unprocessed rows from another namespace — which a crashed
// sibling worker strands exactly — would fill every page with foreign rows and
// hand this wrapper an empty local set on every retry. The replay would then
// starve and fail on the settle timeout, blaming the wrong thing entirely.
type namespaceScopedCalendarRepo struct {
	real   *repository.CalendarEventRepository
	prefix string
}

func (r *namespaceScopedCalendarRepo) Upsert(ctx context.Context, req repository.UpsertCalendarEventRequest) (*repository.CalendarEvent, error) {
	return r.real.Upsert(ctx, req)
}

func (r *namespaceScopedCalendarRepo) ListPastEventsNeedingUpdate(ctx context.Context, before time.Time, limit int32) ([]repository.CalendarEvent, error) {
	return r.real.ListPastEventsNeedingUpdateByPrefixForTest(ctx, before, r.prefix, limit)
}

func (r *namespaceScopedCalendarRepo) MarkLastContactedUpdated(ctx context.Context, id uuid.UUID) error {
	return r.real.MarkLastContactedUpdated(ctx, id)
}

func (r *namespaceScopedCalendarRepo) GetByGcalID(ctx context.Context, gcalEventID, gcalCalendarID, googleAccountID string) (*repository.CalendarEvent, error) {
	return r.real.GetByGcalID(ctx, gcalEventID, gcalCalendarID, googleAccountID)
}

func (r *namespaceScopedCalendarRepo) DeleteByGcalIDTx(ctx context.Context, tx pgx.Tx, gcalEventID, gcalCalendarID, googleAccountID string) error {
	return r.real.DeleteByGcalIDTx(ctx, tx, gcalEventID, gcalCalendarID, googleAccountID)
}

func (r *namespaceScopedCalendarRepo) MarkCancelledByGcalID(ctx context.Context, gcalEventID, gcalCalendarID, googleAccountID string) error {
	return r.real.MarkCancelledByGcalID(ctx, gcalEventID, gcalCalendarID, googleAccountID)
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
	// Confine the provider's DB-wide past-event enumeration
	// (ListPastEventsNeedingUpdate) to THIS harness's events so a concurrent
	// test's calendar_events on the shared DB are never read, marked, or
	// published for. The before/after behavior on the harness's own events is
	// unchanged.
	provider.SetCalendarRepoForTest(&namespaceScopedCalendarRepo{
		real:   repository.NewCalendarEventRepository(h.database.Queries),
		prefix: h.gen.Prefix(),
	})

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
	if err := h.assertContactVenue(ctx, contactID, repository.InteractionSourceGCal); err != nil {
		return GCalResult{}, err
	}
	return GCalResult{ContactID: contactID, GcalEventID: spec.GcalEventID, Matched: true}, nil
}

// GCalBatchItem is one calendar payload in a batch: the seeded contact it
// targets and the event. There is no PairKey: a matched calendar event is always
// MUTUAL, so calendar carries no promotion mechanic and therefore no dependency
// generations. A caller mapping a plan that CAN carry a pair key should assert
// it is unset for calendar rather than drop it — there is no field here to
// carry it, so a silent drop would lose a stated intent.
type GCalBatchItem struct {
	ContactID uuid.UUID
	Spec      factory.GCalEventSpec
}

// ReplayGCalBatch drives N calendar payloads through the CalendarSyncProvider and
// settles ONCE. One Sync genuinely carries the whole batch — the fake ListEvents
// returns every event on the first page — but the provider's past-event publish
// loop reads only gcalPastEventPageSize rows per Sync, so a batch above that
// needs several. Re-Syncing makes progress because MarkLastContactedUpdated
// permanently removes a processed event from that read.
//
// The loop polls the plain Gate A count between iterations, never Settle: a
// Settle whose predicate demands all N must time out on the first iteration when
// only a page can have landed. Settle runs exactly once, after the drive loop —
// so SyncCalls > 1 while SettleCalls stays 1.
func (h *Harness) ReplayGCalBatch(ctx context.Context, items []GCalBatchItem, opts ...BatchOption) (BatchResult, error) {
	const source = "gcal"

	options := applyBatchOptions(opts)

	entries, err := gcalBatchEntries(items)
	if err != nil {
		return BatchResult{}, err
	}
	if err := validateBatchStructure(source, entries); err != nil {
		return BatchResult{}, err
	}
	accountID, err := gcalBatchAccount(items)
	if err != nil {
		return BatchResult{}, err
	}
	if err := h.validateBatchOwnership(ctx, source, entries); err != nil {
		return BatchResult{}, err
	}

	contactIDs := distinctContactIDs(entries)
	res := BatchResult{Payloads: len(items), Contacts: len(contactIDs)}
	before, err := h.snapshotInteractionIDs(ctx, contactIDs)
	if err != nil {
		return res, err
	}

	// The provider publishes calendar.attended per (event, contact); those events
	// carry contact_id and are captured by the contact-scoped read, but track the
	// source as a backstop for any non-contact-bearing gcal root event.
	h.track(func(c *created) { c.addDirectSource(google.CalendarSourceName) })

	provider := google.NewCalendarSyncProvider(
		nil, // oauthService unused with the injected fetcher
		repository.NewCalendarEventRepository(h.database.Queries),
		h.contactRepo,
		h.identityService,
		h.externalRepo,
		h.bus,
		h.database.Pool,
	)
	allEvents := make([]*calendarapi.Event, 0, len(items))
	for _, it := range items {
		allEvents = append(allEvents, it.Spec.Event)
	}
	provider.SetFetcherFactoryForTest(google.NewFakeCalendarFetcherFactoryForTest(google.FakeCalendarFetcherFuncs{
		ListEvents: func(_ context.Context, _ string, opts google.CalendarListOpts) ([]*calendarapi.Event, string, string, error) {
			if opts.PageToken != "" {
				return nil, "", "synth-sync-token", nil
			}
			return allEvents, "", "synth-sync-token", nil
		},
	}))
	// Confine the provider's DB-wide past-event enumeration to THIS harness's
	// events, exactly as the single adapter does.
	provider.SetCalendarRepoForTest(&namespaceScopedCalendarRepo{
		real:   repository.NewCalendarEventRepository(h.database.Queries),
		prefix: h.gen.Prefix(),
	})

	gcalIDs, pairContactIDs := gcalBatchGateKeys(items)
	count := func(ctx context.Context) (int64, error) {
		return h.support.CountMatchedCalendarEventsByGcalIDs(ctx, gcalIDs, pairContactIDs)
	}
	drive := func(ctx context.Context) error {
		state := &repository.SyncState{Source: repository.InteractionSourceGCal, AccountID: &accountID}
		if _, err := provider.Sync(ctx, state, nil); err != nil {
			return fmt.Errorf("gcal sync: %w", err)
		}
		return nil
	}

	// Cap: one Sync per past-event page, plus two — one for the trailing partial
	// page and one of slack, so an off-by-one in the provider's page accounting
	// surfaces as a completed batch rather than a spurious failure.
	derivedCap := (len(items)+gcalPastEventPageSize-1)/gcalPastEventPageSize + 2
	syncCalls, driveErr := driveUntilCount(ctx, int64(len(items)), options.maxSyncs(derivedCap), drive, count)
	res.SyncCalls = syncCalls
	if driveErr != nil {
		return res, h.drainPartial(ctx, source, "", contactIDs, driveErr)
	}

	if err := h.Settle(ctx, h.gcalBatchSettled(gcalIDs, pairContactIDs), ""); err != nil {
		return res, h.drainPartial(ctx, source, "", contactIDs, err)
	}
	res.SettleCalls++

	res.Interactions = h.trackBatchInteractions(ctx, contactIDs, before)
	for _, contactID := range contactIDs {
		if err := h.assertContactVenue(ctx, contactID, repository.InteractionSourceGCal); err != nil {
			return res, err
		}
	}
	return res, nil
}

// ReplayGCalUpcoming drives N FUTURE calendar payloads through the
// CalendarSyncProvider and settles once.
//
// It is a separate adapter from ReplayGCalBatch because the two have different
// terminal states, not merely different data. A future matched event is stored
// and readable but publishes NOTHING: updateLastContactedForPastEvents is the
// sole calendar.attended publisher and its read requires end_time < now, so a
// future event creates no event row, no interaction and no venue node — and
// never acquires last_contacted_updated, which is exactly what the past-event
// gate demands. Driving future items on that gate would time out and report a
// drain failure over a world that is in fact complete.
//
// For the same reason ONE Sync carries any number of items: the per-Sync page
// budget belongs to the past-event publish loop, which future events never enter.
// There is no drain loop here.
//
// Mixing past and future items in ONE call is not supported; drive them in
// separate calls. That is safe because no reconciliation step deletes stored
// events a later fetch omits — the provider only processes what it lists. Order
// matters in one direction: Upsert writes last_contacted_updated = false on
// EVERY upsert, so a later Sync that re-lists an already-projected past event
// would reset its flag. Drive the past items first and do not re-list them.
func (h *Harness) ReplayGCalUpcoming(ctx context.Context, items []GCalBatchItem) (BatchResult, error) {
	const source = "gcal-upcoming"

	entries, err := gcalBatchEntries(items)
	if err != nil {
		return BatchResult{}, err
	}
	if err := validateBatchStructure(source, entries); err != nil {
		return BatchResult{}, err
	}
	accountID, err := gcalBatchAccount(items)
	if err != nil {
		return BatchResult{}, err
	}
	if err := h.validateBatchOwnership(ctx, source, entries); err != nil {
		return BatchResult{}, err
	}

	contactIDs := distinctContactIDs(entries)
	res := BatchResult{Payloads: len(items), Contacts: len(contactIDs)}

	provider := google.NewCalendarSyncProvider(
		nil, // oauthService unused with the injected fetcher
		repository.NewCalendarEventRepository(h.database.Queries),
		h.contactRepo,
		h.identityService,
		h.externalRepo,
		h.bus,
		h.database.Pool,
	)
	allEvents := make([]*calendarapi.Event, 0, len(items))
	for _, it := range items {
		allEvents = append(allEvents, it.Spec.Event)
	}
	provider.SetFetcherFactoryForTest(google.NewFakeCalendarFetcherFactoryForTest(google.FakeCalendarFetcherFuncs{
		ListEvents: func(_ context.Context, _ string, opts google.CalendarListOpts) ([]*calendarapi.Event, string, string, error) {
			if opts.PageToken != "" {
				return nil, "", "synth-sync-token", nil
			}
			return allEvents, "", "synth-sync-token", nil
		},
	}))
	// Confine the provider's DB-wide past-event enumeration to THIS harness's
	// events, exactly as the other two adapters do. These payloads are future, so
	// the enumeration finds none of them — but the provider runs it regardless,
	// and an unscoped run would read a concurrent namespace's rows.
	provider.SetCalendarRepoForTest(&namespaceScopedCalendarRepo{
		real:   repository.NewCalendarEventRepository(h.database.Queries),
		prefix: h.gen.Prefix(),
	})

	state := &repository.SyncState{Source: repository.InteractionSourceGCal, AccountID: &accountID}
	if _, err := provider.Sync(ctx, state, nil); err != nil {
		return res, fmt.Errorf("gcal upcoming sync: %w", err)
	}
	res.SyncCalls++

	gcalIDs, pairContactIDs := gcalBatchGateKeys(items)
	if err := h.Settle(ctx, h.gcalUpcomingSettled(gcalIDs, pairContactIDs), ""); err != nil {
		return res, h.drainPartial(ctx, source, "", contactIDs, err)
	}
	res.SettleCalls++
	// No interaction or venue tracking: a future event publishes nothing, so
	// there is no interaction to track and no venue node to reclaim. The
	// calendar_event rows are recovered by their namespace-prefixed
	// gcal_event_id, and the attendee identities by the identifier prefix.
	return res, nil
}

// gcalUpcomingSettled is the upcoming Gate A: every one of these (event, contact)
// pairs has a calendar_event carrying the contact in matched_contact_ids. No
// processed flag — see ReplayGCalUpcoming.
func (h *Harness) gcalUpcomingSettled(gcalEventIDs []string, contactIDs []uuid.UUID) gateA {
	want := int64(len(gcalEventIDs))
	return func(ctx context.Context) (bool, error) {
		n, err := h.support.CountLinkedCalendarEventsByGcalIDs(ctx, gcalEventIDs, contactIDs)
		return n >= want, err
	}
}

// gcalBatchSettled is the batch Gate A: every one of these (event, contact)
// pairs has a calendar_event carrying the contact in matched_contact_ids with
// last_contacted_updated set.
func (h *Harness) gcalBatchSettled(gcalEventIDs []string, contactIDs []uuid.UUID) gateA {
	want := int64(len(gcalEventIDs))
	return func(ctx context.Context) (bool, error) {
		n, err := h.support.CountMatchedCalendarEventsByGcalIDs(ctx, gcalEventIDs, contactIDs)
		return n >= want, err
	}
}

// gcalBatchGateKeys returns the batch's parallel (gcal event id, contact id)
// gate arrays, in item order.
func gcalBatchGateKeys(items []GCalBatchItem) ([]string, []uuid.UUID) {
	gcalIDs := make([]string, 0, len(items))
	contactIDs := make([]uuid.UUID, 0, len(items))
	for _, it := range items {
		gcalIDs = append(gcalIDs, it.Spec.GcalEventID)
		contactIDs = append(contactIDs, it.ContactID)
	}
	return gcalIDs, contactIDs
}

// gcalBatchEntries projects the typed items into the source-neutral view. There
// is no direction (a matched event is always mutual) and no PairKey; the
// addressed identifier is the non-self attendee, the address that decides
// whether the contact matches.
func gcalBatchEntries(items []GCalBatchItem) ([]batchEntry, error) {
	out := make([]batchEntry, 0, len(items))
	for i, it := range items {
		if it.Spec.Event == nil {
			return nil, fmt.Errorf("gcal: item %d has a nil event", i)
		}
		out = append(out, batchEntry{
			contactID:     it.ContactID,
			identifier:    it.Spec.GcalEventID,
			seeded:        it.Spec.Intent == factory.MatchSeeded,
			addressed:     gcalSpecPeerAttendee(it.Spec),
			addressedType: identity.IdentifierTypeEmail,
		})
	}
	return out, nil
}

// gcalSpecPeerAttendee returns the first non-self attendee address on an event
// ("" if there is none).
func gcalSpecPeerAttendee(spec factory.GCalEventSpec) string {
	if spec.Event == nil {
		return ""
	}
	for _, attendee := range spec.Event.Attendees {
		if attendee == nil || attendee.Self {
			continue
		}
		return attendee.Email
	}
	return ""
}

// gcalBatchAccount returns the single connected account the batch is driven
// under; a mixed-account batch would sync the wrong account's calendar.
func gcalBatchAccount(items []GCalBatchItem) (string, error) {
	account := items[0].Spec.AccountID
	for i, it := range items {
		if it.Spec.AccountID != account {
			return "", fmt.Errorf("gcal: item %d is on account %q but the batch is on %q: %w",
				i, it.Spec.AccountID, account, ErrBatchMixedAccounts)
		}
	}
	return account, nil
}
