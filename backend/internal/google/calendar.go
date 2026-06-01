package google

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/identity"
	"personal-crm/backend/internal/logger"
	"personal-crm/backend/internal/matching"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"
	"personal-crm/backend/internal/sync"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/api/calendar/v3"
	"google.golang.org/api/option"
)

const (
	// CalendarSourceName is the source identifier for Google Calendar
	CalendarSourceName = "gcal"
	// CalendarAttendeeSource is the source identifier for calendar attendee import candidates
	CalendarAttendeeSource = "gcal_attendee"
	// CalendarDefaultInterval is the default sync interval for calendar events (every 15 minutes).
	// Incremental sync via syncToken keeps API usage low (~1-2 calls per sync for typical changes).
	// Google Calendar API quota: 1M queries/day (default) - 15min interval = 96 syncs/day, well within limits.
	CalendarDefaultInterval = 15 * time.Minute
	// CalendarPastSyncDays is the number of days to sync into the past.
	// 1 year provides comprehensive meeting history for relationship context.
	CalendarPastSyncDays = 365
	// CalendarFutureSyncDays is the number of days to sync into the future.
	// 30 days captures near-term scheduled meetings without excessive API calls.
	CalendarFutureSyncDays = 30
)

// blockedCalendarDomains contains email domains that are always system/resource
// accounts, never real people. We only block domains where ALL emails are system
// accounts - not company domains where employees might have legitimate addresses.
var blockedCalendarDomains = []string{
	// Google Calendar resources (always system, never real people)
	"group.calendar.google.com",    // Group/secondary calendars
	"resource.calendar.google.com", // Room/resource calendars
	"calendar.google.com",          // Generic calendar resources
	"group.v.calendar.google.com",  // System calendars (holidays, birthdays)
	"gtempaccount.com",             // Google temp accounts for external invitees
}

// blockedEmailPrefixes contains email local-part prefixes that indicate system
// or automated emails rather than real people.
var blockedEmailPrefixes = []string{
	"noreply",
	"no-reply",
	"no_reply",
	"do-not-reply",
	"donotreply",
	"calendar-invite",
	"notifications",
	"mailer-daemon",
	"postmaster",
}

// calendarRepoInterface defines the methods needed from calendar repository (for testability)
type calendarRepoInterface interface {
	Upsert(ctx context.Context, req repository.UpsertCalendarEventRequest) (*repository.CalendarEvent, error)
	ListPastEventsNeedingUpdate(ctx context.Context, before time.Time, limit int32) ([]repository.CalendarEvent, error)
	MarkLastContactedUpdated(ctx context.Context, id uuid.UUID) error
	// Decline/cancel remove branch.
	GetByGcalID(ctx context.Context, gcalEventID, gcalCalendarID, googleAccountID string) (*repository.CalendarEvent, error)
	DeleteByGcalIDTx(ctx context.Context, tx pgx.Tx, gcalEventID, gcalCalendarID, googleAccountID string) error
	MarkCancelledByGcalID(ctx context.Context, gcalEventID, gcalCalendarID, googleAccountID string) error
}

// contactRepoInterface defines the methods needed from contact repository (for testability)
type contactRepoInterface interface {
	FindSimilarContacts(ctx context.Context, name string, threshold float64, limit int32) ([]repository.ContactMatch, error)
}

// identityServiceInterface defines the methods needed from identity service (for testability)
type identityServiceInterface interface {
	MatchOrCreate(ctx context.Context, req service.MatchRequest) (*service.MatchResult, error)
}

// externalContactRepoInterface defines the methods needed from external contact repository (for testability)
type externalContactRepoInterface interface {
	Upsert(ctx context.Context, req repository.UpsertExternalContactRequest) (*repository.ExternalContact, error)
}

// EventContext contains meeting information that is stored as metadata when
// creating import candidates from unmatched calendar attendees. This provides
// context about where the attendee was discovered (which meeting) so users
// can make informed import decisions.
type EventContext struct {
	Title     string    // Event summary/title
	StartTime time.Time // Event start time
	HtmlLink  string    // URL to view the event in Google Calendar
}

