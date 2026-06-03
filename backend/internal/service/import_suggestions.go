package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/logger"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Sentinel errors the resolve/dismiss flow returns so the handler can map
// them to the right HTTP status without re-deriving the cause.
var (
	// ErrSuggestionContactGone is returned when the row's effective
	// contact has been soft-deleted between the suggestion being recorded
	// and the user acting on it.
	ErrSuggestionContactGone = errors.New("suggestion: effective contact is gone")
	// ErrSuggestionNotLinked is returned when the row resolves to no live
	// effective contact (unmatched, ignored, or canonical gone). A clean
	// no-op signal, not an internal error.
	ErrSuggestionNotLinked = errors.New("suggestion: row resolves to no live contact")
	// ErrSuggestionInvalidMethod is returned when a requested (type,value)
	// is malformed (empty type or value). Membership in the live set is
	// NEVER an error (idempotent no-op).
	ErrSuggestionInvalidMethod = errors.New("suggestion: malformed method (empty type or value)")
)

// linkOnlySources are sources that may never create a NEW CRM contact.
// They can only link to an existing contact or be ignored. Enforced
// server-side (the import endpoint rejects them); the frontend mirrors
// this policy for presentation. Keep in lock-step with the frontend
// allowedActionsForSource helper.
var linkOnlySources = map[string]bool{
	"gmail_correspondence": true,
}

// AllowedActionsForSource returns the actions a candidate of the given
// source permits. A link-only source omits "import" so it cannot seed a
// new contact. This is the single server-side source of truth for the
// link-only policy.
func AllowedActionsForSource(source string) []string {
	if linkOnlySources[source] {
		return []string{"link", "ignore"}
	}
	return []string{"import", "link", "ignore"}
}

// IsLinkOnlySource reports whether a source is barred from creating a new
// CRM contact. The import endpoint guards on this.
func IsLinkOnlySource(source string) bool {
	return linkOnlySources[source]
}

// CandidateWithMatch pairs an unmatched external_contact with its
// pre-computed suggested match, in the confidence-sorted order the
// candidate surface renders. Both the existing /imports/candidates handler
// and the suggestions surface build their response DTOs from this so there
// is exactly one sort implementation.
type CandidateWithMatch struct {
	External repository.ExternalContact
	Match    *ImportSuggestedMatch
}

// MethodSuggestionItem is one method-suggestion queue entry: the pending
// (type,value) methods for an already-linked contact, ready to confirm or
// dismiss. ContactID is the EFFECTIVE contact (self or, for a duplicate,
// the canonical).
type MethodSuggestionItem struct {
	ExternalID  uuid.UUID
	ContactID   uuid.UUID
	ContactName string
	Source      string
	Methods     []repository.PendingMethodSuggestion
}

// SuggestionListParams filters the unified suggestions list. Source filters
// BOTH the method group and the candidate group; Page/Limit paginate the
// candidate group only (the method group rides above the fold on page 1).
type SuggestionListParams struct {
	Source string
	Page   int
	Limit  int
}

// SuggestionList is the composed read-model: the method-suggestion group
// (page 1 only) plus the page's slice of confidence-ranked candidates, with
// candidate-group pagination meta.
type SuggestionList struct {
	Methods        []MethodSuggestionItem
	Candidates     []CandidateWithMatch
	CandidateTotal int
	Page           int
	Limit          int
	Pages          int
}

// ResolveResult is the outcome of confirming pending methods. Applied is
// the number of methods actually sent to enrichment (already-present /
// unappliable pending entries are pruned but NOT counted).
type ResolveResult struct {
	ContactID    uuid.UUID
	Applied      int
	RematchJobID uuid.UUID
}

// DismissResult is the outcome of a dismiss. Dismissed is the number of
// genuine user dismissals recorded (already-applied / unappliable entries
// are pruned, not counted).
type DismissResult struct {
	Dismissed int
}

