package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"personal-crm/backend/internal/db"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// The correction representation (KNW-037). A correction exists only as:
//   - removing contact C from judged interaction node N: an accepted
//     not_participant_in assertion from C's person node (id == C) to N;
//   - removing everyone from N: an accepted removed_for_everyone assertion on N
//     with value true;
//   - merging root M into root W: M's registry merge alias set to W
//     (MergeInteractionNodeTx) plus an accepted same_interaction_as assertion
//     from M to W.
//
// Each correction assertion carries one provenance locator with source kind
// and producer kind user, whose source id names the correction act and is
// shared by every assertion that act writes. A removal's locator also carries
// its reason as field "reason" with quote duplicate, did_not_attend or
// did_not_happen.

// EffectiveParticipation is one contact's effective participation (arc seam
// S4): the contact's observations with the user's corrections applied. A
// correctable entry is keyed by the root of its members' merge-alias chains; a
// message-burst entry has no interaction node and is its own only member.
//
// Every value here is derived on demand from observation rows, interaction-node
// merge aliases and accepted correction assertions by the effective_participation
// view (migration 089), the one implementation of the derivation. Nothing
// stores it.
type EffectiveParticipation struct {
	ContactID uuid.UUID
	// InteractionNodeID is the root interaction node of the entry; nil for a
	// message burst.
	InteractionNodeID *uuid.UUID
	Direction         string
	OccurredAt        time.Time
	Members           []EffectiveParticipationMember
}

// EffectiveParticipationMember is one observation an effective participation
// groups, with its own interaction node (nil for a message burst).
type EffectiveParticipationMember struct {
	ObservationID     uuid.UUID  `json:"observation_id"`
	Source            string     `json:"source"`
	SourceRef         *string    `json:"source_ref"`
	InteractionNodeID *uuid.UUID `json:"interaction_node_id"`
}

// ObservationVerdict is the verdict read (arc seam S3): whether
// a correction excludes a live observation and, when it is effective, the
// contact's effective participation it belongs to.
type ObservationVerdict struct {
	Excluded bool
	// Entry is the effective participation that holds the observation; nil when
	// the observation is excluded.
	Entry *EffectiveParticipation
}

// InteractionTree is the alias tree of a merged interaction: its root and every
// interaction node aliased under it, the root included.
type InteractionTree struct {
	Root  uuid.UUID
	Nodes []uuid.UUID
}

// ErrInteractionMergeNotRoots is returned by the interaction-node merge write
// when the pair is not two distinct live root interaction nodes.
var ErrInteractionMergeNotRoots = errors.New("interaction merge: loser and winner must be two distinct root interaction nodes")

// ListEffectiveParticipation returns the contact's effective participations,
// latest first.
func (r *InteractionRepository) ListEffectiveParticipation(ctx context.Context, contactID uuid.UUID) ([]EffectiveParticipation, error) {
	return listEffectiveParticipation(ctx, r.queries, contactID)
}

// ListEffectiveParticipationTx is the tx-bound variant of
// ListEffectiveParticipation; it sees the transaction's own writes.
func (r *InteractionRepository) ListEffectiveParticipationTx(ctx context.Context, tx pgx.Tx, contactID uuid.UUID) ([]EffectiveParticipation, error) {
	if tx == nil {
		return nil, fmt.Errorf("ListEffectiveParticipationTx: %w", errNilTx)
	}
	return listEffectiveParticipation(ctx, db.New(tx), contactID)
}

func listEffectiveParticipation(ctx context.Context, q db.Querier, contactID uuid.UUID) ([]EffectiveParticipation, error) {
	rows, err := q.ListEffectiveParticipationForContact(ctx, contactID)
	if err != nil {
		return nil, fmt.Errorf("list effective participation: %w", err)
	}
	return convertEffectiveParticipations(rows)
}

// ListInteractionParticipants returns the effective participants of the
// interaction whose alias tree holds nodeID (its root or any node merged into
// it): exactly the contacts holding an entry for the root, each with that
// entry. These are the contacts a removal can name.
func (r *InteractionRepository) ListInteractionParticipants(ctx context.Context, nodeID uuid.UUID) ([]EffectiveParticipation, error) {
	return listInteractionParticipants(ctx, r.queries, nodeID)
}

// ListInteractionParticipantsTx is the tx-bound variant of
// ListInteractionParticipants.
func (r *InteractionRepository) ListInteractionParticipantsTx(ctx context.Context, tx pgx.Tx, nodeID uuid.UUID) ([]EffectiveParticipation, error) {
	if tx == nil {
		return nil, fmt.Errorf("ListInteractionParticipantsTx: %w", errNilTx)
	}
	return listInteractionParticipants(ctx, db.New(tx), nodeID)
}

func listInteractionParticipants(ctx context.Context, q db.Querier, nodeID uuid.UUID) ([]EffectiveParticipation, error) {
	rows, err := q.ListInteractionParticipants(ctx, nodeID)
	if err != nil {
		return nil, fmt.Errorf("list interaction participants: %w", err)
	}
	return convertEffectiveParticipations(rows)
}