// CalendarSyncProvider implements SyncProvider for Google Calendar.
//
// Post-PR-6 (cutover): past-event interaction writes happen via the
// event bus — updateLastContactedForPastEvents publishes calendar.attended
// per (event, contact) pair and the async consumer writes the
// interaction rows.
type CalendarSyncProvider struct {
	oauthService        *OAuthService
	calendarRepo        calendarRepoInterface
	contactRepo         contactRepoInterface
	identityService     identityServiceInterface
	externalContactRepo externalContactRepoInterface
	// eventBus is required in cutover mode. Nil when mode=off/shadow
	// post-cutover — past-event publishes are skipped and
	// last_contacted-from-calendar updates do NOT happen (spec §3.9).
	eventBus *events.Bus
	// pool is required by the cutover decline remove branch, which does
	// N calendar.declined publishes + one calendar_event DELETE in a single
	// atomic tx (publish-before-delete). Mirrors the Todoist provider's pool
	// dependency. Nil when mode=off — the remove branch then defers cleanup
	// by marking the stored row cancelled instead of publishing + deleting.
	pool *pgxpool.Pool
	// declineBus is the bus used by the cutover decline remove branch's
	// per-contact PublishTx. It defaults to eventBus; tests substitute a
	// failing stub via setDeclineBusForTest to assert publish-before-delete
	// (a publish failure must leave the calendar_event row intact). The
	// off-mode gate keys off the concrete eventBus/pool fields, not this one.
	declineBus busTx
}

// NewCalendarSyncProvider creates a new Google Calendar sync provider.
// eventBus is required in cutover (default post-PR-6). Nil disables
// past-event interaction writes entirely. pool backs the decline remove
// branch's atomic publish-before-delete tx; in off mode (nil eventBus or
// nil pool) the branch defers cleanup via MarkCancelledByGcalID.
func NewCalendarSyncProvider(
	oauthService *OAuthService,
	calendarRepo *repository.CalendarEventRepository,
	contactRepo *repository.ContactRepository,
	identityService *service.IdentityService,
	externalContactRepo *repository.ExternalContactRepository,
	eventBus *events.Bus,
	pool *pgxpool.Pool,
) *CalendarSyncProvider {
	return &CalendarSyncProvider{
		oauthService:        oauthService,
		calendarRepo:        calendarRepo,
		contactRepo:         contactRepo,
		identityService:     identityService,
		externalContactRepo: externalContactRepo,
		eventBus:            eventBus,
		pool:                pool,
		declineBus:          eventBus,
	}
}

// Config returns the provider's configuration
func (p *CalendarSyncProvider) Config() sync.SourceConfig {
	return sync.SourceConfig{
		Name:                 CalendarSourceName,
		DisplayName:          "Google Calendar",
		Strategy:             repository.SyncStrategyFetchAll,
		SupportsMultiAccount: true,
		SupportsDiscovery:    true,
		DefaultInterval:      CalendarDefaultInterval,
	}
}

// Sync performs the calendar sync for a specific account
func (p *CalendarSyncProvider) Sync(
	ctx context.Context,
	state *repository.SyncState,
	contacts []repository.Contact,
) (*sync.SyncResult, error) {
	// Account ID is required for Google Calendar sync
	if state.AccountID == nil {
		return nil, fmt.Errorf("account ID required for Google Calendar sync")
	}
	accountID := *state.AccountID

	logger.Info().
		Str("source", CalendarSourceName).
		Str("account", accountID).
		Msg("starting Google Calendar sync")

	// Get authenticated client for this account
	client, err := p.oauthService.GetClientForAccount(ctx, accountID)
	if err != nil {
		return nil, fmt.Errorf("get OAuth client: %w", err)
	}

	// Create Calendar API service
	calSvc, err := calendar.NewService(ctx, option.WithHTTPClient(client))
	if err != nil {
		return nil, fmt.Errorf("create Calendar service: %w", err)
	}

	result := &sync.SyncResult{}

	// Perform initial or incremental sync based on cursor
	if state.SyncCursor == nil || *state.SyncCursor == "" {
		return p.initialSync(ctx, calSvc, accountID, result)
	}
	return p.incrementalSync(ctx, calSvc, accountID, *state.SyncCursor, result)
}

// ValidateCredentials checks if the Google credentials are valid
func (p *CalendarSyncProvider) ValidateCredentials(ctx context.Context, accountID *string) error {
	if accountID == nil {
		// Check if any accounts exist
		accounts, err := p.oauthService.ListAccounts(ctx)
		if err != nil {
			return fmt.Errorf("list accounts: %w", err)
		}
		if len(accounts) == 0 {
			return fmt.Errorf("no Google accounts connected")
		}
		return nil
	}

	// Validate specific account
	_, err := p.oauthService.GetClientForAccount(ctx, *accountID)
	if err != nil {
		return fmt.Errorf("get OAuth client for account: %w", err)
	}
	return nil
}

