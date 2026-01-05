package google

import (
	"context"
	"fmt"
	"strings"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/identity"
	"personal-crm/backend/internal/logger"
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
	// CalendarDefaultInterval is the default sync interval for calendar events
	CalendarDefaultInterval = 15 * time.Minute
	// CalendarSyncWindowDays is the number of days to sync in each direction
	CalendarSyncWindowDays = 30
)

// CalendarSyncProvider implements SyncProvider for Google Calendar
type CalendarSyncProvider struct {
	oauthService    *OAuthService
	calendarRepo    *repository.CalendarEventRepository
	contactRepo     *repository.ContactRepository
	identityService *service.IdentityService
}

// NewCalendarSyncProvider creates a new Google Calendar sync provider
func NewCalendarSyncProvider(
	oauthService *OAuthService,
	calendarRepo *repository.CalendarEventRepository,
	contactRepo *repository.ContactRepository,
	identityService *service.IdentityService,
) *CalendarSyncProvider {
	return &CalendarSyncProvider{
		oauthService:    oauthService,
		calendarRepo:    calendarRepo,
		contactRepo:     contactRepo,
		identityService: identityService,
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

// initialSync fetches events ±30 days from now and gets a sync token
func (p *CalendarSyncProvider) initialSync(
	ctx context.Context,
	calSvc *calendar.Service,
	accountID string,
	result *sync.SyncResult,
) (*sync.SyncResult, error) {
	now := accelerated.GetCurrentTime()
	timeMin := now.AddDate(0, 0, -CalendarSyncWindowDays).Format(time.RFC3339)
	timeMax := now.AddDate(0, 0, CalendarSyncWindowDays).Format(time.RFC3339)

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

	// Skip declined events
	if userResponse != nil && *userResponse == "declined" {
		logger.Debug().
			Str("eventId", event.Id).
			Msg("skipping declined event")
		return nil
	}

	// Build attendee list
	attendees := p.buildAttendeeList(event, accountID)

	// Match attendees to CRM contacts
	matchedContactIDs := p.matchAttendees(ctx, attendees, accountID)

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
func (p *CalendarSyncProvider) matchAttendees(
	ctx context.Context,
	attendees []repository.Attendee,
	accountID string,
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

		// Use identity service to match (discovery mode - no KnownContactID)
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
				Msg("failed to match attendee")
			continue
		}

		if result != nil && result.ContactID != nil {
			if !seen[*result.ContactID] {
				matchedIDs = append(matchedIDs, *result.ContactID)
				seen[*result.ContactID] = true
			}
		}
	}

	return matchedIDs
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