// suggestionExternalRepo is the external_contact surface the suggestion
// service needs. Defined as an interface so tests can substitute fakes,
// matching the rest of the service package's composition style.
type suggestionExternalRepo interface {
	GetByID(ctx context.Context, id uuid.UUID) (*repository.ExternalContact, error)
	ListUnmatched(ctx context.Context, source string, limit, offset int32) ([]repository.ExternalContact, error)
	ListAllUnmatched(ctx context.Context, limit, offset int32) ([]repository.ExternalContact, error)
	ListPendingMethodSuggestionRows(ctx context.Context, sourceFilter string) ([]repository.PendingMethodSuggestionRow, error)
	ResolveReconcileTarget(ctx context.Context, id uuid.UUID) (*repository.ReconcileTarget, error)
	GetForUpdateTx(ctx context.Context, tx pgx.Tx, id uuid.UUID) (*repository.ExternalContact, error)
	SetMethodSuggestionSetsTx(ctx context.Context, tx pgx.Tx, id uuid.UUID, pending, dismissed []repository.PendingMethodSuggestion) (*repository.ExternalContact, error)
}

// suggestionContactRepo is the contact-liveness surface.
type suggestionContactRepo interface {
	GetContact(ctx context.Context, id uuid.UUID) (*repository.Contact, error)
}

// suggestionMethodRepo lists a contact's current methods for the drift
// re-check.
type suggestionMethodRepo interface {
	ListContactMethodsByContact(ctx context.Context, contactID uuid.UUID) ([]repository.ContactMethod, error)
}

// suggestionEnricher is the enrichment surface for confirming methods.
type suggestionEnricher interface {
	EnrichContactFromExternalWithSelections(
		ctx context.Context,
		crmContactID uuid.UUID,
		external *repository.ExternalContact,
		selectedMethods []MethodSelection,
		conflictResolutions map[string]string,
		cadenceArg *string,
		name *string,
	) (uuid.UUID, error)
}

// suggestionMatcher finds suggested matches for the candidate group.
type suggestionMatcher interface {
	FindBestMatchesBatch(ctx context.Context, externals []*repository.ExternalContact) ([]*ImportSuggestedMatch, error)
}

// SuggestionService composes the method-suggestion group with the existing
// confidence-ranked candidate list, and runs the resolve/dismiss actions.
// All business logic lives here (the handler only parses/binds and maps
// errors to HTTP). Layering: Handler → SuggestionService →
// EnrichmentService / Repository → DB.
type SuggestionService struct {
	externalRepo suggestionExternalRepo
	contactRepo  suggestionContactRepo
	methodRepo   suggestionMethodRepo
	enricher     suggestionEnricher
	matcher      suggestionMatcher
	database     *db.Database
}

// NewSuggestionService builds the suggestion service. All dependencies are
// required. database backs the FOR-UPDATE tx the resolve/dismiss RMW runs
// in.
func NewSuggestionService(
	externalRepo suggestionExternalRepo,
	contactRepo suggestionContactRepo,
	methodRepo suggestionMethodRepo,
	enricher suggestionEnricher,
	matcher suggestionMatcher,
	database *db.Database,
) *SuggestionService {
	return &SuggestionService{
		externalRepo: externalRepo,
		contactRepo:  contactRepo,
		methodRepo:   methodRepo,
		enricher:     enricher,
		matcher:      matcher,
		database:     database,
	}
}

