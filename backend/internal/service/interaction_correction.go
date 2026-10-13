package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Removal reasons (IXN-018). A removal records its reason in the user
// provenance of each assertion it writes; the reason never changes what is
// removed.
const (
	RemovalReasonDuplicate     = "duplicate"
	RemovalReasonDidNotAttend  = "did_not_attend"
	RemovalReasonDidNotHappen  = "did_not_happen"
	correctionEventSource      = "interaction_correction"
	correctionPredicateMerge   = "same_interaction_as"
	correctionPredicateRemoved = "not_participant_in"
	correctionPredicateAll     = "removed_for_everyone"
)

var (
	// ErrCorrectionRefused is returned when a correction request names no
	// applicable interaction or contact. Nothing is written.
	ErrCorrectionRefused = errors.New("correction refused")
	// ErrInteractionNotFound is returned when an id names no interaction node.
	ErrInteractionNotFound = errors.New("interaction not found")
)

func refused(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrCorrectionRefused, fmt.Sprintf(format, args...))
}

// IsRemovalReason reports whether reason is one of the three removal reasons.
func IsRemovalReason(reason string) bool {
	switch reason {
	case RemovalReasonDuplicate, RemovalReasonDidNotAttend, RemovalReasonDidNotHappen:
		return true
	default:
		return false
	}
}

// InteractionMergeRequest declares the listed interactions one interaction for
// every attendee. ContactID names the contact whose resulting participation
// the merge answers with.
type InteractionMergeRequest struct {
	InteractionIDs []uuid.UUID
	ContactID      uuid.UUID
}

// InteractionRemovalRequest removes participations of the interaction that
// holds InteractionID: the listed contacts', or everyone's. A nil ContactIDs
// means no contact list was given; a non-nil one was given, even when empty.
type InteractionRemovalRequest struct {
	InteractionID uuid.UUID
	ContactIDs    []uuid.UUID
	Everyone      bool
	Reason        string
}

// MergedParticipation is a merge's answer: the merged interaction (the root of
// the first listed id) and the requesting contact's effective participation in
// it.
type MergedParticipation struct {
	InteractionID uuid.UUID
	Direction     string
	OccurredAt    time.Time
}

// correctionCadence is the subset of *consumer.CadenceUpdater a correction
// recomputes cadence fields through.
type correctionCadence interface {
	RecomputeAfterCorrection(ctx context.Context, tx pgx.Tx, contactID uuid.UUID, before, after []repository.ApplyInteractionRequest) error
}

// correctionFollowUp is the subset of *consumer.FollowUpManager a correction
// reconciles follow-up tasks through.
type correctionFollowUp interface {
	ReconcileAfterCorrection(ctx context.Context, tx pgx.Tx, contactID uuid.UUID, before, after *repository.ApplyInteractionRequest, act uuid.UUID) error
}

// correctionPublisher publishes the interaction.corrected event in the
// correction's tx.
type correctionPublisher interface {
	PublishTx(ctx context.Context, tx pgx.Tx, env *events.Envelope) error
}

// InteractionCorrectionService writes the user's corrections of interactions
// (IXN-016, IXN-017, IXN-018): merges and removals, as SP1 state in the
// correction representation (KNW-037) — accepted same_interaction_as,
// not_participant_in and removed_for_everyone assertions with user
// provenance, written through AssertService, plus the interaction-node merge
// alias. It is their only writer.
//
// Each correction runs in one transaction: it takes the correction lock on
// every node of the alias trees it reads or writes, then the row lock of every
// contact it recomputes, writes the correction, recomputes those contacts'
// cadence fields and follow-up tasks from effective participation, and
// publishes interaction.corrected. A merge preview runs the same merge and
// rolls it back, so the preview's answer is the merge's.
type InteractionCorrectionService struct {
	pool         txBeginner
	interactions *repository.InteractionRepository
	nodes        *repository.NodeRepository
	assertions   *repository.AssertionRepository
	contacts     *repository.ContactRepository
	asserts      *AssertService
	cadence      correctionCadence
	followUp     correctionFollowUp
	bus          correctionPublisher
}