// initialSync fetches events from the past year to 30 days ahead and gets a sync token
func (p *CalendarSyncProvider) initialSync(
	ctx context.Context,
	calSvc *calendar.Service,
	accountID string,
	result *sync.SyncResult,
) (*sync.SyncResult, error) {
	now := accelerated.GetCurrentTime()
	timeMin := now.AddDate(0, 0, -CalendarPastSyncDays).Format(time.RFC3339)
	timeMax := now.AddDate(0, 0, CalendarFutureSyncDays).Format(time.RFC3339)

	logger.Debug().
		Str("timeMin", timeMin).
		Str("timeMax", timeMax).
		Msg("performing initial calendar sync")

	var pageToken string
	for {
		req := calSvc.Events.List("primary").
			TimeMin(timeMin).
			TimeMax(timeMax).
			SingleEvents(true).
			OrderBy("startTime").
			MaxResults(250)

		if pageToken != "" {
			req = req.PageToken(pageToken)
		}

		resp, err := req.Do()
		if err != nil {
			return result, fmt.Errorf("list events: %w", err)
		}

		for _, event := range resp.Items {
			if err := p.processEvent(ctx, event, accountID); err != nil {
				logger.Warn().
					Err(err).
					Str("eventId", event.Id).
					Msg("failed to process event")
				continue
			}
			result.ItemsProcessed++
		}

		pageToken = resp.NextPageToken
		if pageToken == "" {
			// Store the sync token for incremental syncs
			if resp.NextSyncToken != "" {
				result.NewCursor = resp.NextSyncToken
			}
			break
		}
	}

	// After initial sync, update last_contacted for past events
	if err := p.updateLastContactedForPastEvents(ctx); err != nil {
		logger.Warn().Err(err).Msg("failed to update last_contacted for past events")
	}

	logger.Info().
		Str("source", CalendarSourceName).
		Str("account", accountID).
		Int("processed", result.ItemsProcessed).
		Msg("initial Google Calendar sync completed")

	return result, nil
}

// incrementalSync uses the sync token to fetch only changed events
func (p *CalendarSyncProvider) incrementalSync(
	ctx context.Context,
	calSvc *calendar.Service,
	accountID string,
	syncToken string,
	result *sync.SyncResult,
) (*sync.SyncResult, error) {
	logger.Debug().
		Str("syncToken", syncToken[:min(len(syncToken), 20)]+"...").
		Msg("performing incremental calendar sync")

	var pageToken string
	for {
		req := calSvc.Events.List("primary").
			SyncToken(syncToken).
			MaxResults(250)

		if pageToken != "" {
			req = req.PageToken(pageToken)
		}

		resp, err := req.Do()
		if err != nil {
			// If sync token is invalid, fall back to initial sync
			if strings.Contains(err.Error(), "410") || strings.Contains(err.Error(), "fullSyncRequired") {
				logger.Warn().Msg("sync token expired, falling back to initial sync")
				return p.initialSync(ctx, calSvc, accountID, result)
			}
			return result, fmt.Errorf("list events: %w", err)
		}

		for _, event := range resp.Items {
			if err := p.processEvent(ctx, event, accountID); err != nil {
				logger.Warn().
					Err(err).
					Str("eventId", event.Id).
					Msg("failed to process event")
				continue
			}
			result.ItemsProcessed++
		}

		pageToken = resp.NextPageToken
		if pageToken == "" {
			// Store the new sync token
			if resp.NextSyncToken != "" {
				result.NewCursor = resp.NextSyncToken
			}
			break
		}
	}

	// After incremental sync, update last_contacted for past events
	if err := p.updateLastContactedForPastEvents(ctx); err != nil {
		logger.Warn().Err(err).Msg("failed to update last_contacted for past events")
	}

	logger.Info().
		Str("source", CalendarSourceName).
		Str("account", accountID).
		Int("processed", result.ItemsProcessed).
		Msg("incremental Google Calendar sync completed")

	return result, nil
}

