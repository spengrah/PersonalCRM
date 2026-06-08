package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/cadence"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
)

// TestSeedService is the behavior-preserving home for the /test/seed/* + /cleanup
// route bodies. The handlers parse + validate HTTP input, then call these methods
// to construct the synthetic rows through repositories/services — so the handler
// no longer inlines repo/query calls (and /cleanup no longer calls db.New(tx)
// queries directly, fixing the standing handler→queries layer violation).
//
// The HTTP request/response contracts are preserved byte-for-byte: these methods
// take the SAME normalized inputs the handlers produced and call the SAME
// repositories/services with the SAME arguments. The 23 E2E specs are the
// behavior-preservation gate.
//
// It depends only on the contact service + targeted repositories (it does NOT
// import the synthetic package — the profile/replay world is CLI-only, and
// service must not import synthetic to avoid a service→synthetic→replay→service
// cycle). All values are caller-supplied/validated at the HTTP boundary, so no
// factory-generated defaults are injected here — that would change behavior.
type TestSeedService struct {
	database        *db.Database
	externalRepo    *repository.ExternalContactRepository
	contactSvc      *ContactService
	calendarRepo    *repository.CalendarEventRepository
	macHostRepo     *repository.MacHostRepository
	meetingNoteRepo *repository.MeetingNoteRepository
}

// NewTestSeedService constructs the test-seed service.
func NewTestSeedService(
	database *db.Database,
	externalRepo *repository.ExternalContactRepository,
	contactSvc *ContactService,
	calendarRepo *repository.CalendarEventRepository,
	macHostRepo *repository.MacHostRepository,
	meetingNoteRepo *repository.MeetingNoteRepository,
) *TestSeedService {
	return &TestSeedService{
		database:        database,
		externalRepo:    externalRepo,
		contactSvc:      contactSvc,
		calendarRepo:    calendarRepo,
		macHostRepo:     macHostRepo,
		meetingNoteRepo: meetingNoteRepo,
	}
}

// --- /test/seed/contacts ---------------------------------------------------

// SeedContactInput is one contact to seed. FullName is already prefix-prepended
// by the caller; Methods are already normalized + built.
type SeedContactInput struct {
	FullName             string
	Location             string
	Cadence              string
	Methods              []ContactMethodInput
	LastContactedDaysAgo int
}

// SeedContacts creates contacts with env-scaled last_contacted backdating
// (preserving the route's exact scaling: 1 "day" = weekly_cadence / 7).
func (s *TestSeedService) SeedContacts(ctx context.Context, inputs []SeedContactInput) ([]string, error) {
	now := accelerated.GetCurrentTime()
	ids := make([]string, 0, len(inputs))

	for _, input := range inputs {
		lastContacted := now
		if input.LastContactedDaysAgo > 0 {
			weeklyDuration := cadence.GetCadenceDuration(cadence.CadenceWeekly)
			scaledDayDuration := weeklyDuration / 7
			lastContacted = now.Add(-time.Duration(input.LastContactedDaysAgo) * scaledDayDuration)
		}

		createReq := repository.CreateContactRequest{
			FullName:      input.FullName,
			LastContacted: &lastContacted,
		}
		if input.Location != "" {
			createReq.Location = &input.Location
		}
		if input.Cadence != "" {
			createReq.Cadence = &input.Cadence
		}

		contact, _, err := s.contactSvc.CreateContact(ctx, createReq, input.Methods)
		if err != nil {
			return nil, fmt.Errorf("seed contact %q: %w", input.FullName, err)
		}
		ids = append(ids, contact.ID.String())
	}
	return ids, nil
}

// --- /test/seed/external-contacts ------------------------------------------

// SeedExternalContactInput mirrors one external_contact to upsert. Source +
// SourceID + DisplayName are already prefix-aware (built by the caller).
type SeedExternalContactInput struct {
	Source       string
	SourceID     string
	HostID       *uuid.UUID
	DisplayName  *string
	FirstName    *string
	LastName     *string
	Organization *string
	JobTitle     *string
	Emails       []repository.EmailEntry
	Phones       []repository.PhoneEntry
	Metadata     map[string]any
}