// txBeginner opens the correction's transaction.
type txBeginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// NewInteractionCorrectionService wires the correction routes' service.
func NewInteractionCorrectionService(
	pool txBeginner,
	interactions *repository.InteractionRepository,
	nodes *repository.NodeRepository,
	assertions *repository.AssertionRepository,
	contacts *repository.ContactRepository,
	asserts *AssertService,
	cadence correctionCadence,
	followUp correctionFollowUp,
	bus correctionPublisher,
) *InteractionCorrectionService {
	return &InteractionCorrectionService{
		pool:         pool,
		interactions: interactions,
		nodes:        nodes,
		assertions:   assertions,
		contacts:     contacts,
		asserts:      asserts,
		cadence:      cadence,
		followUp:     followUp,
		bus:          bus,
	}
}

// Merge declares the listed interactions one interaction for every attendee
// and answers with the requesting contact's resulting participation.
func (s *InteractionCorrectionService) Merge(ctx context.Context, req InteractionMergeRequest) (*MergedParticipation, error) {
	return s.merge(ctx, req, true)
}

// PreviewMerge answers exactly what Merge would answer for req and writes
// nothing: it runs the merge and rolls it back.
func (s *InteractionCorrectionService) PreviewMerge(ctx context.Context, req InteractionMergeRequest) (*MergedParticipation, error) {
	return s.merge(ctx, req, false)
}

func (s *InteractionCorrectionService) merge(ctx context.Context, req InteractionMergeRequest, commit bool) (result *MergedParticipation, err error) {
	if len(req.InteractionIDs) < 2 {
		return nil, refused("a merge names at least two interactions")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() {
		if rbErr := tx.Rollback(ctx); rbErr != nil && !errors.Is(rbErr, pgx.ErrTxClosed) && err == nil {
			err = rbErr
		}
	}()

	trees, err := s.lockTrees(ctx, tx, req.InteractionIDs)
	if err != nil {
		return nil, err
	}
	if _, err := s.contacts.GetContactTx(ctx, tx, req.ContactID); err != nil {
		return nil, err
	}
	var roots []uuid.UUID
	for _, tree := range trees {
		if !slices.Contains(roots, tree.Root) {
			roots = append(roots, tree.Root)
		}
	}
	if len(roots) < 2 {
		return nil, refused("the listed ids name fewer than two distinct interactions")
	}

	// The contacts observed in a merged interaction: every effective participant
	// of any listed interaction.
	var affected []uuid.UUID
	for _, root := range roots {
		participants, err := s.interactions.ListInteractionParticipantsTx(ctx, tx, root)
		if err != nil {
			return nil, err
		}
		for _, p := range participants {
			if !slices.Contains(affected, p.ContactID) {
				affected = append(affected, p.ContactID)
			}
		}
	}
	if !slices.Contains(affected, req.ContactID) {
		return nil, refused("contact %s has no participation in the listed interactions", req.ContactID)
	}

	before, err := s.lockAndRead(ctx, tx, affected)
	if err != nil {
		return nil, err
	}

	act := uuid.New()
	winner := roots[0]
	for _, loser := range roots[1:] {
		if err := s.nodes.MergeInteractionNodeTx(ctx, tx, loser, winner); err != nil {
			return nil, fmt.Errorf("merge interaction node: %w", err)
		}
		object := winner
		if _, err := s.asserts.AssertTx(ctx, tx, AssertRequest{
			SubjectNodeID: loser,
			PredicateKey:  correctionPredicateMerge,
			ObjectNodeID:  &object,
			Confidence:    100,
			Locators:      []ProvenanceLocator{correctionLocator(act, nil)},
		}); err != nil {
			return nil, fmt.Errorf("assert %s: %w", correctionPredicateMerge, err)
		}
	}

	entries, err := s.interactions.ListEffectiveParticipationTx(ctx, tx, req.ContactID)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if entry.InteractionNodeID != nil && *entry.InteractionNodeID == winner {
			result = &MergedParticipation{InteractionID: winner, Direction: entry.Direction, OccurredAt: entry.OccurredAt}
		}
	}
	if result == nil {
		return nil, fmt.Errorf("merge: contact %s has no participation in merged interaction %s", req.ContactID, winner)
	}
	if !commit {
		return result, nil
	}

	if err := s.recompute(ctx, tx, act, before); err != nil {
		return nil, err
	}
	if err := s.publish(ctx, tx, act, events.CorrectionMerge, winner, affected); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return result, nil
}

