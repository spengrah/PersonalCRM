package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Operation verbs accepted by the contact-method operations endpoint.
//
// The payload is a list of OPERATIONS, never a desired set. That is the whole
// point: a desired set makes absence mean "delete", so a client working from a
// stale read destroys every method it did not know about. In an operations
// payload absence expresses nothing, so removal requires naming what to remove.
const (
	MethodOpAdd          = "add"
	MethodOpUpdate       = "update"
	MethodOpRemove       = "remove"
	MethodOpSetPrimary   = "set_primary"
	MethodOpClearPrimary = "clear_primary"
)

// Per-operation outcomes reported back to the client.
const (
	MethodOutcomeCreated         = "created"
	MethodOutcomeMatchedExisting = "matched_existing"
	MethodOutcomeUpdated         = "updated"
	MethodOutcomeRemoved         = "removed"
	MethodOutcomeNoOp            = "no_op"
)

// ErrInvalidOperations reports a payload the endpoint refuses to apply: a
// malformed operation, or operations that conflict with each other, or a final
// state that would violate an invariant. Always a 400, and always rejected
// whole — no partial application.
var ErrInvalidOperations = errors.New("invalid contact method operations")

// ErrMethodNotOwned reports an operation naming a method id that exists but
// belongs to a different contact. A 404: a method id is not a capability, and
// that guarantee is not relaxed for remove, which is precisely where it guards
// a destructive operation.
var ErrMethodNotOwned = errors.New("contact method belongs to another contact")

// ErrContactNotFound reports that the contact the request addresses does not
// exist. A 404.
//
// Service-owned on purpose. The handler must not branch on db.ErrNotFound: that
// is a persistence-layer classification, and reaching across the service
// boundary for it is a layer skip. The same reasoning applies to
// repository.ErrMethodValueConflict, which classifyApplyError folds into
// ErrInvalidOperations below.
var ErrContactNotFound = errors.New("contact not found")

// classifyApplyError translates repository-owned errors into service-owned ones
// so nothing above this layer needs to know repository or database error
// values.
//
// The value conflict becomes ErrInvalidOperations because that is exactly what
// it is: a payload whose resulting method set would contain a duplicate. The
// caller therefore cannot tell whether the fold rejected the request or the
// database's unique index did — which is the intended contract, since a correct
// C6 mirror makes the two agree by construction.
func classifyApplyError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, repository.ErrMethodValueConflict) {
		return fmt.Errorf("%w: %v", ErrInvalidOperations, err)
	}
	return err
}

// ContactMethodOperation is one requested mutation.
//
// IsPrimary is a pointer so the validator can distinguish "absent" from
// "present and false". The field is forbidden on update, which is a presence
// test — a plain bool would silently accept `"is_primary": false`.
type ContactMethodOperation struct {
	Op        string
	MethodID  *uuid.UUID
	Type      string
	Value     string
	IsPrimary *bool
}

// ContactMethodOperationResult reports what happened to one SUBMITTED
// operation, at that operation's own request index.
//
// Method carries the resolved row's post-apply state, and is nil exactly for
// removals: a removed row has no post-apply state, and an idempotent removal of
// an already-absent id has no row at all. MethodID is always the id the
// operation ADDRESSED — for a removal, the id the client submitted, whether or
// not a row existed.
//
// This exists so the client never has to re-derive server identity. A client
// that infers which row an add resolved to by re-normalizing the returned list
// with its own normalizer is wrong by construction: the client's normalizer and
// the database trigger are different functions, so the inference fails exactly
// when it matters.
type ContactMethodOperationResult struct {
	Index    int
	Outcome  string
	MethodID uuid.UUID
	Method   *repository.ContactMethod
}

// ApplyContactMethodsResult is the endpoint's full response payload.
type ApplyContactMethodsResult struct {
	Methods      []repository.ContactMethod
	RematchJobID uuid.UUID
	Results      []ContactMethodOperationResult
}

