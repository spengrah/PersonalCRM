package service

import (
	"context"
	"fmt"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/identity"
	"personal-crm/backend/internal/logger"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// MatchRequest represents a request to match an external identifier
type MatchRequest struct {
	RawIdentifier string
	Type          identity.IdentifierType
	Source        string
	SourceID      *string
	DisplayName   *string

	// KnownContactID allows contact-driven sync sources to skip the matching search.
	// When set, the identity is directly linked to this contact without searching
	// the contact_method table. Use this when the sync provider already knows the
	// contact (e.g., Gmail sync querying for a specific contact's emails).
	KnownContactID *uuid.UUID
}

// MatchResult represents the result of a match attempt
type MatchResult struct {
	Identity  *repository.ExternalIdentity
	ContactID *uuid.UUID
	MatchType repository.MatchType
	Cached    bool
}

// NormalizationPolicy controls how MatchOrCreateTx treats an identifier
// that normalizes to the empty string.
type NormalizationPolicy int

const (
	// NormalizationFailEmpty rejects an empty-after-normalization
	// identifier with an error. Use for callers where each envelope
	// carries exactly one identifier and an un-normalizable value is
	// fatal data (e.g. raw_message / call peer handles): there is
	// nothing to fall back to, and rejecting holds the daemon cursor
	// for retry rather than silently dropping the event.
	//
	// This is the iota zero value deliberately: the parameter is
	// required and positional so the zero value is never an implicit
	// default, but if a struct literal or reflection path ever produced
	// a zero value, failing (not silently dropping) is the correct
	// fail-closed choice.
	NormalizationFailEmpty NormalizationPolicy = iota

	// NormalizationSkipEmpty treats an empty-after-normalization
	// identifier as a no-op: MatchOrCreateTx returns (nil, nil) without
	// touching the database. Use for callers that loop over many
	// identifiers and want partial success (e.g. the external_contact
	// emails/phones loops): one junk field must not reject the whole
	// envelope.
	NormalizationSkipEmpty
)

// IdentityService handles identity matching operations
type IdentityService struct {
	identityRepo *repository.IdentityRepository
}

// NewIdentityService creates a new identity service
func NewIdentityService(identityRepo *repository.IdentityRepository) *IdentityService {
	return &IdentityService{
		identityRepo: identityRepo,
	}
}

// BackfillAnarlogIdentityForImport links the external_identity row
// keyed by an anarlog_human_id to the supplied CRM contact, mirroring
// the email/phone identity-link logic that runs at external_contact
// upsert time. Returns nil + no-op when no identity row exists yet,
// or when the existing identity is already linked to a different
// contact (manual user intent wins). Returns an error on any
// underlying repository failure so the caller can surface a 500.
//
// Used by the import handler after a user-driven import/link of an
// anarlog_humans external_contact. Keeps the handler clean of direct
// repository.IdentityRepository calls.
func (s *IdentityService) BackfillAnarlogIdentityForImport(ctx context.Context, anarlogHumanID string, contactID uuid.UUID) error {
	identities, err := s.identityRepo.FindIdentitiesByAnarlogHumanID(ctx, anarlogHumanID)
	if err != nil {
		return fmt.Errorf("anarlog import backfill: lookup identity: %w", err)
	}
	for _, ident := range identities {
		if ident.ContactID != nil && *ident.ContactID != contactID {
			logger.Warn().
				Str("anarlog_human_id", anarlogHumanID).
				Str("existing_contact_id", ident.ContactID.String()).
				Str("import_contact_id", contactID.String()).
				Msg("anarlog import backfill: identity already linked to different contact; skipping")
			continue
		}
		if _, err := s.identityRepo.LinkToContact(ctx, repository.LinkIdentityRequest{
			IdentityID: ident.ID,
			ContactID:  contactID,
			MatchType:  repository.MatchTypeManual,
		}); err != nil {
			return fmt.Errorf("anarlog import backfill: link identity: %w", err)
		}
	}
	return nil
}

// MatchOrCreate finds a matching contact or creates an unmatched identity record.
// This is the main entry point for identity matching during sync operations.
//
// Two modes of operation:
//   - Discovery mode (KnownContactID is nil): Searches contact_method table for matches.
//     Used by sources like Google Contacts that sync everything and need to find matches.
//   - Contact-driven mode (KnownContactID is set): Skips search and directly links to the
//     known contact. Used by sources like Gmail that query for specific contacts' data.
func (s *IdentityService) MatchOrCreate(ctx context.Context, req MatchRequest) (*MatchResult, error) {
	// 1. Normalize the identifier
	normalized := identity.Normalize(req.RawIdentifier, req.Type)
	if normalized == "" {
		return nil, fmt.Errorf("empty identifier after normalization")
	}

	// 2. Fast path: caller already knows the contact (contact-driven sync)
	if req.KnownContactID != nil {
		return s.recordKnownMatch(ctx, normalized, req)
	}

	// 3. Discovery path: check cache first
	existing, err := s.identityRepo.GetByIdentifier(ctx, req.Type, normalized, req.Source)
	if err == nil && existing.ContactID != nil {
		logger.Debug().
			Str("identifier", normalized).
			Str("source", req.Source).
			Str("contact_id", existing.ContactID.String()).
			Msg("found cached identity match")
		return &MatchResult{
			Identity:  existing,
			ContactID: existing.ContactID,
			MatchType: existing.MatchType,
			Cached:    true,
		}, nil
	}

	// 4. Discovery path: search contact_method table for matches
	contactID, matchType := s.findContactByMethod(ctx, normalized, req.Type)

	// 5. Store/update the identity record
	now := accelerated.GetCurrentTime()
	upsertReq := repository.UpsertIdentityRequest{
		Identifier:     normalized,
		IdentifierType: req.Type,
		RawIdentifier:  &req.RawIdentifier,
		Source:         req.Source,
		SourceID:       req.SourceID,
		ContactID:      contactID,
		MatchType:      matchType,
		DisplayName:    req.DisplayName,
		LastSeenAt:     &now,
		MessageCount:   1,
	}

	if matchType == repository.MatchTypeExact {
		confidence := 1.0
		upsertReq.MatchConfidence = &confidence
	}

	ident, err := s.identityRepo.Upsert(ctx, upsertReq)
	if err != nil {
		return nil, fmt.Errorf("upsert identity: %w", err)
	}

	logger.Debug().
		Str("identifier", normalized).
		Str("source", req.Source).
		Str("match_type", string(matchType)).
		Msg("identity match result")

	return &MatchResult{
		Identity:  ident,
		ContactID: contactID,
		MatchType: matchType,
		Cached:    false,
	}, nil
}

// recordKnownMatch handles the fast path for contact-driven sync sources.
// It records the identity mapping without searching for matches.
func (s *IdentityService) recordKnownMatch(ctx context.Context, normalized string, req MatchRequest) (*MatchResult, error) {
	now := accelerated.GetCurrentTime()
	confidence := 1.0

	upsertReq := repository.UpsertIdentityRequest{
		Identifier:      normalized,
		IdentifierType:  req.Type,
		RawIdentifier:   &req.RawIdentifier,
		Source:          req.Source,
		SourceID:        req.SourceID,
		ContactID:       req.KnownContactID,
		MatchType:       repository.MatchTypeExact,
		MatchConfidence: &confidence,
		DisplayName:     req.DisplayName,
		LastSeenAt:      &now,
		MessageCount:    1,
	}

	ident, err := s.identityRepo.Upsert(ctx, upsertReq)
	if err != nil {
		return nil, fmt.Errorf("upsert known identity: %w", err)
	}

	logger.Debug().
		Str("identifier", normalized).
		Str("source", req.Source).
		Str("contact_id", req.KnownContactID.String()).
		Msg("recorded known identity match")

	return &MatchResult{
		Identity:  ident,
		ContactID: req.KnownContactID,
		MatchType: repository.MatchTypeExact,
		Cached:    false,
	}, nil
}

// MatchOrCreateTx is the tx-bound variant of MatchOrCreate. The
// identity-row upsert and contact-method search both run inside the
// caller's transaction, so a downstream failure in the same tx rolls
// back the identity write atomically with whatever else the caller
// is doing (e.g., the ingest service's staging-row insert).
//
// Error-handling divergence vs the non-tx MatchOrCreate: the
// underlying findContactByMethodTx propagates repository errors
// rather than swallowing them and degrading to MatchTypeUnmatched.
// In the ingest hot path a silent degrade would strand the row AND
// let the daemon advance its cursor — the caller wants to surface a
// per-event rejection instead. The non-tx MatchOrCreate keeps its
// existing forgiving behavior for the sync providers that already
// depend on it.
//
// The policy parameter controls the empty-after-normalization case:
//   - NormalizationFailEmpty returns an error (for single-identifier
//     callers where an un-normalizable value is fatal data).
//   - NormalizationSkipEmpty returns (nil, nil) without touching the
//     database (for loop callers that want partial success). Callers
//     under this policy must treat a nil result as "nothing to match";
//     only this policy can return (nil, nil).
func (s *IdentityService) MatchOrCreateTx(ctx context.Context, tx pgx.Tx, req MatchRequest, policy NormalizationPolicy) (*MatchResult, error) {
	normalized := identity.Normalize(req.RawIdentifier, req.Type)
	if normalized == "" {
		switch policy {
		case NormalizationSkipEmpty:
			return nil, nil
		default: // NormalizationFailEmpty (and the fail-closed zero value)
			return nil, fmt.Errorf("empty identifier after normalization")
		}
	}

	// Contact-driven mode is intentionally NOT supported on the tx
	// path today — the ingest hot path is always discovery-mode
	// (the daemon does not know which contact the peer belongs to).
	// Future callers can extend; reject for now to keep the surface
	// minimal.
	if req.KnownContactID != nil {
		return nil, fmt.Errorf("MatchOrCreateTx: KnownContactID is not supported")
	}

	// Cache check (matches MatchOrCreate's step 3): if we already
	// know this identifier for this source AND a contact, return the
	// cached match.
	existing, err := s.identityRepo.GetByIdentifierTx(ctx, tx, req.Type, normalized, req.Source)
	if err == nil && existing.ContactID != nil {
		return &MatchResult{
			Identity:  existing,
			ContactID: existing.ContactID,
			MatchType: existing.MatchType,
			Cached:    true,
		}, nil
	}

	// Discovery path: search contact_method for a match. Error
	// propagation (vs. swallow + unmatched) is the deliberate
	// divergence from the non-tx path — see method doc.
	contactID, matchType, err := s.findContactByMethodTx(ctx, tx, normalized, req.Type)
	if err != nil {
		return nil, fmt.Errorf("find contact methods: %w", err)
	}

	now := accelerated.GetCurrentTime()
	upsertReq := repository.UpsertIdentityRequest{
		Identifier:     normalized,
		IdentifierType: req.Type,
		RawIdentifier:  &req.RawIdentifier,
		Source:         req.Source,
		SourceID:       req.SourceID,
		ContactID:      contactID,
		MatchType:      matchType,
		DisplayName:    req.DisplayName,
		LastSeenAt:     &now,
		MessageCount:   1,
	}
	if matchType == repository.MatchTypeExact {
		confidence := 1.0
		upsertReq.MatchConfidence = &confidence
	}

	ident, err := s.identityRepo.UpsertTx(ctx, tx, upsertReq)
	if err != nil {
		return nil, fmt.Errorf("upsert identity: %w", err)
	}

	return &MatchResult{
		Identity:  ident,
		ContactID: contactID,
		MatchType: matchType,
		Cached:    false,
	}, nil
}

// findContactByMethodTx is the tx-bound variant of findContactByMethod.
// CRITICALLY, it propagates errors rather than swallowing them — see
// MatchOrCreateTx for why this divergence matters in the ingest hot
// path.
func (s *IdentityService) findContactByMethodTx(ctx context.Context, tx pgx.Tx, identifier string, idType identity.IdentifierType) (*uuid.UUID, repository.MatchType, error) {
	methodTypes := identity.MapIdentifierTypeToContactMethodTypes(idType)
	if len(methodTypes) == 0 {
		return nil, repository.MatchTypeUnmatched, nil
	}
	typeStrings := make([]string, len(methodTypes))
	for i, mt := range methodTypes {
		typeStrings[i] = string(mt)
	}

	matches, err := s.identityRepo.FindContactMethodsByValueTx(ctx, tx, typeStrings, identifier)
	if err != nil {
		return nil, repository.MatchTypeUnmatched, err
	}

	if len(matches) == 0 {
		return nil, repository.MatchTypeUnmatched, nil
	}
	if len(matches) == 1 {
		return &matches[0].ContactID, repository.MatchTypeExact, nil
	}
	// Multiple matches — ambiguous, leave for user to resolve.
	logger.Warn().
		Str("identifier", identifier).
		Int("match_count", len(matches)).
		Msg("ambiguous identity match - multiple contacts found")
	return nil, repository.MatchTypeUnmatched, nil
}

// findContactByMethod searches contact_method table for a match
func (s *IdentityService) findContactByMethod(ctx context.Context, identifier string, idType identity.IdentifierType) (*uuid.UUID, repository.MatchType) {
	// Map identity type to contact method types
	methodTypes := identity.MapIdentifierTypeToContactMethodTypes(idType)
	if len(methodTypes) == 0 {
		return nil, repository.MatchTypeUnmatched
	}

	// Convert to string slice for query
	typeStrings := make([]string, len(methodTypes))
	for i, mt := range methodTypes {
		typeStrings[i] = string(mt)
	}

	// Find matching contact methods
	matches, err := s.identityRepo.FindContactMethodsByValue(ctx, typeStrings, identifier)
	if err != nil {
		logger.Warn().
			Err(err).
			Str("identifier", identifier).
			Msg("error finding contact methods")
		return nil, repository.MatchTypeUnmatched
	}

	// Handle match results
	if len(matches) == 0 {
		return nil, repository.MatchTypeUnmatched
	}

	if len(matches) == 1 {
		// Unique match found
		return &matches[0].ContactID, repository.MatchTypeExact
	}

	// Multiple matches - ambiguous, let user resolve
	logger.Warn().
		Str("identifier", identifier).
		Int("match_count", len(matches)).
		Msg("ambiguous identity match - multiple contacts found")
	return nil, repository.MatchTypeUnmatched
}

// LinkIdentity manually links an identity to a contact
func (s *IdentityService) LinkIdentity(ctx context.Context, identityID, contactID uuid.UUID) (*repository.ExternalIdentity, error) {
	confidence := 1.0
	return s.identityRepo.LinkToContact(ctx, repository.LinkIdentityRequest{
		IdentityID:      identityID,
		ContactID:       contactID,
		MatchType:       repository.MatchTypeManual,
		MatchConfidence: &confidence,
	})
}

// UnlinkIdentity unlinks an identity from its contact
func (s *IdentityService) UnlinkIdentity(ctx context.Context, identityID uuid.UUID) (*repository.ExternalIdentity, error) {
	return s.identityRepo.UnlinkFromContact(ctx, identityID)
}

// GetIdentity retrieves an identity by ID
func (s *IdentityService) GetIdentity(ctx context.Context, id uuid.UUID) (*repository.ExternalIdentity, error) {
	return s.identityRepo.GetByID(ctx, id)
}

// ListUnmatchedIdentities returns unmatched identities with pagination
func (s *IdentityService) ListUnmatchedIdentities(ctx context.Context, limit, offset int32) ([]repository.ExternalIdentity, error) {
	return s.identityRepo.ListUnmatched(ctx, limit, offset)
}

// CountUnmatchedIdentities returns the count of unmatched identities
func (s *IdentityService) CountUnmatchedIdentities(ctx context.Context) (int64, error) {
	return s.identityRepo.CountUnmatched(ctx)
}

// ListIdentitiesForContact returns all identities linked to a contact
func (s *IdentityService) ListIdentitiesForContact(ctx context.Context, contactID uuid.UUID) ([]repository.ExternalIdentity, error) {
	return s.identityRepo.ListForContact(ctx, contactID)
}

// DeleteIdentity removes an identity
func (s *IdentityService) DeleteIdentity(ctx context.Context, id uuid.UUID) error {
	return s.identityRepo.Delete(ctx, id)
}

// BulkLinkIdentities links multiple identities to a contact
func (s *IdentityService) BulkLinkIdentities(ctx context.Context, identityIDs []uuid.UUID, contactID uuid.UUID) error {
	confidence := 1.0
	return s.identityRepo.BulkLinkToContact(ctx, identityIDs, contactID, repository.MatchTypeManual, &confidence)
}

// IncrementMessageCount updates the message count for an identity
func (s *IdentityService) IncrementMessageCount(ctx context.Context, id uuid.UUID, count int32) (*repository.ExternalIdentity, error) {
	now := accelerated.GetCurrentTime()
	return s.identityRepo.UpdateMessageCount(ctx, id, count, now)
}