// processEvent processes a single calendar event.
//
// The keep decision is evaluated BEFORE time parsing and the all-day skip:
// incremental-sync deltas for cancelled/declined events can arrive with empty
// Start.DateTime/End.DateTime (Google sends a minimal stub), so the remove
// branch must run without depending on inbound times. The CRM stores a
// calendar_event only while the user has it accepted AND it is not cancelled;
// any sync that observes it otherwise removes the stored copy + its derived
// interactions.
func (p *CalendarSyncProvider) processEvent(
	ctx context.Context,
	event *calendar.Event,
	accountID string,
) error {
	userResponse := p.getUserResponse(event, accountID)
	status := getEventStatus(event)

	keep := userResponse != nil && *userResponse == "accepted" && status != "cancelled"
	if !keep {
		// Decline / tentative / needsAction / user-removed / cancelled.
		// Run the remove branch (keyed on event.Id, always present on any
		// delta) BEFORE the all-day skip and the time parse: a previously-
		// stored TIMED event arriving as a cancelled/declined stub (possibly
		// all-day-shaped, possibly without DateTime) must still be cleaned up.
		return p.removeDeclinedEvent(ctx, event.Id, accountID)
	}

	// Keep path. Skip all-day events (holidays/birthdays, not meetings).
	// Accepted meetings always carry DateTime, so this is byte-for-byte
	// identical to the pre-restructure keep-path behavior.
	if event.Start.Date != "" {
		logger.Debug().
			Str("eventId", event.Id).
			Str("summary", event.Summary).
			Msg("skipping all-day event")
		return nil
	}

	// Parse start and end times
	startTime, err := time.Parse(time.RFC3339, event.Start.DateTime)
	if err != nil {
		return fmt.Errorf("parse start time: %w", err)
	}
	endTime, err := time.Parse(time.RFC3339, event.End.DateTime)
	if err != nil {
		return fmt.Errorf("parse end time: %w", err)
	}

	// Build attendee list
	attendees := p.buildAttendeeList(event, accountID)

	// Create event context for import candidates
	eventContext := &EventContext{
		Title:     event.Summary,
		StartTime: startTime,
		HtmlLink:  event.HtmlLink,
	}

	// Match attendees to CRM contacts (and store unmatched as import candidates)
	matchedContactIDs := p.matchAttendees(ctx, attendees, accountID, eventContext)

	// Prepare upsert request
	title := event.Summary
	req := repository.UpsertCalendarEventRequest{
		GcalEventID:          event.Id,
		GcalCalendarID:       "primary",
		GoogleAccountID:      accountID,
		Title:                &title,
		Description:          strPtrIfNotEmpty(event.Description),
		Location:             strPtrIfNotEmpty(event.Location),
		StartTime:            startTime,
		EndTime:              endTime,
		AllDay:               false,
		Status:               status,
		UserResponse:         userResponse,
		OrganizerEmail:       getOrganizerEmail(event),
		Attendees:            attendees,
		MatchedContactIDs:    matchedContactIDs,
		SyncedAt:             accelerated.GetCurrentTime(),
		LastContactedUpdated: false,
		HtmlLink:             strPtrIfNotEmpty(event.HtmlLink),
	}

	_, err = p.calendarRepo.Upsert(ctx, req)
	if err != nil {
		return fmt.Errorf("upsert calendar event: %w", err)
	}
	return nil
}

