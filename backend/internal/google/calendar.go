package google

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/identity"
	"personal-crm/backend/internal/logger"
	"personal-crm/backend/internal/matching"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"
	"personal-crm/backend/internal/sync"

	"github.com/google/uuid"
	"google.golang.org/api/calendar/v3"
	"google.golang.org/api/option"
)

const (
	// CalendarSourceName is the source identifier for Google Calendar
	CalendarSourceName = "gcal"
	// CalendarAttendeeSource is the source identifier for calendar attendee import candidates
	CalendarAttendeeSource = "gcal_attendee"
	// CalendarDefaultInterval is the default sync interval for calendar events (daily)
	CalendarDefaultInterval = 24 * time.Hour
	// CalendarPastSyncDays is the number of days to sync into the past.
	// 1 year provides comprehensive meeting history for relationship context.
	CalendarPastSyncDays = 365
	// CalendarFutureSyncDays is the number of days to sync into the future.
	// 30 days captures near-term scheduled meetings without excessive API calls.
	CalendarFutureSyncDays = 30
)

// blockedCalendarDomains contains email domains that represent calendar resources,
// not real people. These are filtered out during calendar sync to avoid creating
// import candidates for meeting rooms, group calendars, and other non-person entities.
var blockedCalendarDomains = []string{
	"group.calendar.google.com",    // Group/secondary calendars
	"resource.calendar.google.com", // Room/resource calendars
	"calendar.google.com",          // Generic calendar resources
	"group.v.calendar.google.com",  // System calendars (holidays, birthdays)
}

// calendarRepoInterface defines the methods needed from calendar repository (for testability)
type calendarRepoInterface interface {
	Upsert(ctx context.Context, req repository.UpsertCalendarEventRequest) (*repository.CalendarEvent, error)
	ListPastEventsNeedingUpdate(ctx context.Context, before time.Time, limit int32) ([]repository.CalendarEvent, error)
	MarkLastContactedUpdated(ctx context.Context, id uuid.UUID) error
}

// contactRepoInterface defines the methods needed from contact repository (for testability)
type contactRepoInterface interface {
	UpdateContactLastContacted(ctx context.Context, id uuid.UUID, lastContacted time.Time) error
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

// CalendarSyncProvider implements SyncProvider for Google Calendar
type CalendarSyncProvider struct {
	oauthService        *OAuthService
	calendarRepo        calendarRepoInterface
	contactRepo         contactRepoInterface
	identityService     identityServiceInterface
	externalContactRepo externalContactRepoInterface
}

// NewCalendarSyncProvider creates a new Google Calendar sync provider
func NewCalendarSyncProvider(
	oauthService *OAuthService,
	calendarRepo *repository.CalendarEventRepository,
	contactRepo *repository.ContactRepository,
	identityService *service.IdentityService,
	externalContactRepo *repository.ExternalContactRepository,
) *CalendarSyncProvider {
	return &CalendarSyncProvider{
		oauthService:        oauthService,
		calendarRepo:        calendarRepo,
		contactRepo:         contactRepo,
		identityService:     identityService,
		externalContactRepo: externalContactRepo,
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

// processEvent processes a single calendar event
func (p *CalendarSyncProvider) processEvent(
	ctx context.Context,
	event *calendar.Event,
	accountID string,
) error {
	// Skip all-day events (holidays/birthdays, not meetings)
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

	// Determine user's response status
	userResponse := p.getUserResponse(event, accountID)

	// Only import events the user has firmly accepted
	// Skip: declined, tentative, needsAction, and events where user is not an attendee
	if userResponse == nil || *userResponse != "accepted" {
		logger.Debug().
			Str("eventId", event.Id).
			Str("userResponse", ptrToStr(userResponse)).
			Msg("skipping non-accepted event")
		return nil
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
		Status:               getEventStatus(event),
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

// buildAttendeeList builds the attendee list for storage
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

		// Skip calendar resource domains (rooms, group calendars, etc.)
		if isBlockedCalendarDomain(attendee.Email) {
			logger.Debug().
				Str("email", attendee.Email).
				Msg("skipping attendee from blocked calendar domain")
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
			case "email_personal", "email_work":
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

// updateLastContactedForPastEvents updates last_contacted for contacts in past events
func (p *CalendarSyncProvider) updateLastContactedForPastEvents(ctx context.Context) error {
	now := accelerated.GetCurrentTime()

	// Fetch past events that need updating (limit to 100 per run)
	events, err := p.calendarRepo.ListPastEventsNeedingUpdate(ctx, now, 100)
	if err != nil {
		return fmt.Errorf("list past events: %w", err)
	}

	for _, event := range events {
		// Update last_contacted for each matched contact
		for _, contactID := range event.MatchedContactIDs {
			if err := p.contactRepo.UpdateContactLastContacted(ctx, contactID, event.EndTime); err != nil {
				logger.Warn().
					Err(err).
					Str("contactId", contactID.String()).
					Str("eventId", event.ID.String()).
					Msg("failed to update last_contacted")
				continue
			}

			logger.Debug().
				Str("contactId", contactID.String()).
				Str("eventTitle", ptrToStr(event.Title)).
				Time("endTime", event.EndTime).
				Msg("updated last_contacted from calendar event")
		}

		// Mark event as processed
		if err := p.calendarRepo.MarkLastContactedUpdated(ctx, event.ID); err != nil {
			logger.Warn().
				Err(err).
				Str("eventId", event.ID.String()).
				Msg("failed to mark event as processed")
		}
	}

	if len(events) > 0 {
		logger.Info().
			Int("eventsProcessed", len(events)).
			Msg("updated last_contacted from past calendar events")
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

// isBlockedCalendarDomain checks if an email address belongs to a blocked calendar domain.
// These domains represent calendar resources (rooms, group calendars) rather than real people.
func isBlockedCalendarDomain(email string) bool {
	email = strings.ToLower(strings.TrimSpace(email))
	for _, domain := range blockedCalendarDomains {
		if strings.HasSuffix(email, "@"+domain) {
			return true
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