// SeedExternalContacts upserts import candidates into external_contact.
func (s *TestSeedService) SeedExternalContacts(ctx context.Context, inputs []SeedExternalContactInput) ([]string, error) {
	now := accelerated.GetCurrentTime()
	ids := make([]string, 0, len(inputs))
	for _, input := range inputs {
		upsertReq := repository.UpsertExternalContactRequest{
			Source:       input.Source,
			SourceID:     input.SourceID,
			HostID:       input.HostID,
			DisplayName:  input.DisplayName,
			FirstName:    input.FirstName,
			LastName:     input.LastName,
			Organization: input.Organization,
			JobTitle:     input.JobTitle,
			Emails:       input.Emails,
			Phones:       input.Phones,
			Metadata:     input.Metadata,
			SyncedAt:     &now,
		}
		contact, err := s.externalRepo.Upsert(ctx, upsertReq)
		if err != nil {
			return nil, fmt.Errorf("seed external contact %q: %w", input.SourceID, err)
		}
		ids = append(ids, contact.ID.String())
	}
	return ids, nil
}

// --- /test/seed/method-suggestions -----------------------------------------

// SeedMethodSuggestionsInput captures the linked address-book row + pending/
// dismissed suggestions to seed. ContactName + DisplayName + SourceID are
// prefix-aware (built by the caller); Pending/Dismissed are normalized.
type SeedMethodSuggestionsInput struct {
	Source      string
	ContactName string
	DisplayName string
	SourceID    string
	Emails      []repository.EmailEntry
	Phones      []repository.PhoneEntry
	Pending     []repository.PendingMethodSuggestion
	Dismissed   []repository.PendingMethodSuggestion
}

// SeedMethodSuggestionsResult echoes the seeded ids.
type SeedMethodSuggestionsResult struct {
	ExternalContactID string
	ContactID         string
}

// SeedMethodSuggestions creates a linked imported address-book row carrying
// pending (and optional dismissed) method suggestions, plus the CRM contact.
func (s *TestSeedService) SeedMethodSuggestions(ctx context.Context, input SeedMethodSuggestionsInput) (SeedMethodSuggestionsResult, error) {
	now := accelerated.GetCurrentTime()
	var res SeedMethodSuggestionsResult

	contact, _, err := s.contactSvc.CreateContact(ctx, repository.CreateContactRequest{
		FullName:      input.ContactName,
		LastContacted: &now,
	}, nil)
	if err != nil {
		return res, fmt.Errorf("seed method-suggestions: create contact: %w", err)
	}

	displayName := input.DisplayName
	external, err := s.externalRepo.Upsert(ctx, repository.UpsertExternalContactRequest{
		Source:      input.Source,
		SourceID:    input.SourceID,
		DisplayName: &displayName,
		Emails:      input.Emails,
		Phones:      input.Phones,
		SyncedAt:    &now,
	})
	if err != nil {
		return res, fmt.Errorf("seed method-suggestions: upsert external: %w", err)
	}

	if _, err := s.externalRepo.UpdateMatch(ctx, external.ID, &contact.ID, repository.MatchStatusImported); err != nil {
		return res, fmt.Errorf("seed method-suggestions: link external: %w", err)
	}

	if _, err := s.externalRepo.SetMethodSuggestions(ctx, external.ID, input.Pending); err != nil {
		return res, fmt.Errorf("seed method-suggestions: set pending: %w", err)
	}

	if len(input.Dismissed) > 0 {
		if _, err := s.externalRepo.SetDismissedMethodSuggestionsForTest(ctx, external.ID, input.Dismissed); err != nil {
			return res, fmt.Errorf("seed method-suggestions: set dismissed: %w", err)
		}
	}

	res.ExternalContactID = external.ID.String()
	res.ContactID = contact.ID.String()
	return res, nil
}

// --- /test/seed/overdue-contacts -------------------------------------------

// SeedOverdueContactInput is one overdue contact. FullName is prefix-prepended;
// Cadence is the validated cadence string.
type SeedOverdueContactInput struct {
	FullName    string
	Cadence     string
	DaysOverdue int
	Email       string
}