// ContactMethodService applies contact-method operations transactionally.
type ContactMethodService struct {
	database        *db.Database
	bus             *events.Bus
	rematchRegistry RematchRegistry
}

func NewContactMethodService(database *db.Database, bus *events.Bus, rematchRegistry RematchRegistry) *ContactMethodService {
	return &ContactMethodService{
		database:        database,
		bus:             bus,
		rematchRegistry: rematchRegistry,
	}
}

// foldedMethod is a method row in the intended final state. Rows carried over
// from pre-state remember what they looked like before, so the apply stage can
// tell a key change (delete-and-reinsert) from a stored-value-only change
// (in-place update) from no change at all.
type foldedMethod struct {
	ID        uuid.UUID
	Type      string
	Value     string
	IsPrimary bool
	CreatedAt time.Time

	fromPreState bool
	preType      string
	preValue     string
	prePrimary   bool
}

func (f *foldedMethod) key() string {
	return f.Type + "|" + repository.NormalizeContactMethodValueForUniqueness(f.Type, f.Value)
}

func (f *foldedMethod) preKey() string {
	return f.preType + "|" + repository.NormalizeContactMethodValueForUniqueness(f.preType, f.preValue)
}

// resolution records, per SUBMITTED operation index, which row the operation
// addressed and what happened to it. Results are built from this, so coalesced
// operations each keep their own index while sharing a resolved id.
type resolution struct {
	methodID    uuid.UUID
	outcome     string
	hasSnapshot bool
}

// ApplyOperations applies a batch of contact-method operations in one
// transaction: fold, validate, publish, apply, commit.
//
// The sequencing is not incidental. Folding in memory makes the outcome
// independent of payload order by construction rather than by hoping the
// database tolerates a particular statement sequence. Validating the intended
// FINAL STATE means conflicts are rejected deterministically instead of being
// discovered by whichever row PostgreSQL happened to reach first. Publishing
// before mutating satisfies the PublishTx -> mutate -> commit rule, so a
// publish failure cannot strand a committed mutation.
func (s *ContactMethodService) ApplyOperations(
	ctx context.Context,
	contactID uuid.UUID,
	ops []ContactMethodOperation,
) (result *ApplyContactMethodsResult, err error) {
	if err := validateOperationShapes(ops); err != nil {
		return nil, err
	}

	tx, err := s.database.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() {
		if rollbackErr := tx.Rollback(ctx); rollbackErr != nil && !errors.Is(rollbackErr, pgx.ErrTxClosed) {
			if err == nil {
				err = rollbackErr
			}
		}
	}()

	txQueries := db.New(tx)
	contactRepo := repository.NewContactRepository(txQueries)
	methodRepo := repository.NewContactMethodRepository(txQueries)

	if _, err := contactRepo.GetContact(ctx, contactID); err != nil {
		// Translated here, not at the handler: db.ErrNotFound is a persistence
		// classification and must not cross the service boundary.
		if errors.Is(err, db.ErrNotFound) {
			return nil, ErrContactNotFound
		}
		return nil, err
	}

	preState, err := methodRepo.ListContactMethodsByContact(ctx, contactID)
	if err != nil {
		return nil, err
	}

	// Resolve every id-bearing operation against this contact's pre-state,
	// falling back to a global ownership lookup. This is what separates "owned
	// by another contact" (rejected outright) from "does not exist at all"
	// (a removal succeeds as a no-op, so a retried removal is idempotent).
	// Those two are indistinguishable from pre-state alone.
	absentIDs, err := s.resolveOwnership(ctx, methodRepo, contactID, preState, ops)
	if err != nil {
		return nil, err
	}

	if err := validateOperationInteractions(ops); err != nil {
		return nil, err
	}

	finalState, resolutions, err := foldOperations(preState, ops, absentIDs)
	if err != nil {
		return nil, err
	}

	if err := validateFinalState(finalState); err != nil {
		return nil, err
	}

	// Publish BEFORE mutating. The diff is semantic — values newly present in
	// the final state — rather than "rows we physically INSERTed", which
	// preserves today's UpdateContact behavior exactly and makes idempotency
	// fall out for free: a replayed payload folds to a final state equal to
	// pre-state, so the diff is empty and nothing is published.
	var (
		jobID           uuid.UUID
		eligibleMethods []Method
	)
	if s.rematchRegistry != nil {
		newlyAdded := diffNewMethodsFromFold(preState, finalState)
		eligibleMethods = s.rematchRegistry.EligibleMethods(newlyAdded)
	}
	if len(eligibleMethods) > 0 && s.bus != nil {
		jobID = uuid.New()
		refs := rematchMethodsToRefs(eligibleMethods)
		env, marshalErr := buildContactMethodsAddedEnvelope("manual", contactID, refs, jobID)
		if marshalErr != nil {
			return nil, marshalErr
		}
		if err := s.bus.PublishTx(ctx, tx, env); err != nil {
			return nil, fmt.Errorf("publish contact_methods.added: %w", err)
		}
	}

	if err := applyFinalState(ctx, methodRepo, contactID, preState, finalState); err != nil {
		return nil, classifyApplyError(err)
	}

	// Read the committed-to-be rows back so every result snapshot carries the
	// row's true post-apply state, including the trigger-owned
	// value_normalized. Reconstructing snapshots from the fold would report
	// what Go believes rather than what the database stored.
	postState, err := methodRepo.ListContactMethodsByContact(ctx, contactID)
	if err != nil {
		return nil, err
	}

	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}

	if jobID != uuid.Nil && s.rematchRegistry != nil {
		s.rematchRegistry.RegisterPending(jobID, contactID, eligibleMethods)
	}

	return &ApplyContactMethodsResult{
		Methods:      postState,
		RematchJobID: jobID,
		Results:      buildOperationResults(resolutions, postState),
	}, nil
}