// Remove removes the selected attendees' participations of the interaction
// that holds req.InteractionID. It binds to every member node of that
// interaction as it stands, so it covers every later re-observation of those
// source objects and never a source object merged in afterwards.
func (s *InteractionCorrectionService) Remove(ctx context.Context, req InteractionRemovalRequest) (err error) {
	if req.Everyone == (req.ContactIDs != nil) || (req.ContactIDs != nil && len(req.ContactIDs) == 0) {
		return refused("a removal names exactly one scope: a non-empty contact list or everyone")
	}
	if !IsRemovalReason(req.Reason) {
		return refused("reason %q is not duplicate, did_not_attend or did_not_happen", req.Reason)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if rbErr := tx.Rollback(ctx); rbErr != nil && !errors.Is(rbErr, pgx.ErrTxClosed) && err == nil {
			err = rbErr
		}
	}()

	trees, err := s.lockTrees(ctx, tx, []uuid.UUID{req.InteractionID})
	if err != nil {
		return err
	}
	tree := trees[0]
	participants, err := s.interactions.ListInteractionParticipantsTx(ctx, tx, tree.Root)
	if err != nil {
		return err
	}
	if len(participants) == 0 {
		return refused("interaction %s has no participation to remove", tree.Root)
	}
	var removed []uuid.UUID
	if req.Everyone {
		for _, p := range participants {
			removed = append(removed, p.ContactID)
		}
	} else {
		for _, contactID := range req.ContactIDs {
			if !slices.ContainsFunc(participants, func(p repository.EffectiveParticipation) bool { return p.ContactID == contactID }) {
				return refused("contact %s is not a participant of interaction %s", contactID, tree.Root)
			}
			if !slices.Contains(removed, contactID) {
				removed = append(removed, contactID)
			}
		}
	}

	before, err := s.lockAndRead(ctx, tx, removed)
	if err != nil {
		return err
	}

	act := uuid.New()
	reason := req.Reason
	nodes := slices.Clone(tree.Nodes)
	slices.SortFunc(nodes, compareUUID)
	for _, node := range nodes {
		existing, err := s.assertions.ListAssertionsTouchingNodeTx(ctx, tx, node)
		if err != nil {
			return fmt.Errorf("list corrections of node %s: %w", node, err)
		}
		if req.Everyone {
			if removedForEveryone(existing, node) {
				continue
			}
			yes := true
			if _, err := s.asserts.AssertTx(ctx, tx, AssertRequest{
				SubjectNodeID: node,
				PredicateKey:  correctionPredicateAll,
				ValueBool:     &yes,
				Confidence:    100,
				Locators:      []ProvenanceLocator{correctionLocator(act, &reason)},
			}); err != nil {
				return fmt.Errorf("assert %s: %w", correctionPredicateAll, err)
			}
			continue
		}
		for _, contactID := range removed {
			if removedFrom(existing, contactID, node) {
				continue
			}
			object := node
			if _, err := s.asserts.AssertTx(ctx, tx, AssertRequest{
				SubjectNodeID: contactID,
				PredicateKey:  correctionPredicateRemoved,
				ObjectNodeID:  &object,
				Confidence:    100,
				Locators:      []ProvenanceLocator{correctionLocator(act, &reason)},
			}); err != nil {
				return fmt.Errorf("assert %s: %w", correctionPredicateRemoved, err)
			}
		}
	}

	if err := s.recompute(ctx, tx, act, before); err != nil {
		return err
	}
	if err := s.publish(ctx, tx, act, events.CorrectionRemoval, tree.Root, removed); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// lockTrees takes the correction lock on every node of the alias trees that
// hold ids, and returns those trees, aligned with ids, as they stand under the
// lock. A tree read before the lock can change before the lock is granted (a
// concurrent merge); the locked re-read then differs, and the round is rolled
// back, releasing its locks, so the next round takes the larger set in one
// ordered acquisition. Returns ErrInteractionNotFound when an id names no
// interaction node.
func (s *InteractionCorrectionService) lockTrees(ctx context.Context, tx pgx.Tx, ids []uuid.UUID) ([]*repository.InteractionTree, error) {
	trees, err := s.readTrees(ctx, tx, ids)
	if err != nil {
		return nil, err
	}
	for {
		round, err := tx.Begin(ctx)
		if err != nil {
			return nil, err
		}
		var nodes []uuid.UUID
		for _, tree := range trees {
			nodes = append(nodes, tree.Nodes...)
		}
		if err := s.nodes.AcquireCorrectionLocksTx(ctx, round, nodes...); err != nil {
			_ = round.Rollback(ctx)
			return nil, err
		}
		locked, err := s.readTrees(ctx, round, ids)
		if err != nil {
			_ = round.Rollback(ctx)
			return nil, err
		}
		if sameTrees(trees, locked) {
			return locked, round.Commit(ctx)
		}
		if err := round.Rollback(ctx); err != nil {
			return nil, err
		}
		trees = locked
	}
}

func (s *InteractionCorrectionService) readTrees(ctx context.Context, tx pgx.Tx, ids []uuid.UUID) ([]*repository.InteractionTree, error) {
	trees := make([]*repository.InteractionTree, len(ids))
	for i, id := range ids {
		tree, err := s.interactions.GetInteractionTreeTx(ctx, tx, id)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				return nil, fmt.Errorf("%w: %s", ErrInteractionNotFound, id)
			}
			return nil, err
		}
		trees[i] = tree
	}
	return trees, nil
}