// removeDeclinedEvent removes a previously-stored calendar_event (and its
// derived interactions) when the event is observed declined / tentative /
// needsAction / user-removed / cancelled upstream. Keyed on the inbound
// gcal event id (always present on any delta) — reads EndTime +
// MatchedContactIDs from the STORED row, so it needs neither the inbound
// Start/End DateTime nor the all-day discriminator.
//
// Cutover (eventBus + pool present): in ONE tx, publish a calendar.declined
// per matched contact (carrying the stored row's INTERNAL ID so the decline
// consumer's source_ref lookup matches), THEN delete the calendar_event.
// Publish-before-delete: a publish failure rolls back the whole tx (event
// rows + delete), and the next sync re-runs the branch cleanly. If the row
// is already gone the next sync no-ops at GetByGcalID.
//
// Off mode (nil eventBus or nil pool): defer cleanup by marking the stored
// row status='cancelled' rather than deleting. This stops re-firing
// calendar.attended (ListPastEventsNeedingUpdate requires status='confirmed')
// and hides the row from contact reads WITHOUT losing the only cleanup
// handle for an already-recorded interaction — the cutover resume re-observes
// the row and finishes the cleanup.
func (p *CalendarSyncProvider) removeDeclinedEvent(ctx context.Context, gcalEventID, accountID string) error {
	stored, err := p.calendarRepo.GetByGcalID(ctx, gcalEventID, "primary", accountID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			// Never accepted/stored (the common case — the user has declined
			// many invites that were never in the CRM). Do NOT emit a decline.
			return nil
		}
		return fmt.Errorf("look up stored calendar event for decline: %w", err)
	}

	// Off mode: defer cleanup by marking cancelled (neither publish nor delete).
	if p.eventBus == nil || p.pool == nil {
		if err := p.calendarRepo.MarkCancelledByGcalID(ctx, gcalEventID, "primary", accountID); err != nil {
			return fmt.Errorf("mark calendar event cancelled (off mode): %w", err)
		}
		logger.Debug().
			Str("eventId", gcalEventID).
			Msg("calendar: decline observed in off mode; marked stored event cancelled (deferred cleanup)")
		return nil
	}

	// Cutover: publish-before-delete in one tx.
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin decline tx: %w", err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(ctx); rollbackErr != nil &&
			!errors.Is(rollbackErr, pgx.ErrTxClosed) {
			logger.Warn().Err(rollbackErr).Str("eventId", gcalEventID).Msg("calendar: decline tx rollback failed")
		}
	}()

	eventIDStr := stored.ID.String()
	for _, contactID := range stored.MatchedContactIDs {
		if pubErr := publishCalendarDeclinedTx(ctx, p.declineBus, tx, contactID, eventIDStr, stored.EndTime); pubErr != nil {
			return fmt.Errorf("publish calendar.declined for contact %s: %w", contactID, pubErr)
		}
	}

	if err := p.calendarRepo.DeleteByGcalIDTx(ctx, tx, gcalEventID, "primary", accountID); err != nil {
		return fmt.Errorf("delete declined calendar event: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit decline tx: %w", err)
	}

	logger.Info().
		Str("eventId", gcalEventID).
		Str("internalId", eventIDStr).
		Int("contacts", len(stored.MatchedContactIDs)).
		Msg("calendar: removed declined/cancelled event + published decline per matched contact")
	return nil
}

// RunProcessEventForTest drives the unexported processEvent entry point so
// cross-package integration tests (package tests) can exercise the full
// keep/remove gate + cutover remove branch end-to-end against a real DB +
// bus + pool. Production code must NOT call this.
func (p *CalendarSyncProvider) RunProcessEventForTest(ctx context.Context, event *calendar.Event, accountID string) error {
	return p.processEvent(ctx, event, accountID)
}

// SetDeclineBusForTest substitutes the bus used by the cutover decline remove
// branch's per-contact PublishTx so a test can assert publish-before-delete
// (a failing PublishTx must leave the calendar_event row intact). Production
// code must NOT call this.
func (p *CalendarSyncProvider) SetDeclineBusForTest(b busTx) {
	p.declineBus = b
}

// getUserResponse extracts the user's response status from an event
func (p *CalendarSyncProvider) getUserResponse(event *calendar.Event, accountID string) *string {
	for _, attendee := range event.Attendees {
		if attendee.Self || strings.EqualFold(attendee.Email, accountID) {
			return &attendee.ResponseStatus
		}
	}
	// If user is the organizer
	if event.Organizer != nil && strings.EqualFold(event.Organizer.Email, accountID) {
		accepted := "accepted"
		return &accepted
	}
	return nil
}

// buildAttendeeList builds the attendee list for storage.
// This includes all attendees from the event plus the organizer if they're not
// already in the attendee list (Google Calendar sometimes omits the organizer
// from the attendees array).
func (p *CalendarSyncProvider) buildAttendeeList(event *calendar.Event, accountID string) []repository.Attendee {
	attendees := make([]repository.Attendee, 0, len(event.Attendees))

	for _, a := range event.Attendees {
		isSelf := a.Self || strings.EqualFold(a.Email, accountID)
		isOrganizer := event.Organizer != nil && strings.EqualFold(a.Email, event.Organizer.Email)

		attendees = append(attendees, repository.Attendee{
			Email:        a.Email,
			DisplayName:  a.DisplayName,
			ResponseType: a.ResponseStatus,
			Self:         isSelf,
			Organizer:    isOrganizer,
		})
	}

	// Include organizer if not already in attendees list (Google Calendar sometimes
	// omits the organizer from the attendees array, but we want to match them too)
	if event.Organizer != nil && event.Organizer.Email != "" {
		organizerEmail := event.Organizer.Email
		// Check both Organizer.Self flag and email match - the Self flag handles
		// aliases and delegated calendars where email may differ from accountID
		organizerIsSelf := event.Organizer.Self || strings.EqualFold(organizerEmail, accountID)

		// Check if organizer is already in the attendees list
		organizerInAttendees := false
		for _, a := range attendees {
			if strings.EqualFold(a.Email, organizerEmail) {
				organizerInAttendees = true
				break
			}
		}

		// Add organizer as attendee if not self and not already in list
		if !organizerIsSelf && !organizerInAttendees {
			attendees = append(attendees, repository.Attendee{
				Email:        organizerEmail,
				DisplayName:  event.Organizer.DisplayName,
				ResponseType: "", // Organizers don't have a response status
				Self:         false,
				Organizer:    true,
			})
		}
	}

	return attendees
}