// validateOperationShapes checks each operation against its own required and
// forbidden fields.
//
// Deliberately NOT the shared normalizeContactMethodRequests/
// validateContactMethods helpers used by the create path: those silently DISCARD
// blank entries, which would turn update(id, value:"") into a successful-looking
// no-op. That is a silent-success failure — the same class this endpoint exists
// to eliminate, merely inverted. An unsatisfiable intent must be rejected, not
// quietly dropped.
//
// Irrelevant fields are rejected rather than ignored so a malformed client
// learns immediately instead of believing an ignored field took effect.
func validateOperationShapes(ops []ContactMethodOperation) error {
	for i, op := range ops {
		switch op.Op {
		case MethodOpAdd:
			if op.MethodID != nil {
				return opErrf(i, "add must not carry method_id")
			}
			if err := requireTypeAndValue(i, op); err != nil {
				return err
			}

		case MethodOpUpdate:
			if op.MethodID == nil {
				return opErrf(i, "update requires method_id")
			}
			// Presence test, not a truth test. A primary change requires an
			// explicit set_primary/clear_primary, so accepting-or-ignoring
			// is_primary here would leave its meaning undefined.
			if op.IsPrimary != nil {
				return opErrf(i, "update must not carry is_primary; use set_primary or clear_primary")
			}
			if err := requireTypeAndValue(i, op); err != nil {
				return err
			}

		case MethodOpRemove, MethodOpSetPrimary, MethodOpClearPrimary:
			if op.MethodID == nil {
				return opErrf(i, "%s requires method_id", op.Op)
			}
			if op.Type != "" {
				return opErrf(i, "%s must not carry type", op.Op)
			}
			if op.Value != "" {
				return opErrf(i, "%s must not carry value", op.Op)
			}
			if op.IsPrimary != nil {
				return opErrf(i, "%s must not carry is_primary", op.Op)
			}

		default:
			return opErrf(i, "unknown op %q", op.Op)
		}
	}
	return nil
}

