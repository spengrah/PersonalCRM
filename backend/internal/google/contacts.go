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
	"google.golang.org/api/option"
	"google.golang.org/api/people/v1"
)

const (
	// ContactsSourceName is the source identifier for Google Contacts
	ContactsSourceName = "gcontacts"
	// ContactsDefaultInterval is the default sync interval for contacts
	ContactsDefaultInterval = 24 * time.Hour
	// ContactsPersonFields specifies which fields to fetch from the People API
	ContactsPersonFields = "names,emailAddresses,phoneNumbers,addresses,organizations,birthdays,photos"
)

// ContactsProvider implements SyncProvider for Google Contacts
type ContactsProvider struct {
	oauthService    *OAuthService
	externalRepo    *repository.ExternalContactRepository
	enricher        *service.EnrichmentService
	identityService *service.IdentityService
	// reconciler re-propagates address-book methods onto already-linked
	// (or duplicate-of-linked) contacts on every resync, closing the
	// "enrich once at first match then never again" leak. May be nil in
	// tests that don't exercise the forward reconcile.
	reconciler *service.AddressBookReconcileService
}

// NewContactsProvider creates a new Google Contacts sync provider. The
// reconciler may be nil for unit tests that don't exercise the forward
// reconcile hook.
func NewContactsProvider(
	oauthService *OAuthService,
	externalRepo *repository.ExternalContactRepository,
	enricher *service.EnrichmentService,
	identityService *service.IdentityService,
	reconciler *service.AddressBookReconcileService,
) *ContactsProvider {
	return &ContactsProvider{
		oauthService:    oauthService,
		externalRepo:    externalRepo,
		enricher:        enricher,
		identityService: identityService,
		reconciler:      reconciler,
	}
}

// Config returns the provider's configuration
func (p *ContactsProvider) Config() sync.SourceConfig {
	return sync.SourceConfig{
		Name:                 ContactsSourceName,
		DisplayName:          "Google Contacts",
		Strategy:             repository.SyncStrategyFetchAll,
		SupportsMultiAccount: true,
		SupportsDiscovery:    true,
		DefaultInterval:      ContactsDefaultInterval,
	}
}

// Sync performs the contact sync for a specific account
func (p *ContactsProvider) Sync(
	ctx context.Context,
	state *repository.SyncState,
	contacts []repository.Contact,
) (*sync.SyncResult, error) {
	// Account ID is required for Google Contacts sync
	if state.AccountID == nil {
		return nil, fmt.Errorf("account ID required for Google Contacts sync")
	}
	accountID := *state.AccountID

	logger.Info().
		Str("source", ContactsSourceName).
		Str("account", accountID).
		Msg("starting Google Contacts sync")

	// Get authenticated client for this account
	client, err := p.oauthService.GetClientForAccount(ctx, accountID)
	if err != nil {
		return nil, fmt.Errorf("get OAuth client: %w", err)
	}

	// Create People API service
	peopleSvc, err := people.NewService(ctx, option.WithHTTPClient(client))
	if err != nil {
		return nil, fmt.Errorf("create People service: %w", err)
	}

	result := &sync.SyncResult{}

	// Build the request
	req := peopleSvc.People.Connections.List("people/me").
		PersonFields(ContactsPersonFields).
		PageSize(1000)

	// Use sync token if available for incremental sync
	if state.SyncCursor != nil && *state.SyncCursor != "" {
		req = req.SyncToken(*state.SyncCursor)
		logger.Debug().Str("syncToken", *state.SyncCursor).Msg("using incremental sync")
	}

	// Paginate through all contacts
	var pageToken string
	for {
		if pageToken != "" {
			req = req.PageToken(pageToken)
		}

		resp, err := req.Do()
		if err != nil {
			return result, fmt.Errorf("list connections: %w", err)
		}

		// Process each contact
		for _, person := range resp.Connections {
			if err := p.processContact(ctx, person, accountID); err != nil {
				logger.Warn().
					Err(err).
					Str("resource", person.ResourceName).
					Msg("failed to process contact")
				continue
			}
			result.ItemsProcessed++
		}

		// Check if there's a next page
		pageToken = resp.NextPageToken
		if pageToken == "" {
			// Store the sync token for next incremental sync
			if resp.NextSyncToken != "" {
				result.NewCursor = resp.NextSyncToken
			}
			break
		}
	}

	logger.Info().
		Str("source", ContactsSourceName).
		Str("account", accountID).
		Int("processed", result.ItemsProcessed).
		Int("matched", result.ItemsMatched).
		Int("created", result.ItemsCreated).
		Msg("Google Contacts sync completed")

	return result, nil
}

// ValidateCredentials checks if the Google credentials are valid
func (p *ContactsProvider) ValidateCredentials(ctx context.Context, accountID *string) error {
	if accountID == nil {
		// Check if any accounts exist
		accounts, err := p.oauthService.ListAccounts(ctx)
		if err != nil {
			return err
		}
		if len(accounts) == 0 {
			return fmt.Errorf("no Google accounts connected")
		}
		return nil
	}

	// Validate specific account
	_, err := p.oauthService.GetClientForAccount(ctx, *accountID)
	return err
}