// matchAttendees matches attendee emails to CRM contacts
// First attempts exact email matching via identity service, then falls back to
// fuzzy name matching with weighted scoring (60% name + 40% method overlap).
// Unmatched attendees are stored as import candidates with meeting context.
func (p *CalendarSyncProvider) matchAttendees(
	ctx context.Context,
	attendees []repository.Attendee,
	accountID string,
	eventContext *EventContext,
) []uuid.UUID {
	matchedIDs := make([]uuid.UUID, 0)
	seen := make(map[uuid.UUID]bool)

	for _, attendee := range attendees {
		// Skip self
		if attendee.Self {
			continue
		}

		// Skip empty emails
		if attendee.Email == "" {
			continue
		}

		// Skip blocked emails (scheduling services, calendar resources, noreply, etc.)
		if isBlockedCalendarEmail(attendee.Email) {
			logger.Debug().
				Str("email", attendee.Email).
				Msg("skipping blocked calendar email")
			continue
		}

		// Step 1: Try exact email matching via identity service
		displayName := attendee.DisplayName
		result, err := p.identityService.MatchOrCreate(ctx, service.MatchRequest{
			RawIdentifier: attendee.Email,
			Type:          identity.IdentifierTypeEmail,
			Source:        CalendarSourceName,
			DisplayName:   &displayName,
		})

		if err != nil {
			logger.Debug().
				Err(err).
				Str("email", attendee.Email).
				Msg("failed to match attendee via identity service")
		}

		// If exact match found, use it
		if result != nil && result.ContactID != nil {
			if !seen[*result.ContactID] {
				matchedIDs = append(matchedIDs, *result.ContactID)
				seen[*result.ContactID] = true
			}
			continue
		}

		// Step 2: Fall back to fuzzy name matching if display name is available
		if attendee.DisplayName != "" {
			fuzzyMatch := p.findFuzzyMatch(ctx, attendee.DisplayName, attendee.Email)
			if fuzzyMatch != nil {
				if !seen[*fuzzyMatch] {
					matchedIDs = append(matchedIDs, *fuzzyMatch)
					seen[*fuzzyMatch] = true
					logger.Debug().
						Str("displayName", attendee.DisplayName).
						Str("email", attendee.Email).
						Str("contactId", fuzzyMatch.String()).
						Msg("fuzzy matched attendee to contact")
				}
				continue
			}
		}

		// Step 3: No match found - store as import candidate
		if err := p.storeUnmatchedAttendee(ctx, attendee, accountID, eventContext); err != nil {
			logger.Warn().
				Err(err).
				Str("email", attendee.Email).
				Msg("failed to store unmatched attendee as import candidate")
		}
	}

	return matchedIDs
}

// findFuzzyMatch attempts to match an attendee by name similarity and contact method overlap
// Returns the contact ID if a match with confidence >= CalendarFuzzyConfidenceThreshold is found
func (p *CalendarSyncProvider) findFuzzyMatch(ctx context.Context, displayName, email string) *uuid.UUID {
	// Find contacts with similar names
	matches, err := p.contactRepo.FindSimilarContacts(ctx, displayName, matching.CalendarConfig.MinSimilarityThreshold, 5)
	if err != nil {
		logger.Debug().Err(err).Str("name", displayName).Msg("failed to find similar contacts")
		return nil
	}

	if len(matches) == 0 {
		return nil
	}

	// Normalize the attendee email for comparison
	normalizedEmail := matching.NormalizeEmail(email)

	var bestMatch *uuid.UUID
	var bestScore float64

	for _, match := range matches {
		// Start with name similarity weighted score (60%)
		score := matching.CalendarConfig.Score(match.Similarity, 0, 0)
		methodMatches := 0
		totalEmailMethods := 0

		// Check for contact method overlap (40% weight)
		// Only count email methods for comparison with the attendee email
		for _, method := range match.Contact.Methods {
			switch method.Type {
			case "email":
				totalEmailMethods++
				if matching.NormalizeEmail(method.Value) == normalizedEmail {
					methodMatches++
				}
			}
		}

		if totalEmailMethods > 0 {
			score = matching.CalendarConfig.Score(match.Similarity, methodMatches, totalEmailMethods)
		}

		// Update best match if this score meets threshold and is higher than current best
		if score >= matching.CalendarConfig.ConfidenceThreshold && score > bestScore {
			bestScore = score
			contactID := match.Contact.ID
			bestMatch = &contactID
		}
	}

	if bestMatch != nil {
		logger.Debug().
			Str("displayName", displayName).
			Float64("confidence", bestScore).
			Str("contactId", bestMatch.String()).
			Msg("found fuzzy match for attendee")
	}

	return bestMatch
}

