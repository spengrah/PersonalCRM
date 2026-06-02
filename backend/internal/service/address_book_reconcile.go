package service

import (
	"context"
	"fmt"

	"personal-crm/backend/internal/identity"
	"personal-crm/backend/internal/logger"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
)

// AddressBookReconcileService re-propagates address-book contact methods
// onto an already-linked CRM contact after the upstream entry gains a
// method. It closes the long-standing leak where gcontacts enriched a
// contact ONCE at first match (then early-returned on every resync) and
// icloud never enriched at all.
//
// It composes the existing EnrichmentService + ExternalContactRepository
// rather than widening EnrichmentService's many-call-site constructor.
// Layering: Handler/CLI → AddressBookReconcileService →
// EnrichmentService / Repository → DB.
//
// The auto-vs-suggest branch keys off the effective match status the
// caller resolved (the duplicate-aware precedence — ignored > imported >
// matched — is applied by the repository's
// ListLinkedAddressBookExternalContactsForReconcile / ResolveReconcileTarget
// before this service is called):
//   - matched  → AUTO-PROPAGATE missing methods via
//     EnrichContactFromExternal (publishes KindContactMethodsAdded →
//     rematch fan-out incl. GmailRematchHandler).
//   - imported → RECORD the missing set as a pending suggestion (no reader
//     yet — the suggestions surface that consumes it is a later change).
//     Always overwrites, clearing to SQL NULL when the recomputed set is
//     empty.
//   - anything else → no-op.
type AddressBookReconcileService struct {
	enricher     *EnrichmentService
	methodRepo   *repository.ContactMethodRepository
	externalRepo *repository.ExternalContactRepository
}

// NewAddressBookReconcileService builds the reconcile service. All three
// dependencies are required.
func NewAddressBookReconcileService(
	enricher *EnrichmentService,
	methodRepo *repository.ContactMethodRepository,
	externalRepo *repository.ExternalContactRepository,
) *AddressBookReconcileService {
	return &AddressBookReconcileService{
		enricher:     enricher,
		methodRepo:   methodRepo,
		externalRepo: externalRepo,
	}
}

// ReconcileResult is the per-row outcome. MethodsAutoApplied is the
// number of contact methods the matched branch added (computed via a
// pre/post dedup-key diff, since EnrichContactFromExternal returns only a
// rematch jobID). SuggestionsRecorded is 1 iff the imported branch wrote
// a non-empty pending suggestion set, else 0.
type ReconcileResult struct {
	MethodsAutoApplied  int
	SuggestionsRecorded int
}

// addressBookReconcileSources is the fixed set of address-book sources
// the one-time catchup reconciles. Telegram / gcal_attendee / anarlog_*
// are out of scope (their own match/enrich flows).
var addressBookReconcileSources = []string{"gcontacts", "icloud_contacts"}

// ReconcileAllResult is the catchup summary. Failed counts rows whose
// reconcile errored (the loop continues past them); the catchup exits
// non-zero iff Failed > 0.
type ReconcileAllResult struct {
	Scanned             int
	MethodsAutoApplied  int
	SuggestionsRecorded int
	Failed              int
}