// processContact processes a single Google contact
func (p *ContactsProvider) processContact(
	ctx context.Context,
	person *people.Person,
	accountID string,
) error {
	// Skip contacts without useful data
	if len(person.Names) == 0 && len(person.EmailAddresses) == 0 && len(person.PhoneNumbers) == 0 {
		return nil
	}

	// Convert to external contact request
	req := p.convertPersonToRequest(person, accountID)

	// Upsert external contact
	externalContact, err := p.externalRepo.Upsert(ctx, req)
	if err != nil {
		return fmt.Errorf("upsert external contact: %w", err)
	}

	// Check for duplicates across accounts. This may mark the row as a
	// duplicate (sets duplicate_of_id in the DB) but does NOT update the
	// local externalContact struct.
	if err := p.checkDuplicates(ctx, externalContact); err != nil {
		logger.Debug().Err(err).Msg("duplicate check failed")
	}

	// Re-read so attemptMatch sees a freshly-set duplicate_of_id. Without
	// this, a row just marked duplicate (whose CRMContactID is still nil)
	// would skip attemptMatch's duplicate guard and auto-enrich directly —
	// bypassing the duplicate-aware effective-status precedence and
	// auto-pushing methods that should only be reconciled into the
	// canonical contact (and only suggested when the canonical is curated).
	//
	// Fail CLOSED: if the re-read errors, we cannot trust the local
	// struct's duplicate_of_id, so we SKIP attemptMatch (running it on a
	// stale struct could re-open the duplicate bypass) and still run the
	// forward reconcile, which re-reads the row itself via
	// ResolveReconcileTarget. The next sync retries the match cleanly.
	reread, rereadErr := p.externalRepo.GetByID(ctx, externalContact.ID)
	if rereadErr != nil || reread == nil {
		logger.Warn().Err(rereadErr).Msg("gcontacts: re-read after duplicate check failed; skipping attemptMatch this sync")
		p.reconcileMethods(ctx, externalContact.ID)
		return nil
	}
	externalContact = reread

	// Attempt to match to CRM contact
	if err := p.attemptMatch(ctx, externalContact); err != nil {
		logger.Debug().Err(err).Msg("match attempt failed")
	}

	// Forward reconcile: re-propagate methods onto an already-linked (or
	// duplicate-of-linked) contact. This is what closes the leak — the
	// attemptMatch early-return enriches only at FIRST match, so a method
	// the address book gains later for an already-linked person never
	// reaches the CRM contact without this step. Best-effort: a reconcile
	// failure must not fail the whole sync item.
	p.reconcileMethods(ctx, externalContact.ID)

	return nil
}

// reconcileMethods resolves the (possibly duplicate) row to its effective
// contact + status and reconciles its methods (auto-propagate for
// matched, record suggestion for imported). No-op when the reconciler is
// unwired, the row is unmatched / not a dup-of-linked, or the row is
// effectively ignored. Best-effort: errors are logged, never propagated,
// since address-book sync items must not fail on a reconcile hiccup.
func (p *ContactsProvider) reconcileMethods(ctx context.Context, externalID uuid.UUID) {
	if p.reconciler == nil {
		return
	}
	if err := p.reconciler.ResolveAndReconcile(ctx, externalID); err != nil {
		// No Err()/id attached: a downstream enrichment error can embed a
		// normalized method value (PII under this repo's model). Log only
		// that a reconcile failed for this sync item.
		logger.Warn().Msg("gcontacts: method reconcile failed for one row")
	}
}

