package service

import (
	"context"
	"fmt"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/identity"
	"personal-crm/backend/internal/logger"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
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