// requireTypeAndValue enforces the create path's type and value format rules on
// add/update. Only the blank handling deliberately differs: blank is rejected
// here rather than dropped.
func requireTypeAndValue(i int, op ContactMethodOperation) error {
	if op.Value == "" {
		return opErrf(i, "%s requires a non-blank value", op.Op)
	}
	if !isKnownContactMethodType(op.Type) {
		return opErrf(i, "%s has unknown type %q", op.Op, op.Type)
	}
	// A value that is non-empty but normalizes to empty — "@@@" for a handle,
	// whitespace, a phone with no digits — is just as unsatisfiable as a blank
	// one. Accepting it stores a row with an empty value_normalized, which the
	// unique index caps at one per contact+type and which identity and sync
	// queries then filter out: a row that exists, cannot be matched on, and
	// blocks the slot. Checked through the C6 mirror so the emptiness test is
	// the trigger's, not a second opinion.
	if repository.NormalizeContactMethodValueForUniqueness(op.Type, op.Value) == "" {
		return opErrf(i, "%s value %q is empty once normalized", op.Op, op.Value)
	}
	// Value FORMAT rules (email shape, phone length) are enforced at the
	// handler, which owns the same validator the create path uses. What is
	// enforced here is the domain rule the create path deliberately does NOT
	// share: a blank value is rejected rather than dropped.
	return nil
}

func isKnownContactMethodType(methodType string) bool {
	for _, t := range repository.ContactMethodTypes {
		if string(t) == methodType {
			return true
		}
	}
	return false
}

func opErrf(index int, format string, args ...any) error {
	return fmt.Errorf("%w: operation %d: %s", ErrInvalidOperations, index, fmt.Sprintf(format, args...))
}

// resolveOwnership resolves every id-bearing operation and returns the set of
// ids that do not exist anywhere.
func (s *ContactMethodService) resolveOwnership(
	ctx context.Context,
	methodRepo *repository.ContactMethodRepository,
	contactID uuid.UUID,
	preState []repository.ContactMethod,
	ops []ContactMethodOperation,
) (map[uuid.UUID]bool, error) {
	owned := make(map[uuid.UUID]bool, len(preState))
	for _, m := range preState {
		owned[m.ID] = true
	}

	absent := make(map[uuid.UUID]bool)
	for i, op := range ops {
		if op.MethodID == nil || owned[*op.MethodID] || absent[*op.MethodID] {
			continue
		}
		owner, err := methodRepo.LookupContactMethodOwner(ctx, *op.MethodID)
		switch {
		case err == nil && owner != contactID:
			// Exists, owned by someone else. Rejected for EVERY verb
			// including remove: relaxing it there would relax it precisely
			// where it protects a destructive operation.
			return nil, fmt.Errorf("%w: operation %d names method %s", ErrMethodNotOwned, i, op.MethodID)
		case errors.Is(err, db.ErrNotFound):
			// Does not exist at all. A removal succeeds as a no-op, which is
			// what makes a retried removal idempotent; every other verb has an
			// intent that cannot be satisfied.
			if op.Op != MethodOpRemove {
				return nil, opErrf(i, "%s names method %s, which does not exist", op.Op, op.MethodID)
			}
			absent[*op.MethodID] = true
		case err != nil:
			return nil, err
		}
	}
	return absent, nil
}