// SeedOverdueContacts creates contacts with backdated last_contacted
// (now - cadence_duration - days_overdue, env-scaled — preserved verbatim).
func (s *TestSeedService) SeedOverdueContacts(ctx context.Context, inputs []SeedOverdueContactInput) ([]string, error) {
	now := accelerated.GetCurrentTime()
	ids := make([]string, 0, len(inputs))

	for _, input := range inputs {
		cadenceType, err := cadence.ParseCadence(input.Cadence)
		if err != nil {
			return nil, fmt.Errorf("seed overdue contact %q: parse cadence: %w", input.FullName, err)
		}

		cadenceDuration := cadence.GetCadenceDuration(cadenceType)
		weeklyDuration := cadence.GetCadenceDuration(cadence.CadenceWeekly)
		scaledDayDuration := weeklyDuration / 7
		daysOverdueDuration := time.Duration(input.DaysOverdue) * scaledDayDuration
		lastContacted := now.Add(-cadenceDuration).Add(-daysOverdueDuration)

		var methods []ContactMethodInput
		if input.Email != "" {
			methods = append(methods, ContactMethodInput{
				Type:      "email",
				Value:     input.Email,
				IsPrimary: true,
			})
		}

		cadenceStr := input.Cadence
		createReq := repository.CreateContactRequest{
			FullName:      input.FullName,
			Cadence:       &cadenceStr,
			LastContacted: &lastContacted,
		}

		contact, _, err := s.contactSvc.CreateContact(ctx, createReq, methods)
		if err != nil {
			return nil, fmt.Errorf("seed overdue contact %q: %w", input.FullName, err)
		}
		ids = append(ids, contact.ID.String())
	}
	return ids, nil
}

// --- /test/seed/calendar-events --------------------------------------------

// SeedCalendarEventInput is one calendar event. Title/GcalEventID/GoogleAccountID
// are prefix-aware (built by the caller); start/end times are pre-computed.
type SeedCalendarEventInput struct {
	GcalEventID     string
	GoogleAccountID string
	Title           string
	Location        string
	HtmlLink        string
	StartTime       time.Time
	EndTime         time.Time
	Attendees       []repository.Attendee
	MatchedIDs      []uuid.UUID
}

// SeedCalendarEvents upserts calendar events.
func (s *TestSeedService) SeedCalendarEvents(ctx context.Context, inputs []SeedCalendarEventInput) ([]string, error) {
	now := accelerated.GetCurrentTime()
	ids := make([]string, 0, len(inputs))

	for _, input := range inputs {
		title := input.Title
		upsertReq := repository.UpsertCalendarEventRequest{
			GcalEventID:          input.GcalEventID,
			GcalCalendarID:       "primary",
			GoogleAccountID:      input.GoogleAccountID,
			Title:                &title,
			StartTime:            input.StartTime,
			EndTime:              input.EndTime,
			Status:               "confirmed",
			Attendees:            input.Attendees,
			MatchedContactIDs:    input.MatchedIDs,
			SyncedAt:             now,
			LastContactedUpdated: false,
		}
		if input.Location != "" {
			upsertReq.Location = &input.Location
		}
		if input.HtmlLink != "" {
			upsertReq.HtmlLink = &input.HtmlLink
		}

		event, err := s.calendarRepo.Upsert(ctx, upsertReq)
		if err != nil {
			return nil, fmt.Errorf("seed calendar event %q: %w", input.GcalEventID, err)
		}
		ids = append(ids, event.ID.String())
	}
	return ids, nil
}

// --- /test/seed/mac-hosts --------------------------------------------------

// SeedMacHostInput is the paired-host row to seed directly (bypassing pairing).
type SeedMacHostInput struct {
	Hostname        string
	DaemonVersion   string
	ProtocolVersion int32
	Permissions     map[string]any
	SourceHealth    map[string]any
}

// SeedMacHost creates a mac_host row directly (bypassing the pairing flow).
func (s *TestSeedService) SeedMacHost(ctx context.Context, input SeedMacHostInput) (string, error) {
	if s.macHostRepo == nil {
		return "", fmt.Errorf("seed mac host: mac host repo not wired")
	}

	permissions, err := json.Marshal(input.Permissions)
	if err != nil {
		return "", fmt.Errorf("seed mac host: marshal permissions: %w", err)
	}
	sourceHealth, err := json.Marshal(input.SourceHealth)
	if err != nil {
		return "", fmt.Errorf("seed mac host: marshal source_health: %w", err)
	}

	// A real bcrypt hash is generated even for the seed path. The hash is never
	// used for authentication (no daemon has the plaintext), but constructing a
	// real hash keeps the row consistent with the production schema invariants.
	hashed := "$2a$04$placeholdersaltplaceholdersaltO9bF.pIyKsl5j8YpqEUYE2N4FfTQpiy"
	host, err := s.macHostRepo.SeedHostForTest(
		ctx,
		input.Hostname,
		input.DaemonVersion,
		input.ProtocolVersion,
		hashed,
		permissions,
		sourceHealth,
	)
	if err != nil {
		return "", fmt.Errorf("seed mac host: %w", err)
	}
	return host.ID.String(), nil
}