// ReconcileAllAddressBookMethods runs the one-time catchup: it lists
// every live linked-or-dup-of-linked address-book row and reconciles
// each. Continue-on-error — a single row's failure is logged with a
// NON-IDENTIFYING ordinal index only (no externalID/contactID/email/
// source — all PII under this repo's model) and tallied in Failed; the
// loop proceeds. Idempotent (a clean re-run after fixing the cause adds
// nothing), so re-running is safe. No transaction spans the loop (each
// enrich owns its own tx).
func (s *AddressBookReconcileService) ReconcileAllAddressBookMethods(ctx context.Context) (ReconcileAllResult, error) {
	targets, err := s.externalRepo.ListLinkedAddressBookExternalContactsForReconcile(ctx, addressBookReconcileSources)
	if err != nil {
		return ReconcileAllResult{}, fmt.Errorf("list linked address-book contacts for reconcile: %w", err)
	}

	var summary ReconcileAllResult
	summary.Scanned = len(targets)
	for i, target := range targets {
		res, reconErr := s.ReconcileLinkedExternalContactMethods(ctx, target)
		if reconErr != nil {
			summary.Failed++
			// Ordinal-only log: never emit row/contact ids or method
			// values (PII). The error class is safe to surface.
			logger.Warn().
				Err(reconErr).
				Int("ordinal", i).
				Msg("address-book method reconcile failed for one row; continuing")
			continue
		}
		summary.MethodsAutoApplied += res.MethodsAutoApplied
		summary.SuggestionsRecorded += res.SuggestionsRecorded
	}
	return summary, nil
}

// ResolveAndReconcile resolves an external_contact row (by id) to its
// effective contact + status via the repository's duplicate-aware
// precedence, then reconciles. A no-op (nil error) when the row is
// missing/tombstoned, unmatched, ignored, or resolves to no live
// contact. Used by the icloud
// post-commit hook, where only the committed row id is known and the
// resolution must read the freshly-committed match state. Satisfies the
// service.AddressBookReconciler interface.
func (s *AddressBookReconcileService) ResolveAndReconcile(
	ctx context.Context,
	externalID uuid.UUID,
) error {
	target, err := s.externalRepo.ResolveReconcileTarget(ctx, externalID)
	if err != nil {
		return fmt.Errorf("resolve reconcile target: %w", err)
	}
	if target == nil {
		return nil
	}
	if _, err := s.ReconcileLinkedExternalContactMethods(ctx, *target); err != nil {
		return err
	}
	return nil
}

// ReconcileLinkedExternalContactMethods reconciles one resolved target.
// Guard: a nil effective contact id is a no-op. The matched branch
// auto-propagates; the imported branch records (and clears) suggestions.
func (s *AddressBookReconcileService) ReconcileLinkedExternalContactMethods(
	ctx context.Context,
	target repository.ReconcileTarget,
) (ReconcileResult, error) {
	if target.EffectiveContactID == uuid.Nil {
		return ReconcileResult{}, nil
	}

	switch target.EffectiveStatus {
	case repository.MatchStatusMatched:
		return s.autoPropagate(ctx, target.EffectiveContactID, &target.ExternalContact)
	case repository.MatchStatusImported:
		return s.recordSuggestions(ctx, target.EffectiveContactID, &target.ExternalContact)
	default:
		return ReconcileResult{}, nil
	}
}

// autoPropagate runs EnrichContactFromExternal and counts the methods it
// added via a pre/post dedup-key diff. EnrichContactFromExternal only
// adds missing methods and publishes KindContactMethodsAdded for the
// added set, so re-running is idempotent (zero added → no event).
//
// EnrichContactFromExternal's method-insert loop logs and continues on a
// per-method insert error (it does not abort the call), so a partial
// silent failure would otherwise be invisible. We compute the expected
// missing set from the external row's methods and compare it to the
// actually-applied delta; a shortfall is surfaced as an error so the
// catchup counts the row as failed (and exits non-zero) rather than
// reporting a false success.
func (s *AddressBookReconcileService) autoPropagate(
	ctx context.Context,
	contactID uuid.UUID,
	external *repository.ExternalContact,
) (ReconcileResult, error) {
	before, err := s.methodDedupKeySet(ctx, contactID)
	if err != nil {
		return ReconcileResult{}, err
	}

	// Expected methods to add: the external row's methods (deduped within
	// the set) that are absent from the contact. EnrichContactFromExternal
	// adds the full external set (not the dismissed-filtered suggestion
	// set), so dismissals are NOT subtracted here.
	expected := make(map[string]bool)
	for _, m := range BuildMethodsFromExternal(external) {
		key := methodDedupKey(m.Type, m.Value)
		if !before[key] {
			expected[key] = true
		}
	}

	if _, err := s.enricher.EnrichContactFromExternal(ctx, contactID, external); err != nil {
		return ReconcileResult{}, fmt.Errorf("auto-propagate methods: %w", err)
	}
	after, err := s.methodDedupKeySet(ctx, contactID)
	if err != nil {
		return ReconcileResult{}, err
	}
	added := 0
	for key := range after {
		if !before[key] {
			added++
		}
	}
	if added < len(expected) {
		return ReconcileResult{MethodsAutoApplied: added}, fmt.Errorf(
			"auto-propagate incomplete: %d of %d expected methods applied (silent insert failure)",
			added, len(expected))
	}
	return ReconcileResult{MethodsAutoApplied: added}, nil
}