// validateOperationInteractions rejects payloads whose operations conflict with
// each other.
//
// A pure fold does not make non-commutative operations commutative — class
// ordering removes only INTER-class dependence. Order-independence comes from
// rejecting non-commutative payloads here, not from asserting the fold absorbs
// them.
func validateOperationInteractions(ops []ContactMethodOperation) error {
	var (
		removed        = map[uuid.UUID]bool{}
		mutatedOnce    = map[uuid.UUID]int{}
		primaryOpIndex = -1
		primaryOpVerb  string
	)

	for i, op := range ops {
		switch op.Op {
		case MethodOpRemove:
			// Two IDENTICAL removes coalesce rather than conflict: both express
			// the same satisfiable intent.
			if idx, seen := mutatedOnce[*op.MethodID]; seen && ops[idx].Op != MethodOpRemove {
				return opErrf(i, "method %s is named by both remove and %s", op.MethodID, ops[idx].Op)
			}
			removed[*op.MethodID] = true
			mutatedOnce[*op.MethodID] = i

		case MethodOpUpdate:
			if idx, seen := mutatedOnce[*op.MethodID]; seen {
				return opErrf(i, "method %s is named by both update and %s", op.MethodID, ops[idx].Op)
			}
			mutatedOnce[*op.MethodID] = i

		case MethodOpSetPrimary, MethodOpClearPrimary:
			if primaryOpIndex >= 0 {
				if primaryOpVerb != op.Op {
					return opErrf(i, "set_primary and clear_primary cannot appear in the same request")
				}
				return opErrf(i, "only one primary designation is allowed per request")
			}
			primaryOpIndex = i
			primaryOpVerb = op.Op
		}
	}

	// A primary designation on a removed row is self-conflicting in both
	// directions: promoting a row that will not exist is unsatisfiable, and
	// clearing one is redundant because removal already leaves no primary.
	for i, op := range ops {
		switch op.Op {
		case MethodOpSetPrimary, MethodOpClearPrimary, MethodOpUpdate:
			if removed[*op.MethodID] {
				return opErrf(i, "method %s is removed in the same request", op.MethodID)
			}
		}
	}

	// Two adds resolving to the same key are a merge only when they agree in
	// every field. Differing stored casing is a genuine conflict — the request
	// asks for two different stored values in one row.
	seenAdds := map[string]int{}
	for i, op := range ops {
		if op.Op != MethodOpAdd {
			continue
		}
		key := op.Type + "|" + repository.NormalizeContactMethodValueForUniqueness(op.Type, op.Value)
		if prev, seen := seenAdds[key]; seen {
			if ops[prev].Value != op.Value || !samePrimaryIntent(ops[prev], op) {
				return opErrf(i, "conflicts with operation %d: same normalized value, different stored value or primary intent", prev)
			}
			continue
		}
		seenAdds[key] = i
	}

	// An add carrying is_primary IS a primary designation. A row that does not
	// exist yet has no id for set_primary to name, so a new row's designation
	// necessarily travels on its add — which means counting only the explicit
	// verbs above misses it.
	//
	// Left uncounted, two such adds both reached the fold, where the last one in
	// payload order won. That is an outcome depending on payload order, which
	// CON-062 forbids outright, and it also produced a misleading result: the
	// losing add reported success with a non-primary snapshot despite having
	// asserted primary intent.
	//
	// Adds resolving to the same key are already proven identical above, so they
	// coalesce to a single designation rather than conflicting with themselves.
	primaryAddKeys := map[string]bool{}
	for i, op := range ops {
		if op.Op != MethodOpAdd || !primaryIntent(op) {
			continue
		}
		key := op.Type + "|" + repository.NormalizeContactMethodValueForUniqueness(op.Type, op.Value)
		if primaryAddKeys[key] {
			continue
		}
		if primaryOpIndex >= 0 {
			return opErrf(i, "add designates a primary alongside %s at operation %d", primaryOpVerb, primaryOpIndex)
		}
		primaryAddKeys[key] = true
		if len(primaryAddKeys) > 1 {
			return opErrf(i, "only one primary designation is allowed per request")
		}
	}

	return nil
}

func samePrimaryIntent(a, b ContactMethodOperation) bool {
	return primaryIntent(a) == primaryIntent(b)
}

func primaryIntent(op ContactMethodOperation) bool {
	return op.IsPrimary != nil && *op.IsPrimary
}