// --- /test/seed/meeting-notes ----------------------------------------------

// SeedMeetingNoteInput is one orphan meeting_note to seed.
type SeedMeetingNoteInput struct {
	AnarlogSessionID uuid.UUID
	Title            string
	Summary          string
}

// SeedMeetingNotes creates orphan_needs_review meeting_note rows against a
// mac_host (transactional, mirroring the route).
func (s *TestSeedService) SeedMeetingNotes(ctx context.Context, hostID uuid.UUID, notes []SeedMeetingNoteInput) ([]string, error) {
	tx, err := s.database.Pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("seed meeting notes: begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	now := accelerated.GetCurrentTime()
	ids := make([]string, 0, len(notes))
	for _, note := range notes {
		params := repository.InsertMeetingNoteParams{
			AnarlogSessionID: note.AnarlogSessionID,
			MacHostID:        &hostID,
			LinkageState:     repository.LinkageStateOrphanNeedsReview,
			MeetingAt:        now,
			InputHash:        "",
			ResolvedSetHash:  "",
		}
		if note.Title != "" {
			title := note.Title
			params.Title = &title
		}
		if note.Summary != "" {
			summary := note.Summary
			params.Summary = &summary
		}

		row, insErr := s.meetingNoteRepo.InsertMeetingNoteTx(ctx, tx, params)
		if insErr != nil {
			return nil, fmt.Errorf("seed meeting notes: insert: %w", insErr)
		}
		ids = append(ids, row.ID.String())
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("seed meeting notes: commit: %w", err)
	}
	return ids, nil
}

// --- /test/cleanup ---------------------------------------------------------

// CleanupResult reports the per-table prefix-delete counts (preserving the HTTP
// response shape).
type CleanupResult struct {
	DeletedContacts         int64
	DeletedExternalContacts int64
	DeletedCalendarEvents   int64
}

// Cleanup deletes prefix-keyed test data (contacts, external contacts, calendar
// events) atomically, plus host-scoped meeting notes. The prefix-delete tx +
// queries now live in the repository (SyntheticSupportRepository.CleanupByPrefix)
// — the handler no longer calls db.New(tx) queries directly (the layer fix). The
// LIKE-wildcard escaping stays at the service boundary, exactly where the handler
// did it before.
func (s *TestSeedService) Cleanup(ctx context.Context, prefix string, hostID *uuid.UUID) (CleanupResult, error) {
	var res CleanupResult

	tx, err := s.database.Pool.Begin(ctx)
	if err != nil {
		return res, fmt.Errorf("cleanup: begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	escapedPrefix := escapeSQLLikeWildcards(prefix)
	support := repository.NewSyntheticSupportRepository(db.New(tx))
	deleted, err := support.CleanupByPrefix(ctx, escapedPrefix)
	if err != nil {
		return res, fmt.Errorf("cleanup: prefix deletes: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return res, fmt.Errorf("cleanup: commit: %w", err)
	}

	res.DeletedContacts = deleted.DeletedContacts
	res.DeletedExternalContacts = deleted.DeletedExternalContacts
	res.DeletedCalendarEvents = deleted.DeletedCalendarEvents

	// Meeting notes are seeded with random session UUIDs scoped to a host, so
	// cleanup is by host id rather than by prefix.
	if hostID != nil {
		if err := s.meetingNoteRepo.TestHardDeleteByHostID(ctx, *hostID); err != nil {
			return res, fmt.Errorf("cleanup: delete meeting notes: %w", err)
		}
	}

	return res, nil
}

// escapeSQLLikeWildcards escapes SQL LIKE pattern wildcards (% and _) so a
// caller-supplied prefix is matched literally (prevents LIKE-wildcard injection).
// Moved verbatim from the /cleanup handler as part of the layer fix.
func escapeSQLLikeWildcards(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	s = strings.ReplaceAll(s, `_`, `\_`)
	return s
}