// storeUnmatchedAttendee stores an unmatched calendar attendee as an import candidate.
// It creates an external_contact record with source='gcal_attendee' so the attendee
// appears on the Imports page for user review.
//
// Deduplication: Uses normalized (lowercase, trimmed) email as source_id, allowing
// the database upsert to handle deduplication. If the same person appears in multiple
// meetings, only one import candidate is created (with metadata from the most recent meeting).
//
// Graceful handling: Returns nil without error if externalContactRepo is nil (for tests)
// or if eventContext is nil (no meeting context available).
func (p *CalendarSyncProvider) storeUnmatchedAttendee(
	ctx context.Context,
	attendee repository.Attendee,
	accountID string,
	eventContext *EventContext,
) error {
	// Skip if no external contact repo (e.g., in tests)
	if p.externalContactRepo == nil {
		return nil
	}

	// Skip if no event context
	if eventContext == nil {
		return nil
	}

	// Use normalized email as source_id for deduplication
	sourceID := matching.NormalizeEmail(attendee.Email)

	// Build metadata with meeting context
	metadata := map[string]any{
		"meeting_title": eventContext.Title,
		"meeting_date":  eventContext.StartTime.Format(time.RFC3339),
		"meeting_link":  eventContext.HtmlLink,
		"discovered_at": accelerated.GetCurrentTime().Format(time.RFC3339),
	}

	// Build emails array
	emails := []repository.EmailEntry{
		{Value: attendee.Email},
	}

	// Determine display name: use provided name, or infer from email if empty
	var displayName *string
	if attendee.DisplayName != "" {
		displayName = &attendee.DisplayName
	} else {
		// Try to infer name from email address pattern (e.g., john.smith@domain.com → "John Smith")
		displayName = inferNameFromEmail(attendee.Email)
	}

	syncedAt := accelerated.GetCurrentTime()

	// Upsert external contact (creates or updates existing)
	_, err := p.externalContactRepo.Upsert(ctx, repository.UpsertExternalContactRequest{
		Source:      CalendarAttendeeSource,
		SourceID:    sourceID,
		AccountID:   &accountID,
		DisplayName: displayName,
		Emails:      emails,
		Metadata:    metadata,
		SyncedAt:    &syncedAt,
	})
	if err != nil {
		return fmt.Errorf("upsert external contact: %w", err)
	}

	logger.Debug().
		Str("email", attendee.Email).
		Str("displayName", ptrToStr(displayName)).
		Bool("nameInferred", attendee.DisplayName == "" && displayName != nil).
		Str("meetingTitle", eventContext.Title).
		Msg("stored unmatched attendee as import candidate")

	return nil
}