// foldOperations applies the operations to a copy of pre-state by operation
// class in fixed order — remove, update, add, primary designation — producing
// the intended final state, plus the per-submitted-index resolutions the
// results array is built from.
func foldOperations(
	preState []repository.ContactMethod,
	ops []ContactMethodOperation,
	absentIDs map[uuid.UUID]bool,
) ([]*foldedMethod, []resolution, error) {
	rows := make([]*foldedMethod, 0, len(preState)+len(ops))
	byID := make(map[uuid.UUID]*foldedMethod, len(preState))
	for _, m := range preState {
		f := &foldedMethod{
			ID:           m.ID,
			Type:         m.Type,
			Value:        m.Value,
			IsPrimary:    m.IsPrimary,
			CreatedAt:    m.CreatedAt,
			fromPreState: true,
			preType:      m.Type,
			preValue:     m.Value,
			prePrimary:   m.IsPrimary,
		}
		rows = append(rows, f)
		byID[m.ID] = f
	}

	resolutions := make([]resolution, len(ops))
	dropped := map[uuid.UUID]bool{}

	// Class 1: remove.
	for i, op := range ops {
		if op.Op != MethodOpRemove {
			continue
		}
		id := *op.MethodID
		resolutions[i] = resolution{methodID: id, outcome: MethodOutcomeRemoved}
		if absentIDs[id] {
			// Replayed removal: the id is already gone. Successful, and the
			// outcome is the whole information — there is no row to snapshot.
			resolutions[i].outcome = MethodOutcomeNoOp
			continue
		}
		dropped[id] = true
	}

	// Class 2: update. Preserves row identity and created_at, which is the
	// entire reason update exists as a distinct verb from remove-then-add.
	for i, op := range ops {
		if op.Op != MethodOpUpdate {
			continue
		}
		f := byID[*op.MethodID]
		f.Type = op.Type
		f.Value = op.Value
		resolutions[i] = resolution{methodID: f.ID, outcome: MethodOutcomeUpdated, hasSnapshot: true}
	}

	// Class 3: add. An add naming a value that already exists resolves to the
	// existing row rather than creating a second one.
	byKey := map[string]*foldedMethod{}
	for _, f := range rows {
		if dropped[f.ID] {
			continue
		}
		byKey[f.key()] = f
	}
	for i, op := range ops {
		if op.Op != MethodOpAdd {
			continue
		}
		key := op.Type + "|" + repository.NormalizeContactMethodValueForUniqueness(op.Type, op.Value)
		if existing, ok := byKey[key]; ok {
			// Coalesced with an earlier add in this payload, or matched against
			// a row that was already there. Either way the client learns the id
			// its assertion resolved to.
			outcome := MethodOutcomeMatchedExisting
			if !existing.fromPreState {
				outcome = MethodOutcomeCreated
			}
			resolutions[i] = resolution{methodID: existing.ID, outcome: outcome, hasSnapshot: true}
			continue
		}
		f := &foldedMethod{
			ID:        uuid.New(),
			Type:      op.Type,
			Value:     op.Value,
			CreatedAt: accelerated.GetCurrentTime(),
		}
		rows = append(rows, f)
		byID[f.ID] = f
		byKey[key] = f
		resolutions[i] = resolution{methodID: f.ID, outcome: MethodOutcomeCreated, hasSnapshot: true}
	}

	// Class 4: primary designation. Applied last so an add carrying
	// is_primary can designate a row created earlier in this same payload.
	final := make([]*foldedMethod, 0, len(rows))
	for _, f := range rows {
		if !dropped[f.ID] {
			final = append(final, f)
		}
	}

	var designated *foldedMethod
	var clearRequested bool
	for i, op := range ops {
		switch op.Op {
		case MethodOpSetPrimary:
			f := byID[*op.MethodID]
			designated = f
			resolutions[i] = resolution{methodID: f.ID, outcome: MethodOutcomeUpdated, hasSnapshot: true}
			if f.prePrimary {
				resolutions[i].outcome = MethodOutcomeNoOp
			}
		case MethodOpClearPrimary:
			f := byID[*op.MethodID]
			resolutions[i] = resolution{methodID: f.ID, outcome: MethodOutcomeNoOp, hasSnapshot: true}
			// Clearing a row that is not primary is a successful no-op — it
			// must NOT demote whichever row happens to be primary.
			if f.prePrimary {
				clearRequested = true
				resolutions[i].outcome = MethodOutcomeUpdated
			}
		case MethodOpAdd:
			if primaryIntent(op) {
				designated = byID[resolutions[i].methodID]
			}
		}
	}

	for _, f := range final {
		f.IsPrimary = false
	}
	switch {
	case designated != nil && !dropped[designated.ID]:
		designated.IsPrimary = true
	case clearRequested:
		// Explicitly left with no primary.
	default:
		// Preserve whatever was primary before, unless it is gone now.
		for _, f := range final {
			if f.prePrimary && !dropped[f.ID] {
				f.IsPrimary = true
				break
			}
		}
	}

	return final, resolutions, nil
}