// BuildSortedCandidates fetches the unmatched candidates for a source (all
// sources when source is empty), attaches the batch-computed suggested
// match, and sorts by confidence descending, then alphabetically by display
// name (empty names last). This is the SINGLE sort implementation shared by
// the existing /imports/candidates handler and the suggestions surface.
func (s *SuggestionService) BuildSortedCandidates(ctx context.Context, source string, maxCandidates int32) ([]CandidateWithMatch, error) {
	var contacts []repository.ExternalContact
	var err error
	if source != "" {
		contacts, err = s.externalRepo.ListUnmatched(ctx, source, maxCandidates, 0)
	} else {
		contacts, err = s.externalRepo.ListAllUnmatched(ctx, maxCandidates, 0)
	}
	if err != nil {
		return nil, fmt.Errorf("list unmatched candidates: %w", err)
	}

	ptrs := make([]*repository.ExternalContact, len(contacts))
	for i := range contacts {
		ptrs[i] = &contacts[i]
	}

	matches, err := s.matcher.FindBestMatchesBatch(ctx, ptrs)
	if err != nil {
		logger.Warn().Err(err).Msg("failed to find suggested matches in batch")
		matches = make([]*ImportSuggestedMatch, len(contacts))
	}

	out := make([]CandidateWithMatch, 0, len(contacts))
	for i := range contacts {
		out = append(out, CandidateWithMatch{External: contacts[i], Match: matches[i]})
	}

	sort.Slice(out, func(i, j int) bool {
		iMatch := out[i].Match
		jMatch := out[j].Match
		if iMatch != nil && jMatch != nil {
			return iMatch.Confidence > jMatch.Confidence
		}
		if iMatch != nil {
			return true
		}
		if jMatch != nil {
			return false
		}
		iName := candidateSortName(&out[i].External)
		jName := candidateSortName(&out[j].External)
		if iName == "" && jName != "" {
			return false
		}
		if iName != "" && jName == "" {
			return true
		}
		return iName < jName
	})
	return out, nil
}

// candidateSortName mirrors the handler's display-name fallback for sorting,
// including the Telegram username fallback (stored leading '@' stripped) so
// handle-only peers sort alphabetically instead of clustering at the end.
func candidateSortName(external *repository.ExternalContact) string {
	if external.DisplayName != nil {
		return *external.DisplayName
	}
	if external.FirstName != nil && external.LastName != nil {
		return *external.FirstName + " " + *external.LastName
	}
	if external.FirstName != nil {
		return *external.FirstName
	}
	if external.LastName != nil {
		return *external.LastName
	}
	if external.Source == "telegram" {
		if u, ok := external.Metadata["username"].(string); ok && u != "" {
			return strings.TrimPrefix(u, "@")
		}
	}
	return ""
}

// ListSuggestions composes the method-suggestion group (page 1 only) with
// the page's slice of confidence-ranked candidates. The source chip filters
// both groups; pagination applies to the candidate group only — the method
// group is small and always returns in full above the fold on page 1.
func (s *SuggestionService) ListSuggestions(ctx context.Context, params SuggestionListParams, maxCandidates int32) (SuggestionList, error) {
	page := params.Page
	if page < 1 {
		page = 1
	}
	limit := params.Limit
	if limit < 1 {
		limit = 20
	}

	// Candidate group: shared sorted build, then paginate.
	candidates, err := s.BuildSortedCandidates(ctx, params.Source, maxCandidates)
	if err != nil {
		return SuggestionList{}, err
	}
	total := len(candidates)
	offset := (page - 1) * limit
	if offset > total {
		offset = total
	}
	end := offset + limit
	if end > total {
		end = total
	}
	pageCandidates := candidates[offset:end]
	pages := total / limit
	if total%limit > 0 {
		pages++
	}

	// Method group rides above the fold on page 1 only.
	var methods []MethodSuggestionItem
	if page == 1 {
		methods, err = s.buildMethodSuggestionItems(ctx, params.Source)
		if err != nil {
			return SuggestionList{}, err
		}
	}

	return SuggestionList{
		Methods:        methods,
		Candidates:     pageCandidates,
		CandidateTotal: total,
		Page:           page,
		Limit:          limit,
		Pages:          pages,
	}, nil
}