// updateLastContactedForPastEvents publishes calendar.attended events for
// past calendar events. In cutover mode the async InteractionRecorder
// consumer writes the interaction rows from those events.
//
// Scheduler bookkeeping: MarkLastContactedUpdated fires ONLY when every
// contact pair successfully published. If any Publish fails, the event
// stays unprocessed and the next tick re-publishes the remaining pairs —
// the per-(event, contact) SourceID ensures already-published pairs no-op
// via the event table's unique index (plan Decision 11).
func (p *CalendarSyncProvider) updateLastContactedForPastEvents(ctx context.Context) error {
	now := accelerated.GetCurrentTime()

	events, err := p.calendarRepo.ListPastEventsNeedingUpdate(ctx, now, 100)
	if err != nil {
		return fmt.Errorf("list past events: %w", err)
	}

	for _, event := range events {
		if p.eventBus == nil {
			logger.Warn().
				Str("eventId", event.ID.String()).
				Msg("calendar: eventBus not configured; skipping past-event publish (mode=off/shadow post-cutover)")
			continue
		}
		eventIDStr := event.ID.String()
		allPublished := true
		for _, contactID := range event.MatchedContactIDs {
			if pubErr := publishCalendarAttended(ctx, p.eventBus, contactID, eventIDStr, event.EndTime, event.Title); pubErr != nil {
				logger.Warn().Err(pubErr).
					Str("contactId", contactID.String()).
					Str("eventId", eventIDStr).
					Msg("calendar: publish calendar.attended failed")
				allPublished = false
				continue
			}
			logger.Debug().
				Str("contactId", contactID.String()).
				Str("eventTitle", ptrToStr(event.Title)).
				Time("endTime", event.EndTime).
				Msg("published calendar.attended for past event")
		}

		// Only mark the event processed when every contact pair published.
		// On partial failure the next tick re-publishes — already-published
		// pairs no-op via the (source, source_id) unique index (plan
		// Decision 11).
		if allPublished {
			if err := p.calendarRepo.MarkLastContactedUpdated(ctx, event.ID); err != nil {
				logger.Warn().
					Err(err).
					Str("eventId", event.ID.String()).
					Msg("failed to mark event as processed")
			}
		}
	}

	if len(events) > 0 {
		logger.Info().
			Int("eventsProcessed", len(events)).
			Msg("published calendar.attended for past events")
	}

	return nil
}

// Helper functions

func strPtrIfNotEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func getEventStatus(event *calendar.Event) string {
	if event.Status == "" {
		return "confirmed"
	}
	return event.Status
}

func getOrganizerEmail(event *calendar.Event) *string {
	if event.Organizer == nil || event.Organizer.Email == "" {
		return nil
	}
	return &event.Organizer.Email
}

func ptrToStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// isBlockedCalendarEmail checks if an email address should be filtered out from
// calendar sync. This includes calendar resources (rooms, group calendars),
// scheduling services, and system/automated email addresses.
func isBlockedCalendarEmail(email string) bool {
	email = strings.ToLower(strings.TrimSpace(email))

	// Check blocked domains (scheduling services, calendar resources)
	for _, domain := range blockedCalendarDomains {
		if strings.HasSuffix(email, "@"+domain) {
			return true
		}
	}

	// Check blocked prefixes (noreply, notifications, etc.)
	parts := strings.SplitN(email, "@", 2)
	if len(parts) == 2 {
		localPart := parts[0]
		for _, prefix := range blockedEmailPrefixes {
			if localPart == prefix || strings.HasPrefix(localPart, prefix+"+") {
				return true
			}
		}
	}

	return false
}

// inferNameFromEmail attempts to extract a human-readable name from an email address.
// It handles common patterns like first.last@domain.com and first_last@domain.com.
// Returns nil if no reasonable name can be inferred.
func inferNameFromEmail(email string) *string {
	parts := strings.Split(email, "@")
	if len(parts) != 2 {
		return nil
	}
	local := parts[0]

	// Remove Gmail-style + modifiers (e.g., john+work@gmail.com → john)
	if plusIdx := strings.Index(local, "+"); plusIdx > 0 {
		local = local[:plusIdx]
	}

	// Split by separators (. or _)
	var nameParts []string
	if strings.Contains(local, ".") {
		nameParts = strings.Split(local, ".")
	} else if strings.Contains(local, "_") {
		nameParts = strings.Split(local, "_")
	} else {
		nameParts = []string{local}
	}

	// Process each part: strip trailing numbers, capitalize
	var result []string
	for _, part := range nameParts {
		part = strings.TrimSpace(part)
		part = stripTrailingNumbers(part)
		if part == "" {
			continue
		}
		result = append(result, capitalize(part))
	}

	if len(result) == 0 {
		return nil
	}

	name := strings.Join(result, " ")
	return &name
}

// stripTrailingNumbers removes trailing digits from a string.
// e.g., "smith2" → "smith", "john123" → "john"
func stripTrailingNumbers(s string) string {
	return strings.TrimRight(s, "0123456789")
}

// capitalize returns the string with the first letter uppercased and the rest lowercased.
func capitalize(s string) string {
	if s == "" {
		return s
	}
	r := []rune(strings.ToLower(s))
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}
