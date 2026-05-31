// Package service — AnarlogDiscoveryService owns the People-tab
// "discovery" surface for anarlog_title weak candidates: the grouped
// list of normalized-token groups and the token-group resolve flow
// (import / link / ignore). It reuses the existing contact create/update
// service methods for the representative row, then atomically fans the
// outcome out to every sibling row via single-statement batch marks.
package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
)

// ErrTokenGroupNotFound is returned by ResolveToken when no live
// unmatched sibling rows exist for the supplied normalized token —
// either the token never existed or the group was already resolved.
// Handler maps to 404.
var ErrTokenGroupNotFound = errors.New("anarlog_title token group not found")

// ErrDiscoveryContactMissing is returned by ResolveToken for the link
// action when the supplied crm_contact_id does not resolve to a live
// contact. Handler maps to 404 (FK-safety: surface a clean not-found
// rather than a 500 from the downstream FK violation).
var ErrDiscoveryContactMissing = errors.New("link target contact not found")

// discoveryGroupRepo is the narrow external_contact surface the
// discovery service needs. Concrete is *repository.ExternalContactRepository.
type discoveryGroupRepo interface {
	ListAnarlogTitleGroups(ctx context.Context) ([]repository.AnarlogTitleGroup, error)
	FindAnarlogTitleSiblingsByToken(ctx context.Context, normalizedToken string) ([]repository.ExternalContact, error)
	MarkAnarlogTitleSiblingsImportedByToken(ctx context.Context, normalizedToken string, contactID uuid.UUID) error
	MarkAnarlogTitleSiblingsMatchedByToken(ctx context.Context, normalizedToken string, contactID uuid.UUID) error
	MarkAnarlogTitleSiblingsIgnoredByToken(ctx context.Context, normalizedToken string) error
}

// discoveryContactWriter is the narrow contact surface the discovery
// service needs. Concrete is *ContactService. CreateContact and
// UpdateContact are both error-returning and cadence-sole-writer-safe
// (UpdateContact routes cadence through CadenceUpdater.ApplyContactByOverride);
// GetContact backs the link FK check and the full-profile preserve read.
type discoveryContactWriter interface {
	GetContact(ctx context.Context, id uuid.UUID) (*repository.Contact, error)
	CreateContact(ctx context.Context, req repository.CreateContactRequest, methods []ContactMethodInput) (*repository.Contact, uuid.UUID, error)
	UpdateContact(ctx context.Context, id uuid.UUID, req repository.UpdateContactRequest, methods []ContactMethodInput, replaceMethods bool) (*repository.Contact, uuid.UUID, error)
}

// AnarlogDiscoveryService implements the discovery list + token-group
// resolve flow.
type AnarlogDiscoveryService struct {
	externalRepo discoveryGroupRepo
	contacts     discoveryContactWriter
}

// NewAnarlogDiscoveryService constructs the service bound to its narrow
// dependencies.
func NewAnarlogDiscoveryService(externalRepo discoveryGroupRepo, contacts discoveryContactWriter) *AnarlogDiscoveryService {
	return &AnarlogDiscoveryService{externalRepo: externalRepo, contacts: contacts}
}

// DiscoveryGroup is the projected view of one normalized-token group
// returned to the handler. EvidenceCount is the authoritative ranking
// signal (member-row count); SessionTitles are display-only.
type DiscoveryGroup struct {
	NormalizedToken string   `json:"normalized_token"`
	TokenDisplay    string   `json:"token_display"`
	EvidenceCount   int64    `json:"evidence_count"`
	SessionTitles   []string `json:"session_titles"`
}

// ListGroups returns the discovery groups ranked by evidence count.
func (s *AnarlogDiscoveryService) ListGroups(ctx context.Context) ([]DiscoveryGroup, error) {
	if s.externalRepo == nil {
		return nil, errors.New("anarlog discovery service not configured")
	}
	groups, err := s.externalRepo.ListAnarlogTitleGroups(ctx)
	if err != nil {
		return nil, fmt.Errorf("list anarlog_title groups: %w", err)
	}
	out := make([]DiscoveryGroup, 0, len(groups))
	for _, g := range groups {
		titles := g.SessionTitles
		if titles == nil {
			titles = []string{}
		}
		out = append(out, DiscoveryGroup{
			NormalizedToken: g.NormalizedToken,
			TokenDisplay:    g.TokenDisplay,
			EvidenceCount:   g.EvidenceCount,
			SessionTitles:   titles,
		})
	}
	return out, nil
}

// Discovery resolve actions.
const (
	DiscoveryActionImport = "import"
	DiscoveryActionLink   = "link"
	DiscoveryActionIgnore = "ignore"
)

// ResolveTokenRequest captures a validated token-group resolve choice.
// The handler is responsible for validating Action (oneof) and Cadence
// (oneof) before calling.
type ResolveTokenRequest struct {
	NormalizedToken string
	Action          string
	// Name overrides the contact name for import (defaults to the
	// title-cased token); for link, when non-nil it updates the linked
	// contact's name.
	Name *string
	// Cadence, when non-nil, sets the cadence on the created (import) or
	// linked (link) contact. Validated oneof at the handler.
	Cadence *string
	// CRMContactID is required for the link action.
	CRMContactID *uuid.UUID
}

// ResolveTokenResult is returned by ResolveToken. ContactID is the
// created contact id (import) or the linked crm_contact_id (link), and
// nil for ignore. The handler serializes it so the frontend can
// invalidate the affected contact's detail key.
type ResolveTokenResult struct {
	Action    string
	ContactID *uuid.UUID
}