// buildMethodSuggestionItems resolves each pending-bearing address-book row
// to its displayable pending set: dropping entries that are dismissed
// (race defense), already on the effective contact (drift defense), or no
// longer present on the external row (unappliable — never show a suggestion
// that cannot be confirmed). Rows whose surviving set is empty are skipped.
// This is read-only; it never prunes the stored pending (resolve/dismiss do
// that under the lock).
func (s *SuggestionService) buildMethodSuggestionItems(ctx context.Context, source string) ([]MethodSuggestionItem, error) {
	rows, err := s.externalRepo.ListPendingMethodSuggestionRows(ctx, source)
	if err != nil {
		return nil, fmt.Errorf("list pending method suggestion rows: %w", err)
	}

	items := make([]MethodSuggestionItem, 0, len(rows))
	for i := range rows {
		row := &rows[i]
		existing, err := s.contactMethodKeySet(ctx, row.EffectiveContactID)
		if err != nil {
			return nil, err
		}
		currentExternalKeys := externalMethodKeySet(&row.External)
		dismissed := suggestionKeySet(row.External.DismissedMethodSuggestions)

		displayable := make([]repository.PendingMethodSuggestion, 0, len(row.External.PendingMethodSuggestions))
		for _, p := range row.External.PendingMethodSuggestions {
			key := methodDedupKey(p.Type, p.Value)
			if dismissed[key] || existing[key] || !currentExternalKeys[key] {
				continue
			}
			displayable = append(displayable, p)
		}
		if len(displayable) == 0 {
			continue
		}
		items = append(items, MethodSuggestionItem{
			ExternalID:  row.External.ID,
			ContactID:   row.EffectiveContactID,
			ContactName: row.ContactFullName,
			Source:      row.External.Source,
			Methods:     displayable,
		})
	}
	return items, nil
}

// ResolveMethodSuggestions confirms the requested pending methods (empty =
// all live pending) for a linked contact: one FOR-UPDATE tx re-reads the
// live state, computes the confirm set as set-ops, clears it from pending,
// and commits; then enrichment runs on its own tx (never nested in the
// lock). Last-write-wins; idempotent (a key no longer in live pending is a
// silent no-op).
func (s *SuggestionService) ResolveMethodSuggestions(
	ctx context.Context,
	externalID uuid.UUID,
	requested []repository.PendingMethodSuggestion,
) (ResolveResult, error) {
	external, effectiveContactID, err := s.resolveActionTarget(ctx, externalID)
	if err != nil {
		return ResolveResult{}, err
	}
	if err := validateRequestedMethods(requested); err != nil {
		return ResolveResult{}, err
	}

	var confirm []repository.PendingMethodSuggestion
	lockedExternal := external
	txErr := pgx.BeginTxFunc(ctx, s.database.Pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		locked, lockErr := s.externalRepo.GetForUpdateTx(ctx, tx, externalID)
		if lockErr != nil {
			return lockErr
		}
		lockedExternal = locked

		existing, methErr := s.contactMethodKeySet(ctx, effectiveContactID)
		if methErr != nil {
			return methErr
		}
		currentExternalKeys := externalMethodKeySet(locked)
		livePending := subtractDismissed(locked.PendingMethodSuggestions, locked.DismissedMethodSuggestions)
		requestedSet := requestedKeySet(requested) // nil → "all"

		confirm = confirm[:0]
		newPending := make([]repository.PendingMethodSuggestion, 0, len(livePending))
		for _, p := range livePending {
			key := methodDedupKey(p.Type, p.Value)
			// Already on the contact, or no longer on the external row:
			// prune from pending, never confirm.
			if existing[key] || !currentExternalKeys[key] {
				continue
			}
			if requestedSet == nil || requestedSet[key] {
				confirm = append(confirm, p)
			} else {
				newPending = append(newPending, p)
			}
		}

		if _, setErr := s.externalRepo.SetMethodSuggestionSetsTx(ctx, tx, externalID, newPending, locked.DismissedMethodSuggestions); setErr != nil {
			return setErr
		}
		return nil
	})
	if txErr != nil {
		return ResolveResult{}, fmt.Errorf("resolve method suggestions tx: %w", txErr)
	}

	if len(confirm) == 0 {
		return ResolveResult{ContactID: effectiveContactID}, nil
	}

	selections := s.selectionsForConfirmed(lockedExternal, confirm)
	if len(selections) == 0 {
		return ResolveResult{ContactID: effectiveContactID}, nil
	}
	jobID, enrichErr := s.enricher.EnrichContactFromExternalWithSelections(
		ctx, effectiveContactID, lockedExternal, selections, nil, nil, nil,
	)
	if enrichErr != nil {
		return ResolveResult{}, fmt.Errorf("enrich confirmed methods: %w", enrichErr)
	}

	return ResolveResult{
		ContactID:    effectiveContactID,
		Applied:      len(selections),
		RematchJobID: jobID,
	}, nil
}