func sameTrees(a, b []*repository.InteractionTree) bool {
	for i := range a {
		if a[i].Root != b[i].Root || !slices.Equal(a[i].Nodes, b[i].Nodes) {
			return false
		}
	}
	return true
}

// contactBefore is a recomputed contact's effective participation before the
// correction, as the recording rules take it.
type contactBefore struct {
	contactID uuid.UUID
	recorded  []repository.ApplyInteractionRequest
	awaited   *repository.ApplyInteractionRequest
}

// lockAndRead locks the cadence row of each contact, in id order, and reads
// its effective participation before the correction.
func (s *InteractionCorrectionService) lockAndRead(ctx context.Context, tx pgx.Tx, contactIDs []uuid.UUID) ([]contactBefore, error) {
	ids := slices.Clone(contactIDs)
	slices.SortFunc(ids, compareUUID)
	before := make([]contactBefore, 0, len(ids))
	for _, id := range ids {
		if err := s.contacts.LockContactForDateRecomputeTx(ctx, tx, id); err != nil {
			return nil, fmt.Errorf("lock contact %s: %w", id, err)
		}
		recorded, awaited, err := s.recordedParticipation(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		before = append(before, contactBefore{contactID: id, recorded: recorded, awaited: awaited})
	}
	return before, nil
}

// recompute recomputes each contact's cadence fields and follow-up tasks from
// its effective participation after the correction.
func (s *InteractionCorrectionService) recompute(ctx context.Context, tx pgx.Tx, act uuid.UUID, before []contactBefore) error {
	for _, b := range before {
		recorded, awaited, err := s.recordedParticipation(ctx, tx, b.contactID)
		if err != nil {
			return err
		}
		if err := s.cadence.RecomputeAfterCorrection(ctx, tx, b.contactID, b.recorded, recorded); err != nil {
			return fmt.Errorf("recompute cadence of contact %s: %w", b.contactID, err)
		}
		if err := s.followUp.ReconcileAfterCorrection(ctx, tx, b.contactID, b.awaited, awaited, act); err != nil {
			return fmt.Errorf("reconcile follow-up of contact %s: %w", b.contactID, err)
		}
	}
	return nil
}

// recordedParticipation reads the contact's effective participation as the
// recording rules take it: each entry at its own direction and time, with the
// source of the member observation recorded last, in the order the entries
// were recorded (an entry is recorded when its last member was). It also
// returns the awaited outbound: the latest outbound entry with no later
// inbound or mutual entry, nil when none.
func (s *InteractionCorrectionService) recordedParticipation(ctx context.Context, tx pgx.Tx, contactID uuid.UUID) ([]repository.ApplyInteractionRequest, *repository.ApplyInteractionRequest, error) {
	entries, err := s.interactions.ListEffectiveParticipationTx(ctx, tx, contactID)
	if err != nil {
		return nil, nil, err
	}
	type recordedEntry struct {
		req        repository.ApplyInteractionRequest
		recordedAt time.Time
	}
	recorded := make([]recordedEntry, 0, len(entries))
	var latestOutbound, latestResponse *repository.ApplyInteractionRequest
	for _, entry := range entries {
		var last *repository.Interaction
		for _, member := range entry.Members {
			row, err := s.interactions.GetInteractionTx(ctx, tx, member.ObservationID)
			if err != nil {
				return nil, nil, fmt.Errorf("read member observation %s: %w", member.ObservationID, err)
			}
			if last == nil || row.CreatedAt.After(last.CreatedAt) {
				last = row
			}
		}
		req := repository.ApplyInteractionRequest{
			ContactID:  contactID,
			Direction:  entry.Direction,
			Source:     last.Source,
			OccurredAt: entry.OccurredAt,
		}
		recorded = append(recorded, recordedEntry{req: req, recordedAt: last.CreatedAt})
		switch entry.Direction {
		case repository.InteractionDirectionOutbound:
			if latestOutbound == nil || req.OccurredAt.After(latestOutbound.OccurredAt) {
				r := req
				latestOutbound = &r
			}
		default:
			if latestResponse == nil || req.OccurredAt.After(latestResponse.OccurredAt) {
				r := req
				latestResponse = &r
			}
		}
	}
	sort.SliceStable(recorded, func(i, j int) bool {
		if !recorded[i].recordedAt.Equal(recorded[j].recordedAt) {
			return recorded[i].recordedAt.Before(recorded[j].recordedAt)
		}
		return recorded[i].req.OccurredAt.Before(recorded[j].req.OccurredAt)
	})
	reqs := make([]repository.ApplyInteractionRequest, len(recorded))
	for i, r := range recorded {
		reqs[i] = r.req
	}
	var awaited *repository.ApplyInteractionRequest
	if latestOutbound != nil && (latestResponse == nil || !latestResponse.OccurredAt.After(latestOutbound.OccurredAt)) {
		awaited = latestOutbound
	}
	return reqs, awaited, nil
}

// publish publishes the correction's interaction.corrected event in its tx.
func (s *InteractionCorrectionService) publish(ctx context.Context, tx pgx.Tx, act uuid.UUID, correction string, root uuid.UUID, recomputed []uuid.UUID) error {
	ids := slices.Clone(recomputed)
	slices.SortFunc(ids, compareUUID)
	if ids == nil {
		ids = []uuid.UUID{}
	}
	payload, err := events.Marshal(events.KindInteractionCorrected, events.InteractionCorrectedPayload{
		Version:              1,
		Correction:           correction,
		InteractionNodeID:    root,
		RecomputedContactIDs: ids,
	})
	if err != nil {
		return fmt.Errorf("marshal interaction.corrected: %w", err)
	}
	if err := s.bus.PublishTx(ctx, tx, &events.Envelope{
		Source:     correctionEventSource,
		SourceID:   act.String(),
		Kind:       events.KindInteractionCorrected,
		Payload:    payload,
		ObservedAt: accelerated.GetCurrentTime(),
	}); err != nil {
		return fmt.Errorf("publish interaction.corrected: %w", err)
	}
	return nil
}

// correctionLocator is the one provenance locator every assertion of a
// correction act carries: user source and producer, the act's id as source
// id, and a removal's reason as field "reason".
func correctionLocator(act uuid.UUID, reason *string) ProvenanceLocator {
	loc := ProvenanceLocator{
		SourceKind:   repository.SourceKindUser,
		SourceID:     act.String(),
		ProducerKind: repository.ProducerKindUser,
	}
	if reason != nil {
		field := correctionReasonField
		loc.Field = &field
		loc.Quote = reason
	}
	return loc
}

// removedFrom reports whether an accepted not_participant_in already removes
// contactID from node.
func removedFrom(existing []repository.Assertion, contactID, node uuid.UUID) bool {
	return slices.ContainsFunc(existing, func(a repository.Assertion) bool {
		return a.PredicateKey == correctionPredicateRemoved && a.Status == repository.AssertionStatusAccepted &&
			a.SubjectNodeID == contactID && a.ObjectNodeID != nil && *a.ObjectNodeID == node
	})
}

// removedForEveryone reports whether an accepted removed_for_everyone already
// removes node for everyone.
func removedForEveryone(existing []repository.Assertion, node uuid.UUID) bool {
	return slices.ContainsFunc(existing, func(a repository.Assertion) bool {
		return a.PredicateKey == correctionPredicateAll && a.Status == repository.AssertionStatusAccepted &&
			a.SubjectNodeID == node && a.ValueBool != nil && *a.ValueBool
	})
}

func compareUUID(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) }