// convertPersonToRequest converts a Google Person to an upsert request
func (p *ContactsProvider) convertPersonToRequest(
	person *people.Person,
	accountID string,
) repository.UpsertExternalContactRequest {
	req := repository.UpsertExternalContactRequest{
		Source:    ContactsSourceName,
		SourceID:  person.ResourceName,
		AccountID: &accountID,
	}

	// Extract names
	if len(person.Names) > 0 {
		name := person.Names[0]
		if name.DisplayName != "" {
			req.DisplayName = &name.DisplayName
		}
		if name.GivenName != "" {
			req.FirstName = &name.GivenName
		}
		if name.FamilyName != "" {
			req.LastName = &name.FamilyName
		}
	}

	// Extract emails
	req.Emails = make([]repository.EmailEntry, 0, len(person.EmailAddresses))
	for _, email := range person.EmailAddresses {
		entry := repository.EmailEntry{
			Value: email.Value,
			Type:  email.Type,
		}
		if email.Metadata != nil {
			entry.Primary = email.Metadata.Primary
		}
		req.Emails = append(req.Emails, entry)
	}

	// Extract phones
	req.Phones = make([]repository.PhoneEntry, 0, len(person.PhoneNumbers))
	for _, phone := range person.PhoneNumbers {
		entry := repository.PhoneEntry{
			Value: phone.Value,
			Type:  phone.Type,
		}
		if phone.Metadata != nil {
			entry.Primary = phone.Metadata.Primary
		}
		req.Phones = append(req.Phones, entry)
	}

	// Extract addresses
	req.Addresses = make([]repository.AddressEntry, 0, len(person.Addresses))
	for _, addr := range person.Addresses {
		entry := repository.AddressEntry{
			Formatted: addr.FormattedValue,
			Type:      addr.Type,
		}
		req.Addresses = append(req.Addresses, entry)
	}

	// Extract organization
	if len(person.Organizations) > 0 {
		org := person.Organizations[0]
		if org.Name != "" {
			req.Organization = &org.Name
		}
		if org.Title != "" {
			req.JobTitle = &org.Title
		}
	}

	// Extract birthday
	if len(person.Birthdays) > 0 && person.Birthdays[0].Date != nil {
		date := person.Birthdays[0].Date
		if date.Year > 0 && date.Month > 0 && date.Day > 0 {
			t := time.Date(int(date.Year), time.Month(date.Month), int(date.Day), 0, 0, 0, 0, time.UTC)
			req.Birthday = &t
		}
	}

	// Extract photo URL
	if len(person.Photos) > 0 && person.Photos[0].Url != "" {
		req.PhotoURL = &person.Photos[0].Url
	}

	// Extract etag
	if person.Etag != "" {
		req.Etag = &person.Etag
	}

	// Set sync time
	now := accelerated.GetCurrentTime()
	req.SyncedAt = &now

	return req
}

// checkDuplicates checks if this contact is a duplicate of another across accounts
func (p *ContactsProvider) checkDuplicates(
	ctx context.Context,
	contact *repository.ExternalContact,
) error {
	// Check by email
	for _, email := range contact.Emails {
		normalized := strings.ToLower(email.Value)
		matches, err := p.externalRepo.FindByNormalizedEmail(ctx, normalized)
		if err != nil {
			continue
		}

		for _, match := range matches {
			// Skip self
			if match.ID == contact.ID {
				continue
			}

			// Found a duplicate - mark this one as duplicate of the older one
			if match.CreatedAt.Before(contact.CreatedAt) {
				if err := p.externalRepo.MarkAsDuplicate(ctx, contact.ID, match.ID); err != nil {
					logger.Warn().Err(err).Msg("failed to mark duplicate")
				}
				return nil
			}
		}
	}

	return nil
}

// attemptMatch attempts to match an external contact to a CRM contact
func (p *ContactsProvider) attemptMatch(
	ctx context.Context,
	contact *repository.ExternalContact,
) error {
	// Skip if already matched
	if contact.CRMContactID != nil {
		return nil
	}

	// Skip duplicates
	if contact.DuplicateOfID != nil {
		return nil
	}

	// Try matching by email first
	for _, email := range contact.Emails {
		result, err := p.identityService.MatchOrCreate(ctx, service.MatchRequest{
			RawIdentifier: email.Value,
			Type:          identity.IdentifierTypeEmail,
			Source:        ContactsSourceName,
			DisplayName:   contact.DisplayName,
		})
		if err != nil {
			continue
		}

		if result != nil && result.ContactID != nil {
			// Found a match!
			if _, err := p.externalRepo.UpdateMatch(ctx, contact.ID, result.ContactID, repository.MatchStatusMatched); err != nil {
				return fmt.Errorf("update match: %w", err)
			}

			// Enrich the CRM contact with external data.
			// Any rematch job kicked off by the newly-added methods is fire-and-forget —
			// sync paths have no HTTP response to surface it on.
			enrichedContact, _ := p.externalRepo.GetByID(ctx, contact.ID)
			if enrichedContact != nil {
				if _, err := p.enricher.EnrichContactFromExternal(ctx, *result.ContactID, enrichedContact); err != nil {
					logger.Warn().Err(err).Msg("enrichment failed")
				}
			}
			return nil
		}
	}

	// Try matching by phone
	for _, phone := range contact.Phones {
		result, err := p.identityService.MatchOrCreate(ctx, service.MatchRequest{
			RawIdentifier: phone.Value,
			Type:          identity.IdentifierTypePhone,
			Source:        ContactsSourceName,
			DisplayName:   contact.DisplayName,
		})
		if err != nil {
			continue
		}

		if result != nil && result.ContactID != nil {
			// Found a match!
			if _, err := p.externalRepo.UpdateMatch(ctx, contact.ID, result.ContactID, repository.MatchStatusMatched); err != nil {
				return fmt.Errorf("update match: %w", err)
			}

			// Enrich the CRM contact. Any rematch job is fire-and-forget (see above).
			enrichedContact, _ := p.externalRepo.GetByID(ctx, contact.ID)
			if enrichedContact != nil {
				if _, err := p.enricher.EnrichContactFromExternal(ctx, *result.ContactID, enrichedContact); err != nil {
					logger.Warn().Err(err).Msg("enrichment failed")
				}
			}
			return nil
		}
	}

	return nil
}