// DismissMethodSuggestions records the requested pending methods (empty =
// all actionable live pending) as sticky dismissals and drops them from
// pending, in one FOR-UPDATE tx. Already-applied / unappliable entries are
// pruned from pending but NOT pushed into dismissed (so a later legitimate
// re-add is not suppressed). Idempotent.
func (s *SuggestionService) DismissMethodSuggestions(
	ctx context.Context,
	externalID uuid.UUID,
	requested []repository.PendingMethodSuggestion,
) (DismissResult, error) {
	_, effectiveContactID, err := s.resolveActionTarget(ctx, externalID)
	if err != nil {
		return DismissResult{}, err
	}
	if err := validateRequestedMethods(requested); err != nil {
		return DismissResult{}, err
	}

	var dismissedCount int
	txErr := pgx.BeginTxFunc(ctx, s.database.Pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		locked, lockErr := s.externalRepo.GetForUpdateTx(ctx, tx, externalID)
		if lockErr != nil {
			return lockErr
		}

		existing, methErr := s.contactMethodKeySet(ctx, effectiveContactID)
		if methErr != nil {
			return methErr
		}
		currentExternalKeys := externalMethodKeySet(locked)
		livePending := subtractDismissed(locked.PendingMethodSuggestions, locked.DismissedMethodSuggestions)
		requestedSet := requestedKeySet(requested) // nil → "all actionable"

		var dismissSet []repository.PendingMethodSuggestion
		newPending := make([]repository.PendingMethodSuggestion, 0, len(livePending))
		for _, p := range livePending {
			key := methodDedupKey(p.Type, p.Value)
			// Already-applied or unappliable: prune from pending, never
			// dismiss (so a legitimate re-add can re-suggest later).
			if existing[key] || !currentExternalKeys[key] {
				continue
			}
			if requestedSet == nil || requestedSet[key] {
				dismissSet = append(dismissSet, p)
			} else {
				newPending = append(newPending, p)
			}
		}

		newDismissed := unionSuggestions(locked.DismissedMethodSuggestions, dismissSet)
		if _, setErr := s.externalRepo.SetMethodSuggestionSetsTx(ctx, tx, externalID, newPending, newDismissed); setErr != nil {
			return setErr
		}
		dismissedCount = len(dismissSet)
		return nil
	})
	if txErr != nil {
		return DismissResult{}, fmt.Errorf("dismiss method suggestions tx: %w", txErr)
	}
	return DismissResult{Dismissed: dismissedCount}, nil
}

// resolveActionTarget fetches the external row, resolves its effective
// contact via the duplicate-aware reconcile resolver, and liveness-checks
// that contact. Shared by resolve and dismiss.
func (s *SuggestionService) resolveActionTarget(ctx context.Context, externalID uuid.UUID) (*repository.ExternalContact, uuid.UUID, error) {
	external, err := s.externalRepo.GetByID(ctx, externalID)
	if err != nil {
		return nil, uuid.Nil, fmt.Errorf("get external contact: %w", err)
	}
	if external == nil {
		return nil, uuid.Nil, db.ErrNotFound
	}

	target, err := s.externalRepo.ResolveReconcileTarget(ctx, externalID)
	if err != nil {
		return nil, uuid.Nil, fmt.Errorf("resolve reconcile target: %w", err)
	}
	if target == nil {
		return nil, uuid.Nil, ErrSuggestionNotLinked
	}

	if _, err := s.contactRepo.GetContact(ctx, target.EffectiveContactID); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return nil, uuid.Nil, ErrSuggestionContactGone
		}
		return nil, uuid.Nil, fmt.Errorf("verify contact liveness: %w", err)
	}
	return external, target.EffectiveContactID, nil
}