// validateFinalState rejects an intended end state that violates an invariant,
// before any mutation is issued.
func validateFinalState(final []*foldedMethod) error {
	seen := map[string]bool{}
	primaries := 0
	for _, f := range final {
		k := f.key()
		if seen[k] {
			return fmt.Errorf("%w: resulting methods would contain a duplicate %s value %q",
				ErrInvalidOperations, f.Type, f.Value)
		}
		seen[k] = true
		if f.IsPrimary {
			primaries++
		}
	}
	if primaries > 1 {
		return fmt.Errorf("%w: resulting methods would contain more than one primary", ErrInvalidOperations)
	}
	return nil
}

// diffNewMethodsFromFold computes the semantic diff between pre-state and the
// intended final state — values newly PRESENT, regardless of which physical
// rows moved. This mirrors UpdateContact's existing behavior exactly.
//
// The fold's rows carry the mirror's normalized key rather than the trigger's
// output, which is sound precisely because the mirror reproduces the trigger
// (pinned by the parity test).
func diffNewMethodsFromFold(before []repository.ContactMethod, after []*foldedMethod) []Method {
	existing := make(map[string]struct{}, len(before))
	for _, m := range before {
		existing[m.Type+"|"+m.ValueNormalized] = struct{}{}
	}
	out := make([]Method, 0, len(after))
	for _, f := range after {
		normalized := repository.NormalizeContactMethodValueForUniqueness(f.Type, f.Value)
		if _, ok := existing[f.Type+"|"+normalized]; ok {
			continue
		}
		out = append(out, Method{Type: f.Type, Value: normalized})
	}
	return out
}