// ResolveToken applies the import/link/ignore flow to an entire
// anarlog_title token group. The sibling set is re-derived server-side
// from the token; a client-supplied id list is never trusted. The
// representative (lowest-id) row anchors the reuse-existing-import-service
// contract, then the outcome fans out atomically to every sibling via a
// single-statement batch mark.
func (s *AnarlogDiscoveryService) ResolveToken(ctx context.Context, req ResolveTokenRequest) (*ResolveTokenResult, error) {
	if s.externalRepo == nil || s.contacts == nil {
		return nil, errors.New("anarlog discovery service not configured")
	}

	siblings, err := s.externalRepo.FindAnarlogTitleSiblingsByToken(ctx, req.NormalizedToken)
	if err != nil {
		return nil, fmt.Errorf("find token siblings: %w", err)
	}
	if len(siblings) == 0 {
		return nil, ErrTokenGroupNotFound
	}

	switch req.Action {
	case DiscoveryActionIgnore:
		if err := s.externalRepo.MarkAnarlogTitleSiblingsIgnoredByToken(ctx, req.NormalizedToken); err != nil {
			return nil, fmt.Errorf("mark siblings ignored: %w", err)
		}
		return &ResolveTokenResult{Action: DiscoveryActionIgnore}, nil

	case DiscoveryActionImport:
		return s.resolveImport(ctx, req, &siblings[0])

	case DiscoveryActionLink:
		return s.resolveLink(ctx, req)

	default:
		return nil, fmt.Errorf("unknown discovery action %q", req.Action)
	}
}

// resolveImport creates a CRM contact via the existing import path, then
// batch-marks every sibling 'imported'. The representative carries the
// title-cased token as the default name.
func (s *AnarlogDiscoveryService) resolveImport(ctx context.Context, req ResolveTokenRequest, representative *repository.ExternalContact) (*ResolveTokenResult, error) {
	name := discoveryDefaultName(req.Name, req.NormalizedToken, representative)
	createReq := repository.CreateContactRequest{
		FullName: name,
		Cadence:  req.Cadence,
	}
	contact, _, err := s.contacts.CreateContact(ctx, createReq, nil)
	if err != nil {
		return nil, fmt.Errorf("create contact for token group: %w", err)
	}
	if err := s.externalRepo.MarkAnarlogTitleSiblingsImportedByToken(ctx, req.NormalizedToken, contact.ID); err != nil {
		return nil, fmt.Errorf("mark siblings imported: %w", err)
	}
	id := contact.ID
	return &ResolveTokenResult{Action: DiscoveryActionImport, ContactID: &id}, nil
}

// resolveLink applies optional name/cadence to an existing contact, then
// batch-marks every sibling 'matched'. When neither name nor cadence is
// supplied the contact write is skipped entirely (the existence check
// still runs for FK safety). The sibling mark is gated on a successful
// (or skipped) contact write so a failed edit is never silently
// swallowed.
func (s *AnarlogDiscoveryService) resolveLink(ctx context.Context, req ResolveTokenRequest) (*ResolveTokenResult, error) {
	if req.CRMContactID == nil {
		return nil, errors.New("link action requires crm_contact_id")
	}
	contactID := *req.CRMContactID

	existing, err := s.contacts.GetContact(ctx, contactID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return nil, ErrDiscoveryContactMissing
		}
		return nil, fmt.Errorf("read link target contact: %w", err)
	}

	// Apply name/cadence only when supplied. UpdateContact is a
	// full-profile overwrite, so build a preserved request from the
	// existing contact and overlay only the supplied fields. When
	// neither is supplied, skip the write entirely.
	if req.Name != nil || req.Cadence != nil {
		updateReq := repository.UpdateContactRequest{
			FullName:     existing.FullName,
			Location:     existing.Location,
			Birthday:     existing.Birthday,
			HowMet:       existing.HowMet,
			Cadence:      existing.Cadence,
			ProfilePhoto: existing.ProfilePhoto,
		}
		if req.Name != nil {
			updateReq.FullName = *req.Name
		}
		if req.Cadence != nil {
			updateReq.Cadence = req.Cadence
		}
		if _, _, uErr := s.contacts.UpdateContact(ctx, contactID, updateReq, nil, false); uErr != nil {
			return nil, fmt.Errorf("update link target contact: %w", uErr)
		}
	}

	if err := s.externalRepo.MarkAnarlogTitleSiblingsMatchedByToken(ctx, req.NormalizedToken, contactID); err != nil {
		return nil, fmt.Errorf("mark siblings matched: %w", err)
	}
	id := contactID
	return &ResolveTokenResult{Action: DiscoveryActionLink, ContactID: &id}, nil
}

// discoveryDefaultName picks the contact name for an import: the
// caller's override when supplied and non-blank, else the
// representative's display_name (already title-cased by the discovery
// writer), else the title-cased normalized token as a last resort.
func discoveryDefaultName(override *string, normalizedToken string, representative *repository.ExternalContact) string {
	if override != nil && strings.TrimSpace(*override) != "" {
		return strings.TrimSpace(*override)
	}
	if representative != nil && representative.DisplayName != nil && strings.TrimSpace(*representative.DisplayName) != "" {
		return strings.TrimSpace(*representative.DisplayName)
	}
	return titleCaseToken(normalizedToken)
}

// titleCaseToken upper-cases the first byte of an ASCII-lowercased
// token. Mirrors the discovery writer's asciiTitleCase so a fallback
// name matches the on-disk display token shape.
func titleCaseToken(s string) string {
	if s == "" {
		return s
	}
	lower := strings.ToLower(s)
	first := lower[0]
	if first >= 'a' && first <= 'z' {
		b := []byte(lower)
		b[0] = first - 'a' + 'A'
		return string(b)
	}
	return lower
}