// selectionsForConfirmed maps each confirmed normalized (type,value) back to
// the external row's ORIGINAL value via BuildMethodsFromExternal, so the
// enrichment selection carries the value the enrich path validates against.
// A confirmed key with no matching original is skipped (it was pruned as
// unappliable under the lock, but this is belt-and-suspenders).
func (s *SuggestionService) selectionsForConfirmed(external *repository.ExternalContact, confirmed []repository.PendingMethodSuggestion) []MethodSelection {
	originalByKey := make(map[string]string)
	for _, m := range BuildMethodsFromExternal(external) {
		originalByKey[methodDedupKey(m.Type, m.Value)] = m.Value
	}
	selections := make([]MethodSelection, 0, len(confirmed))
	for _, c := range confirmed {
		key := methodDedupKey(c.Type, c.Value)
		original, ok := originalByKey[key]
		if !ok {
			continue
		}
		selections = append(selections, MethodSelection{
			OriginalValue: original,
			Type:          c.Type,
		})
	}
	return selections
}

// contactMethodKeySet returns the methodDedupKey set of the contact's
// current methods (drift re-check).
func (s *SuggestionService) contactMethodKeySet(ctx context.Context, contactID uuid.UUID) (map[string]bool, error) {
	methods, err := s.methodRepo.ListContactMethodsByContact(ctx, contactID)
	if err != nil {
		return nil, fmt.Errorf("list contact methods: %w", err)
	}
	set := make(map[string]bool, len(methods))
	for _, m := range methods {
		set[methodDedupKey(m.Type, m.Value)] = true
	}
	return set, nil
}

// externalMethodKeySet returns the methodDedupKey set of the methods
// currently emitted by the external row (the appliable set).
func externalMethodKeySet(external *repository.ExternalContact) map[string]bool {
	set := make(map[string]bool)
	for _, m := range BuildMethodsFromExternal(external) {
		set[methodDedupKey(m.Type, m.Value)] = true
	}
	return set
}

// suggestionKeySet returns the methodDedupKey set of a suggestion slice.
func suggestionKeySet(suggestions []repository.PendingMethodSuggestion) map[string]bool {
	set := make(map[string]bool, len(suggestions))
	for _, s := range suggestions {
		set[methodDedupKey(s.Type, s.Value)] = true
	}
	return set
}

// requestedKeySet returns the methodDedupKey set of the user's requested
// (type,value) list, or nil when the request is empty (meaning "all").
func requestedKeySet(requested []repository.PendingMethodSuggestion) map[string]bool {
	if len(requested) == 0 {
		return nil
	}
	return suggestionKeySet(requested)
}

// subtractDismissed returns the pending entries not present in the
// dismissed set (defense-in-depth read of the live pending set).
func subtractDismissed(pending, dismissed []repository.PendingMethodSuggestion) []repository.PendingMethodSuggestion {
	dismissedKeys := suggestionKeySet(dismissed)
	out := make([]repository.PendingMethodSuggestion, 0, len(pending))
	for _, p := range pending {
		if dismissedKeys[methodDedupKey(p.Type, p.Value)] {
			continue
		}
		out = append(out, p)
	}
	return out
}

// unionSuggestions appends additions to base, deduping by methodDedupKey
// (append-only/sticky). Order preserves base then new.
func unionSuggestions(base, additions []repository.PendingMethodSuggestion) []repository.PendingMethodSuggestion {
	seen := suggestionKeySet(base)
	out := make([]repository.PendingMethodSuggestion, len(base), len(base)+len(additions))
	copy(out, base)
	for _, a := range additions {
		key := methodDedupKey(a.Type, a.Value)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, a)
	}
	return out
}

// validateRequestedMethods rejects malformed entries (empty type/value).
// Membership in any set is never an error (idempotency rule). The value is
// normalized to the dedup space before set math, so callers may send either
// the normalized or a canonicalizable value.
func validateRequestedMethods(requested []repository.PendingMethodSuggestion) error {
	for _, m := range requested {
		if strings.TrimSpace(m.Type) == "" || strings.TrimSpace(m.Value) == "" {
			return ErrSuggestionInvalidMethod
		}
	}
	return nil
}