// applyFinalState carries pre-state to the intended final state.
//
// Delete-and-reinsert rather than in-place rewriting: idx_contact_method_unique_value
// is enforced per statement, so updating rows in place can transiently collide
// even when the final state is valid. Since no foreign key references
// contact_method, a row can be deleted and reinserted with its identity intact,
// which reaches every valid final state — including full value swaps and
// type-only swaps — with no collision analysis at all.
//
// Inserts are always non-primary and promotion is always last, so an add
// carrying is_primary can never violate idx_contact_method_primary mid-apply.
func applyFinalState(
	ctx context.Context,
	methodRepo *repository.ContactMethodRepository,
	contactID uuid.UUID,
	preState []repository.ContactMethod,
	final []*foldedMethod,
) error {
	survives := make(map[uuid.UUID]*foldedMethod, len(final))
	for _, f := range final {
		survives[f.ID] = f
	}

	reinserted := map[uuid.UUID]bool{}

	// Step 1: delete removed rows AND rows whose (type, value_normalized) key
	// changes. The KEY, not merely the value — a type-only swap changes the key
	// with no value change at all.
	for _, m := range preState {
		f, kept := survives[m.ID]
		if !kept {
			if err := methodRepo.DeleteContactMethodByContact(ctx, contactID, m.ID); err != nil {
				return err
			}
			continue
		}
		if f.key() != f.preKey() {
			if err := methodRepo.DeleteContactMethodByContact(ctx, contactID, m.ID); err != nil {
				return err
			}
			reinserted[m.ID] = true
		}
	}

	// Step 2: reinsert key-changing rows with their ORIGINAL id and created_at.
	// Step 3: insert genuinely new rows.
	for _, f := range final {
		needsInsert := reinserted[f.ID] || !f.fromPreState
		if !needsInsert {
			continue
		}
		if _, err := methodRepo.InsertContactMethodWithIdentity(ctx, repository.InsertContactMethodWithIdentityRequest{
			ID:        f.ID,
			ContactID: contactID,
			Type:      f.Type,
			Value:     f.Value,
			IsPrimary: false,
			CreatedAt: f.CreatedAt,
		}); err != nil {
			return err
		}
	}

	// Step 4: in-place update for rows whose stored value changed but whose key
	// did not. Without this phase the request returns 200 having silently
	// discarded the edit — a case-only correction or a phone respelling never
	// reaches steps 1-3 because its normalized key never moved.
	for _, f := range final {
		if !f.fromPreState || reinserted[f.ID] {
			continue
		}
		if f.Value == f.preValue && f.Type == f.preType {
			continue
		}
		if _, err := methodRepo.UpdateContactMethodByContact(ctx, contactID, f.ID, f.Type, f.Value); err != nil {
			return err
		}
	}

	// Steps 5-6 read DATABASE state, not payload deltas. If the current
	// primary's own key changed it was reinserted non-primary above, while its
	// DESIGNATION did not change — a delta-based rule would fire neither demote
	// nor promote and leave the contact with no primary at all.
	var prePrimaryID uuid.UUID
	for _, m := range preState {
		if m.IsPrimary {
			prePrimaryID = m.ID
			break
		}
	}
	var finalPrimary *foldedMethod
	for _, f := range final {
		if f.IsPrimary {
			finalPrimary = f
			break
		}
	}

	// Demote only if the pre-state primary still carries the flag in the
	// database — a reinserted row already came back non-primary, and a deleted
	// one is gone.
	if prePrimaryID != uuid.Nil && !reinserted[prePrimaryID] && survives[prePrimaryID] != nil {
		if finalPrimary == nil || finalPrimary.ID != prePrimaryID {
			if err := methodRepo.DemoteContactMethodPrimaryByContact(ctx, contactID, prePrimaryID); err != nil {
				return err
			}
		}
	}

	// Promote whenever the final state names a primary that is not already
	// flagged in the database. This is a no-op on replay, where the row was
	// never reinserted and is already flagged.
	if finalPrimary != nil {
		alreadyFlagged := finalPrimary.ID == prePrimaryID && !reinserted[finalPrimary.ID]
		if !alreadyFlagged {
			if err := methodRepo.PromoteContactMethodPrimaryByContact(ctx, contactID, finalPrimary.ID); err != nil {
				return err
			}
		}
	}

	return nil
}

// buildOperationResults emits exactly one result per SUBMITTED operation, at
// that operation's own index — never one per folded operation. Coalesced
// operations each get their own result carrying the same resolved id.
func buildOperationResults(resolutions []resolution, postState []repository.ContactMethod) []ContactMethodOperationResult {
	byID := make(map[uuid.UUID]*repository.ContactMethod, len(postState))
	for i := range postState {
		byID[postState[i].ID] = &postState[i]
	}

	results := make([]ContactMethodOperationResult, len(resolutions))
	for i, r := range resolutions {
		results[i] = ContactMethodOperationResult{
			Index:    i,
			Outcome:  r.outcome,
			MethodID: r.methodID,
		}
		if r.hasSnapshot {
			if row, ok := byID[r.methodID]; ok {
				snapshot := *row
				results[i].Method = &snapshot
			}
		}
	}
	return results
}