// ObservationVerdictTx is the verdict read for the live observation
// observationID, inside the caller's tx so it sees the write that just
// recorded the observation. Returns db.ErrNotFound when no live observation
// has that id.
func (r *InteractionRepository) ObservationVerdictTx(ctx context.Context, tx pgx.Tx, observationID uuid.UUID) (*ObservationVerdict, error) {
	if tx == nil {
		return nil, fmt.Errorf("ObservationVerdictTx: %w", errNilTx)
	}
	q := db.New(tx)
	verdict, err := q.GetObservationVerdict(ctx, observationID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, fmt.Errorf("get observation verdict: %w", err)
	}
	if verdict.Excluded {
		return &ObservationVerdict{Excluded: true}, nil
	}
	row, err := q.GetEffectiveParticipationOfObservation(ctx, db.GetEffectiveParticipationOfObservationParams{
		ContactID:     verdict.ContactID,
		RootNodeID:    verdict.RootNodeID,
		ObservationID: observationID,
	})
	if err != nil {
		return nil, fmt.Errorf("get effective participation of observation: %w", err)
	}
	entry, err := convertEffectiveParticipation(row)
	if err != nil {
		return nil, err
	}
	return &ObservationVerdict{Entry: &entry}, nil
}

// GetInteractionTree returns the alias tree that holds nodeID. Returns
// db.ErrNotFound when nodeID names no interaction node.
func (r *InteractionRepository) GetInteractionTree(ctx context.Context, nodeID uuid.UUID) (*InteractionTree, error) {
	return getInteractionTree(ctx, r.queries, nodeID)
}

// GetInteractionTreeTx is the tx-bound variant of GetInteractionTree.
func (r *InteractionRepository) GetInteractionTreeTx(ctx context.Context, tx pgx.Tx, nodeID uuid.UUID) (*InteractionTree, error) {
	if tx == nil {
		return nil, fmt.Errorf("GetInteractionTreeTx: %w", errNilTx)
	}
	return getInteractionTree(ctx, db.New(tx), nodeID)
}

func getInteractionTree(ctx context.Context, q db.Querier, nodeID uuid.UUID) (*InteractionTree, error) {
	rows, err := q.ListInteractionAliasTree(ctx, nodeID)
	if err != nil {
		return nil, fmt.Errorf("list interaction alias tree: %w", err)
	}
	if len(rows) == 0 {
		return nil, db.ErrNotFound
	}
	tree := &InteractionTree{Nodes: make([]uuid.UUID, len(rows))}
	for i, row := range rows {
		tree.Nodes[i] = row.ID
		if row.MergedInto == nil {
			tree.Root = row.ID
		}
	}
	return tree, nil
}

// AcquireCorrectionLocksTx takes the correction lock on each interaction node
// in nodeIDs: a transaction-scoped advisory lock keyed by node id, released at
// commit or rollback. Taking a lock the transaction already holds is a no-op.
func (r *NodeRepository) AcquireCorrectionLocksTx(ctx context.Context, tx pgx.Tx, nodeIDs ...uuid.UUID) error {
	if tx == nil {
		return fmt.Errorf("AcquireCorrectionLocksTx: %w", errNilTx)
	}
	return acquireCorrectionLocks(ctx, db.New(tx), nodeIDs)
}

func acquireCorrectionLocks(ctx context.Context, q db.Querier, nodeIDs []uuid.UUID) error {
	if len(nodeIDs) == 0 {
		return nil
	}
	if err := q.AcquireInteractionCorrectionLocks(ctx, nodeIDs); err != nil {
		return fmt.Errorf("acquire correction locks: %w", err)
	}
	return nil
}

// MergeInteractionNodeTx is the interaction-node merge write: it aliases the
// root interaction node loser to the distinct root interaction node winner,
// after taking the correction lock on both. It leaves the loser without a
// tombstone, rewrites no alias, and changes no assertion, observation or
// content row. Returns ErrInteractionMergeNotRoots when the pair is not two
// distinct live roots.
func (r *NodeRepository) MergeInteractionNodeTx(ctx context.Context, tx pgx.Tx, loser, winner uuid.UUID) error {
	if tx == nil {
		return fmt.Errorf("MergeInteractionNodeTx: %w", errNilTx)
	}
	q := db.New(tx)
	if err := acquireCorrectionLocks(ctx, q, []uuid.UUID{loser, winner}); err != nil {
		return err
	}
	n, err := q.AliasInteractionNode(ctx, db.AliasInteractionNodeParams{LoserID: loser, WinnerID: winner})
	if err != nil {
		return fmt.Errorf("alias interaction node: %w", err)
	}
	if n == 0 {
		return ErrInteractionMergeNotRoots
	}
	return nil
}

func convertEffectiveParticipations(rows []*db.EffectiveParticipation) ([]EffectiveParticipation, error) {
	out := make([]EffectiveParticipation, len(rows))
	for i, row := range rows {
		ep, err := convertEffectiveParticipation(row)
		if err != nil {
			return nil, err
		}
		out[i] = ep
	}
	return out, nil
}

func convertEffectiveParticipation(row *db.EffectiveParticipation) (EffectiveParticipation, error) {
	ep := EffectiveParticipation{
		ContactID:         row.ContactID,
		InteractionNodeID: row.InteractionNodeID,
		Direction:         row.Direction,
		OccurredAt:        row.OccurredAt.UTC(),
	}
	if err := json.Unmarshal(row.Members, &ep.Members); err != nil {
		return EffectiveParticipation{}, fmt.Errorf("decode effective participation members: %w", err)
	}
	return ep, nil
}