// recordSuggestions computes the missing-method set (external methods −
// contact methods − dismissed) for the effective contact and overwrites
// pending_method_suggestions with it. An empty set clears the column to
// SQL NULL, so a method later applied elsewhere drops the stale
// suggestion on the next reconcile.
func (s *AddressBookReconcileService) recordSuggestions(
	ctx context.Context,
	contactID uuid.UUID,
	external *repository.ExternalContact,
) (ReconcileResult, error) {
	missing, err := s.missingMethodSuggestions(ctx, contactID, external)
	if err != nil {
		return ReconcileResult{}, err
	}
	if _, err := s.externalRepo.SetMethodSuggestions(ctx, external.ID, missing); err != nil {
		return ReconcileResult{}, fmt.Errorf("record method suggestions: %w", err)
	}
	recorded := 0
	if len(missing) > 0 {
		recorded = 1
	}
	return ReconcileResult{SuggestionsRecorded: recorded}, nil
}

// missingMethodSuggestions returns the external contact's methods that
// are absent from the CRM contact AND not already dismissed. Stored as
// normalized (type, value) pairs so the suggestion list/resolve surface
// and the dismissed-set subtraction key on the same dedup space.
//
// contactID is the EFFECTIVE contact (for a dup, the canonical's
// contact, resolved by the driver query / forward hook). external is the
// row carrying the methods.
func (s *AddressBookReconcileService) missingMethodSuggestions(
	ctx context.Context,
	contactID uuid.UUID,
	external *repository.ExternalContact,
) ([]repository.PendingMethodSuggestion, error) {
	existing, err := s.methodDedupKeySet(ctx, contactID)
	if err != nil {
		return nil, err
	}

	dismissed := make(map[string]bool, len(external.DismissedMethodSuggestions))
	for _, d := range external.DismissedMethodSuggestions {
		dismissed[methodDedupKey(d.Type, d.Value)] = true
	}

	// Deduplicate within the external set itself (an address book can
	// carry the same value twice) so a suggestion is recorded once.
	seen := make(map[string]bool)
	missing := make([]repository.PendingMethodSuggestion, 0)
	for _, m := range BuildMethodsFromExternal(external) {
		key := methodDedupKey(m.Type, m.Value)
		if existing[key] || dismissed[key] || seen[key] {
			continue
		}
		seen[key] = true
		missing = append(missing, repository.PendingMethodSuggestion{
			Type:  m.Type,
			Value: identity.Normalize(m.Value, mapMethodTypeToIdentifier(m.Type)),
		})
	}
	return missing, nil
}

// methodDedupKeySet returns the set of methodDedupKey values for the
// contact's current methods.
func (s *AddressBookReconcileService) methodDedupKeySet(
	ctx context.Context,
	contactID uuid.UUID,
) (map[string]bool, error) {
	methods, err := s.methodRepo.ListContactMethodsByContact(ctx, contactID)
	if err != nil {
		return nil, fmt.Errorf("list contact methods for reconcile: %w", err)
	}
	set := make(map[string]bool, len(methods))
	for _, m := range methods {
		set[methodDedupKey(m.Type, m.Value)] = true
	}
	return set, nil
}
