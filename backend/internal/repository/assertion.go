package repository

import (
	"context"
	"errors"
	"time"

	"personal-crm/backend/internal/db"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// Assertion status constants. The state machine is proposed → accepted | rejected
// and accepted → superseded | retracted; the terminal-knowledge_to CHECK enforces
// that a terminal status carries a knowledge_to.
const (
	AssertionStatusProposed   = "proposed"
	AssertionStatusAccepted   = "accepted"
	AssertionStatusRejected   = "rejected"
	AssertionStatusSuperseded = "superseded"
	AssertionStatusRetracted  = "retracted"
)

// Assertion closure-reason constants (nullable; set when a row is closed).
const (
	ClosureReasonEnded      = "ended"
	ClosureReasonSuperseded = "superseded"
	ClosureReasonRetracted  = "retracted"
	ClosureReasonRejected   = "rejected"
)

// Provenance source-kind constants (the closed enum the CHECK enforces). The
// content kinds back fact/edge extraction; calendar_event/phone_call are
// metadata sources; user/agent_session/anarlog_transcript carry no backing-row
// existence check.
const (
	SourceKindCommsMessage      = "comms_message"
	SourceKindTelegramMessage   = "telegram_message"
	SourceKindMessagesMessage   = "messages_message"
	SourceKindMeetingNote       = "meeting_note"
	SourceKindAnarlogTranscript = "anarlog_transcript"
	SourceKindCalendarEvent     = "calendar_event"
	SourceKindPhoneCall         = "phone_call"
	SourceKindUser              = "user"
	SourceKindAgentSession      = "agent_session"
)

// Provenance producer-kind constants (per-locator; the strongest across an
// assertion's locators denorms onto assertion.trust_tier).
const (
	ProducerKindExtractor = "extractor"
	ProducerKindAgent     = "agent"
	ProducerKindUser      = "user"
)

// Predicate-key constants for the contact knowledge-column cutover. These three
// predicates back the derived contact.location / contact.birthday /
// contact.how_met cache columns: lives_in is an edge to a place entity node
// (the cache holds the place node's canonical_label); birthday/how_met are
// facts (the cache holds value_date / value_text).
const (
	PredicateLivesIn  = "lives_in"
	PredicateBirthday = "birthday"
	PredicateHowMet   = "how_met"
)

// Assertion is the bi-temporal fact/edge row. Exactly one payload field is set
// (ObjectNodeID for an edge, or one ValueX for a fact). Nullable temporal bounds
// and the supersession/closure fields are pointers; an open-ended bound is nil.
type Assertion struct {
	ID             uuid.UUID  `json:"id"`
	SubjectNodeID  uuid.UUID  `json:"subject_node_id"`
	PredicateKey   string     `json:"predicate_key"`
	ObjectNodeID   *uuid.UUID `json:"object_node_id,omitempty"`
	ValueText      *string    `json:"value_text,omitempty"`
	ValueNum       *float64   `json:"value_num,omitempty"`
	ValueDate      *time.Time `json:"value_date,omitempty"`
	ValueBool      *bool      `json:"value_bool,omitempty"`
	ValidFrom      *time.Time `json:"valid_from,omitempty"`
	ValidTo        *time.Time `json:"valid_to,omitempty"`
	KnowledgeFrom  time.Time  `json:"knowledge_from"`
	KnowledgeTo    *time.Time `json:"knowledge_to,omitempty"`
	Confidence     int16      `json:"confidence"`
	Salience       int16      `json:"salience"`
	Status         string     `json:"status"`
	ClosureReason  *string    `json:"closure_reason,omitempty"`
	SupersededBy   *uuid.UUID `json:"superseded_by,omitempty"`
	TrustTier      *string    `json:"trust_tier,omitempty"`
	PropositionKey string     `json:"proposition_key"`
	CreatedAt      time.Time  `json:"created_at"`
}

// Provenance is one corroborating source locator for an assertion. SourceID is
// the row id (UUID-as-text) for content kinds, or a stable ref for user/
// agent_session. The locator's span/chunk fields and Quote are nullable.
type Provenance struct {
	AssertionID     uuid.UUID `json:"assertion_id"`
	LocatorHash     string    `json:"locator_hash"`
	SourceKind      string    `json:"source_kind"`
	SourceID        string    `json:"source_id"`
	ProducerKind    string    `json:"producer_kind"`
	ProducerVersion string    `json:"producer_version"`
	Field           *string   `json:"field,omitempty"`
	StartOffset     *int32    `json:"start_offset,omitempty"`
	EndOffset       *int32    `json:"end_offset,omitempty"`
	ChunkID         *string   `json:"chunk_id,omitempty"`
	InputHash       string    `json:"input_hash"`
	Quote           *string   `json:"quote,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
}

// InsertAssertionParams is the input for InsertAssertion. The caller (the write
// API) supplies the computed proposition_key and the full bi-temporal envelope.
// Nullable fields are pointers.
type InsertAssertionParams struct {
	SubjectNodeID  uuid.UUID
	PredicateKey   string
	ObjectNodeID   *uuid.UUID
	ValueText      *string
	ValueNum       *float64
	ValueDate      *time.Time
	ValueBool      *bool
	ValidFrom      *time.Time
	ValidTo        *time.Time
	KnowledgeFrom  time.Time
	KnowledgeTo    *time.Time
	Confidence     int16
	Salience       int16
	Status         string
	ClosureReason  *string
	SupersededBy   *uuid.UUID
	TrustTier      *string
	PropositionKey string
}

// InsertProvenanceParams is the input for InsertProvenance. The caller computes
// locator_hash from the full locator identity.
type InsertProvenanceParams struct {
	AssertionID     uuid.UUID
	LocatorHash     string
	SourceKind      string
	SourceID        string
	ProducerKind    string
	ProducerVersion string
	Field           *string
	StartOffset     *int32
	EndOffset       *int32
	ChunkID         *string
	InputHash       string
	Quote           *string
}

// CloseAssertionParams is the input for CloseAssertion (the terminal close used
// by the present-successor, closure-only, retract, and rollover paths).
type CloseAssertionParams struct {
	ID            uuid.UUID
	ValidTo       *time.Time
	Status        string
	ClosureReason *string
	SupersededBy  *uuid.UUID
	KnowledgeTo   *time.Time
}

// AssertionRepository handles assertion + provenance persistence.
type AssertionRepository struct {
	queries db.Querier
}

// NewAssertionRepository creates a new AssertionRepository.
func NewAssertionRepository(queries db.Querier) *AssertionRepository {
	return &AssertionRepository{queries: queries}
}

func convertDbAssertion(a *db.Assertion) Assertion {
	out := Assertion{
		PredicateKey:   a.PredicateKey,
		Confidence:     a.Confidence,
		Salience:       a.Salience,
		Status:         a.Status,
		PropositionKey: a.PropositionKey,
	}
	if a.ID.Valid {
		out.ID = uuid.UUID(a.ID.Bytes)
	}
	if a.SubjectNodeID.Valid {
		out.SubjectNodeID = uuid.UUID(a.SubjectNodeID.Bytes)
	}
	if a.ObjectNodeID.Valid {
		id := uuid.UUID(a.ObjectNodeID.Bytes)
		out.ObjectNodeID = &id
	}
	if a.ValueText.Valid {
		out.ValueText = &a.ValueText.String
	}
	if a.ValueNum.Valid {
		out.ValueNum = &a.ValueNum.Float64
	}
	out.ValueDate = pgDateToTimePtr(a.ValueDate)
	if a.ValueBool.Valid {
		out.ValueBool = &a.ValueBool.Bool
	}
	out.ValidFrom = pgTimestamptzToTimePtr(a.ValidFrom)
	out.ValidTo = pgTimestamptzToTimePtr(a.ValidTo)
	if a.KnowledgeFrom.Valid {
		out.KnowledgeFrom = a.KnowledgeFrom.Time.UTC()
	}
	out.KnowledgeTo = pgTimestamptzToTimePtr(a.KnowledgeTo)
	if a.ClosureReason.Valid {
		out.ClosureReason = &a.ClosureReason.String
	}
	if a.SupersededBy.Valid {
		id := uuid.UUID(a.SupersededBy.Bytes)
		out.SupersededBy = &id
	}
	if a.TrustTier.Valid {
		out.TrustTier = &a.TrustTier.String
	}
	if a.CreatedAt.Valid {
		out.CreatedAt = a.CreatedAt.Time.UTC()
	}
	return out
}

func convertDbProvenance(p *db.AssertionProvenance) Provenance {
	out := Provenance{
		LocatorHash:     p.LocatorHash,
		SourceKind:      p.SourceKind,
		SourceID:        p.SourceID,
		ProducerKind:    p.ProducerKind,
		ProducerVersion: p.ProducerVersion,
		InputHash:       p.InputHash,
	}
	if p.AssertionID.Valid {
		out.AssertionID = uuid.UUID(p.AssertionID.Bytes)
	}
	if p.Field.Valid {
		out.Field = &p.Field.String
	}
	if p.StartOffset.Valid {
		out.StartOffset = &p.StartOffset.Int32
	}
	if p.EndOffset.Valid {
		out.EndOffset = &p.EndOffset.Int32
	}
	if p.ChunkID.Valid {
		out.ChunkID = &p.ChunkID.String
	}
	if p.Quote.Valid {
		out.Quote = &p.Quote.String
	}
	if p.CreatedAt.Valid {
		out.CreatedAt = p.CreatedAt.Time.UTC()
	}
	return out
}

func dbProvenanceToDomain(rows []*db.AssertionProvenance) []Provenance {
	out := make([]Provenance, 0, len(rows))
	for _, row := range rows {
		out = append(out, convertDbProvenance(row))
	}
	return out
}

func dbAssertionsToDomain(rows []*db.Assertion) []Assertion {
	out := make([]Assertion, 0, len(rows))
	for _, r := range rows {
		out = append(out, convertDbAssertion(r))
	}
	return out
}

func insertAssertionParams(p InsertAssertionParams) db.InsertAssertionParams {
	return db.InsertAssertionParams{
		SubjectNodeID:  uuidToPgUUID(p.SubjectNodeID),
		PredicateKey:   p.PredicateKey,
		ObjectNodeID:   uuidPtrToPgUUID(p.ObjectNodeID),
		ValueText:      stringToPgText(p.ValueText),
		ValueNum:       float64ToPgFloat8(p.ValueNum),
		ValueDate:      timeToPgDate(p.ValueDate),
		ValueBool:      boolToPgBool(p.ValueBool),
		ValidFrom:      timeToPgTimestamptz(p.ValidFrom),
		ValidTo:        timeToPgTimestamptz(p.ValidTo),
		KnowledgeFrom:  pgtype.Timestamptz{Time: p.KnowledgeFrom, Valid: true},
		KnowledgeTo:    timeToPgTimestamptz(p.KnowledgeTo),
		Confidence:     p.Confidence,
		Salience:       p.Salience,
		Status:         p.Status,
		ClosureReason:  stringToPgText(p.ClosureReason),
		SupersededBy:   uuidPtrToPgUUID(p.SupersededBy),
		TrustTier:      stringToPgText(p.TrustTier),
		PropositionKey: p.PropositionKey,
	}
}

// InsertAssertion inserts a new assertion row and returns it.
func (r *AssertionRepository) InsertAssertion(ctx context.Context, p InsertAssertionParams) (*Assertion, error) {
	return insertAssertion(ctx, r.queries, p)
}

// InsertAssertionTx is the tx-bound variant of InsertAssertion.
func (r *AssertionRepository) InsertAssertionTx(ctx context.Context, tx pgx.Tx, p InsertAssertionParams) (*Assertion, error) {
	return insertAssertion(ctx, db.New(tx), p)
}

func insertAssertion(ctx context.Context, q db.Querier, p InsertAssertionParams) (*Assertion, error) {
	row, err := q.InsertAssertion(ctx, insertAssertionParams(p))
	if err != nil {
		return nil, err
	}
	a := convertDbAssertion(row)
	return &a, nil
}

// GetAssertion retrieves an assertion by id (any status).
func (r *AssertionRepository) GetAssertion(ctx context.Context, id uuid.UUID) (*Assertion, error) {
	return getAssertion(ctx, r.queries, id)
}

// GetAssertionTx is the tx-bound variant of GetAssertion.
func (r *AssertionRepository) GetAssertionTx(ctx context.Context, tx pgx.Tx, id uuid.UUID) (*Assertion, error) {
	return getAssertion(ctx, db.New(tx), id)
}

// GetAssertionForUpdateTx reads + row-locks an assertion (SELECT … FOR UPDATE) so
// a lifecycle transition's status-precondition check and the status update are
// atomic within the tx. Tx-only (the lock is held until commit).
func (r *AssertionRepository) GetAssertionForUpdateTx(ctx context.Context, tx pgx.Tx, id uuid.UUID) (*Assertion, error) {
	row, err := db.New(tx).GetAssertionForUpdate(ctx, uuidToPgUUID(id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, err
	}
	a := convertDbAssertion(row)
	return &a, nil
}

func getAssertion(ctx context.Context, q db.Querier, id uuid.UUID) (*Assertion, error) {
	row, err := q.GetAssertion(ctx, uuidToPgUUID(id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, err
	}
	a := convertDbAssertion(row)
	return &a, nil
}

// FindLiveProposition returns the single LIVE assertion for a proposition_key
// (the dedup lookup); ErrNotFound when none is live.
func (r *AssertionRepository) FindLiveProposition(ctx context.Context, propositionKey string) (*Assertion, error) {
	return findLiveProposition(ctx, r.queries, propositionKey)
}

// FindLivePropositionTx is the tx-bound variant of FindLiveProposition.
func (r *AssertionRepository) FindLivePropositionTx(ctx context.Context, tx pgx.Tx, propositionKey string) (*Assertion, error) {
	return findLiveProposition(ctx, db.New(tx), propositionKey)
}

func findLiveProposition(ctx context.Context, q db.Querier, propositionKey string) (*Assertion, error) {
	row, err := q.FindLiveProposition(ctx, propositionKey)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, err
	}
	a := convertDbAssertion(row)
	return &a, nil
}

// FindAcceptedForSlotTx returns the accepted, knowledge-open assertions for an
// ASYMMETRIC (subject, predicate) slot whose valid-time overlaps the new row's
// effective window. effectiveFrom is the caller-computed COALESCE(valid_from,
// now); newValidTo is the new row's valid_to (nil = open-ended). Rows are locked
// FOR UPDATE, so it is tx-only (the lock is held for the rest of the tx).
func (r *AssertionRepository) FindAcceptedForSlotTx(ctx context.Context, tx pgx.Tx, subjectNodeID uuid.UUID, predicateKey string, effectiveFrom time.Time, newValidTo *time.Time) ([]Assertion, error) {
	rows, err := db.New(tx).FindAcceptedForSlot(ctx, db.FindAcceptedForSlotParams{
		SubjectNodeID: uuidToPgUUID(subjectNodeID),
		PredicateKey:  predicateKey,
		EffectiveFrom: pgtype.Timestamptz{Time: effectiveFrom, Valid: true},
		NewValidTo:    timeToPgTimestamptz(newValidTo),
	})
	if err != nil {
		return nil, err
	}
	return dbAssertionsToDomain(rows), nil
}

// FindAcceptedForSlotSymmetricTx returns the accepted, knowledge-open SYMMETRIC
// edges where either participant appears in either position and whose valid-time
// overlaps the new row's effective window. Rows are locked FOR UPDATE.
func (r *AssertionRepository) FindAcceptedForSlotSymmetricTx(ctx context.Context, tx pgx.Tx, predicateKey string, participantA, participantB uuid.UUID, effectiveFrom time.Time, newValidTo *time.Time) ([]Assertion, error) {
	rows, err := db.New(tx).FindAcceptedForSlotSymmetric(ctx, db.FindAcceptedForSlotSymmetricParams{
		PredicateKey:  predicateKey,
		ParticipantA:  uuidToPgUUID(participantA),
		ParticipantB:  uuidToPgUUID(participantB),
		EffectiveFrom: pgtype.Timestamptz{Time: effectiveFrom, Valid: true},
		NewValidTo:    timeToPgTimestamptz(newValidTo),
	})
	if err != nil {
		return nil, err
	}
	return dbAssertionsToDomain(rows), nil
}

// CloseAssertion terminalizes an assertion (present-successor / closure-only /
// retract / rollover paths).
func (r *AssertionRepository) CloseAssertion(ctx context.Context, p CloseAssertionParams) error {
	return closeAssertion(ctx, r.queries, p)
}

// CloseAssertionTx is the tx-bound variant of CloseAssertion.
func (r *AssertionRepository) CloseAssertionTx(ctx context.Context, tx pgx.Tx, p CloseAssertionParams) error {
	return closeAssertion(ctx, db.New(tx), p)
}

func closeAssertion(ctx context.Context, q db.Querier, p CloseAssertionParams) error {
	return q.CloseAssertion(ctx, db.CloseAssertionParams{
		ID:            uuidToPgUUID(p.ID),
		ValidTo:       timeToPgTimestamptz(p.ValidTo),
		Status:        p.Status,
		ClosureReason: stringToPgText(p.ClosureReason),
		SupersededBy:  uuidPtrToPgUUID(p.SupersededBy),
		KnowledgeTo:   timeToPgTimestamptz(p.KnowledgeTo),
	})
}

// BoundPendingSuccessorTx bounds a prior's valid_to and points it at a pending
// future successor while keeping it accepted/knowledge-open.
func (r *AssertionRepository) BoundPendingSuccessorTx(ctx context.Context, tx pgx.Tx, id uuid.UUID, validTo time.Time, supersededBy uuid.UUID) error {
	return db.New(tx).BoundPendingSuccessor(ctx, db.BoundPendingSuccessorParams{
		ID:           uuidToPgUUID(id),
		ValidTo:      pgtype.Timestamptz{Time: validTo, Valid: true},
		SupersededBy: uuidToPgUUID(supersededBy),
	})
}

// SetAssertionPendingSuccessorTx stamps superseded_by on a still-accepted
// survivor that inherited a merged stint's pending future successor, so the
// rollover sweep terminalizes it at the bound.
func (r *AssertionRepository) SetAssertionPendingSuccessorTx(ctx context.Context, tx pgx.Tx, id, successorID uuid.UUID) error {
	return db.New(tx).SetAssertionPendingSuccessor(ctx, db.SetAssertionPendingSuccessorParams{
		ID:           uuidToPgUUID(id),
		SupersededBy: uuidToPgUUID(successorID),
	})
}

// WidenAssertionValidityTx widens an accepted row's valid window to cover new
// evidence and sets the recomputed proposition_key.
func (r *AssertionRepository) WidenAssertionValidityTx(ctx context.Context, tx pgx.Tx, id uuid.UUID, validFrom, validTo *time.Time, propositionKey string) error {
	return db.New(tx).WidenAssertionValidity(ctx, db.WidenAssertionValidityParams{
		ID:             uuidToPgUUID(id),
		ValidFrom:      timeToPgTimestamptz(validFrom),
		ValidTo:        timeToPgTimestamptz(validTo),
		PropositionKey: propositionKey,
	})
}

// RolloverDueBoundedSuccessors terminalizes the bounded-with-pending-successor
// rows whose bound has passed (knowledgeTo = now), returning the updated rows so
// the caller can emit one assertion.superseded event per row.
func (r *AssertionRepository) RolloverDueBoundedSuccessors(ctx context.Context, knowledgeTo time.Time) ([]Assertion, error) {
	return rolloverDueBoundedSuccessors(ctx, r.queries, knowledgeTo)
}

// RolloverDueBoundedSuccessorsTx is the tx-bound variant of
// RolloverDueBoundedSuccessors (the rollover worker runs the terminal flip + its
// per-row event publish in one tx).
func (r *AssertionRepository) RolloverDueBoundedSuccessorsTx(ctx context.Context, tx pgx.Tx, knowledgeTo time.Time) ([]Assertion, error) {
	return rolloverDueBoundedSuccessors(ctx, db.New(tx), knowledgeTo)
}

func rolloverDueBoundedSuccessors(ctx context.Context, q db.Querier, knowledgeTo time.Time) ([]Assertion, error) {
	rows, err := q.RolloverDueBoundedSuccessors(ctx, pgtype.Timestamptz{Time: knowledgeTo, Valid: true})
	if err != nil {
		return nil, err
	}
	return dbAssertionsToDomain(rows), nil
}

// TransitionStatusTx applies an accept/reject/retract status transition.
// knowledgeTo + closureReason are nil for a non-terminal transition.
func (r *AssertionRepository) TransitionStatusTx(ctx context.Context, tx pgx.Tx, id uuid.UUID, status string, knowledgeTo *time.Time, closureReason *string) error {
	return db.New(tx).TransitionStatus(ctx, db.TransitionStatusParams{
		ID:            uuidToPgUUID(id),
		Status:        status,
		KnowledgeTo:   timeToPgTimestamptz(knowledgeTo),
		ClosureReason: stringToPgText(closureReason),
	})
}

// GetCurrentAccepted returns the current-accepted assertion for a slot (accepted,
// knowledge-open, valid-time window contains now); ErrNotFound when there is no
// current value (a gap).
func (r *AssertionRepository) GetCurrentAccepted(ctx context.Context, subjectNodeID uuid.UUID, predicateKey string, now time.Time) (*Assertion, error) {
	return getCurrentAccepted(ctx, r.queries, subjectNodeID, predicateKey, now)
}

// GetCurrentAcceptedTx is the tx-bound variant of GetCurrentAccepted.
func (r *AssertionRepository) GetCurrentAcceptedTx(ctx context.Context, tx pgx.Tx, subjectNodeID uuid.UUID, predicateKey string, now time.Time) (*Assertion, error) {
	return getCurrentAccepted(ctx, db.New(tx), subjectNodeID, predicateKey, now)
}

func getCurrentAccepted(ctx context.Context, q db.Querier, subjectNodeID uuid.UUID, predicateKey string, now time.Time) (*Assertion, error) {
	row, err := q.GetCurrentAccepted(ctx, db.GetCurrentAcceptedParams{
		SubjectNodeID: uuidToPgUUID(subjectNodeID),
		PredicateKey:  predicateKey,
		Now:           pgtype.Timestamptz{Time: now, Valid: true},
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, err
	}
	a := convertDbAssertion(row)
	return &a, nil
}

// ListAssertionsBySubject returns all assertions for a subject node (any status),
// newest first.
func (r *AssertionRepository) ListAssertionsBySubject(ctx context.Context, subjectNodeID uuid.UUID) ([]Assertion, error) {
	rows, err := r.queries.ListAssertionsBySubject(ctx, uuidToPgUUID(subjectNodeID))
	if err != nil {
		return nil, err
	}
	return dbAssertionsToDomain(rows), nil
}

// ListAssertionsTouchingNodeTx returns all assertions touching a node in either
// position (subject OR object), any status, oldest first. The node-merge
// procedure uses it to find every loser-referencing row to re-point onto the
// winner. Tx-bound so it sees rows the same merge tx wrote earlier.
func (r *AssertionRepository) ListAssertionsTouchingNodeTx(ctx context.Context, tx pgx.Tx, nodeID uuid.UUID) ([]Assertion, error) {
	rows, err := db.New(tx).ListAssertionsTouchingNode(ctx, uuidToPgUUID(nodeID))
	if err != nil {
		return nil, err
	}
	return dbAssertionsToDomain(rows), nil
}

// ListLiveEdgesForNode returns live edges of a predicate touching a node in
// either orientation (the symmetric two-direction read).
func (r *AssertionRepository) ListLiveEdgesForNode(ctx context.Context, nodeID uuid.UUID, predicateKey string) ([]Assertion, error) {
	rows, err := r.queries.ListLiveEdgesForNode(ctx, db.ListLiveEdgesForNodeParams{
		SubjectNodeID: uuidToPgUUID(nodeID),
		PredicateKey:  predicateKey,
	})
	if err != nil {
		return nil, err
	}
	return dbAssertionsToDomain(rows), nil
}

// RepointAssertionSubjectTx repoints a loser assertion's subject to the winner
// and sets the recomputed proposition_key (merge primitive).
func (r *AssertionRepository) RepointAssertionSubjectTx(ctx context.Context, tx pgx.Tx, id, subjectNodeID uuid.UUID, propositionKey string) error {
	return db.New(tx).RepointAssertionSubject(ctx, db.RepointAssertionSubjectParams{
		ID:             uuidToPgUUID(id),
		SubjectNodeID:  uuidToPgUUID(subjectNodeID),
		PropositionKey: propositionKey,
	})
}

// RepointAssertionObjectTx repoints a loser assertion's object to the winner and
// sets the recomputed proposition_key (merge primitive).
func (r *AssertionRepository) RepointAssertionObjectTx(ctx context.Context, tx pgx.Tx, id, objectNodeID uuid.UUID, propositionKey string) error {
	return db.New(tx).RepointAssertionObject(ctx, db.RepointAssertionObjectParams{
		ID:             uuidToPgUUID(id),
		ObjectNodeID:   uuidToPgUUID(objectNodeID),
		PropositionKey: propositionKey,
	})
}

// UpdateAssertionConfidenceTrustTx re-aggregates confidence + trust_tier on a
// corroborating write. trustTier is nil to leave it NULL.
func (r *AssertionRepository) UpdateAssertionConfidenceTrustTx(ctx context.Context, tx pgx.Tx, id uuid.UUID, confidence int16, trustTier *string) error {
	return db.New(tx).UpdateAssertionConfidenceTrust(ctx, db.UpdateAssertionConfidenceTrustParams{
		ID:         uuidToPgUUID(id),
		Confidence: confidence,
		TrustTier:  stringToPgText(trustTier),
	})
}

// AcquirePropositionSlotLockTx takes the transaction-scoped advisory lock guarding
// a single-cardinality slot. The caller passes the Go-computed int64 slot key; the
// lock auto-releases at tx end.
func (r *AssertionRepository) AcquirePropositionSlotLockTx(ctx context.Context, tx pgx.Tx, slotKey int64) error {
	return db.New(tx).AcquirePropositionSlotLock(ctx, slotKey)
}

// InsertProvenance appends a corroborating locator. Returns true when a row was
// actually inserted (false on the ON CONFLICT no-op), so the write API knows
// whether to emit a provenance_added event.
func (r *AssertionRepository) InsertProvenance(ctx context.Context, p InsertProvenanceParams) (bool, error) {
	return insertProvenance(ctx, r.queries, p)
}

// InsertProvenanceTx is the tx-bound variant of InsertProvenance.
func (r *AssertionRepository) InsertProvenanceTx(ctx context.Context, tx pgx.Tx, p InsertProvenanceParams) (bool, error) {
	return insertProvenance(ctx, db.New(tx), p)
}

func insertProvenance(ctx context.Context, q db.Querier, p InsertProvenanceParams) (bool, error) {
	affected, err := q.InsertProvenance(ctx, db.InsertProvenanceParams{
		AssertionID:     uuidToPgUUID(p.AssertionID),
		LocatorHash:     p.LocatorHash,
		SourceKind:      p.SourceKind,
		SourceID:        p.SourceID,
		ProducerKind:    p.ProducerKind,
		ProducerVersion: p.ProducerVersion,
		Field:           stringToPgText(p.Field),
		StartOffset:     int32ToPgInt4(p.StartOffset),
		EndOffset:       int32ToPgInt4(p.EndOffset),
		ChunkID:         stringToPgText(p.ChunkID),
		InputHash:       p.InputHash,
		Quote:           stringToPgText(p.Quote),
	})
	if err != nil {
		return false, err
	}
	return affected > 0, nil
}

// ListProvenance returns all locators for an assertion, oldest first.
func (r *AssertionRepository) ListProvenance(ctx context.Context, assertionID uuid.UUID) ([]Provenance, error) {
	return listProvenance(ctx, r.queries, assertionID)
}

// ListProvenanceTx is the tx-bound variant of ListProvenance. The merge path uses
// it so it sees a loser assertion's provenance written earlier in the SAME tx
// (e.g. AssertTx then AcceptTx in one tx).
func (r *AssertionRepository) ListProvenanceTx(ctx context.Context, tx pgx.Tx, assertionID uuid.UUID) ([]Provenance, error) {
	return listProvenance(ctx, db.New(tx), assertionID)
}

func listProvenance(ctx context.Context, q db.Querier, assertionID uuid.UUID) ([]Provenance, error) {
	rows, err := q.ListProvenance(ctx, uuidToPgUUID(assertionID))
	if err != nil {
		return nil, err
	}
	return dbProvenanceToDomain(rows), nil
}

// ListProvenanceBySource is the reverse lookup: every locator a given source
// produced (via the (source_kind, source_id) index). Backs the source-row-
// deletion sweep.
func (r *AssertionRepository) ListProvenanceBySource(ctx context.Context, sourceKind, sourceID string) ([]Provenance, error) {
	rows, err := r.queries.ListProvenanceBySource(ctx, db.ListProvenanceBySourceParams{
		SourceKind: sourceKind,
		SourceID:   sourceID,
	})
	if err != nil {
		return nil, err
	}
	return dbProvenanceToDomain(rows), nil
}

// DeleteProvenanceLocatorTx drops a single locator (re-extraction retirement).
func (r *AssertionRepository) DeleteProvenanceLocatorTx(ctx context.Context, tx pgx.Tx, assertionID uuid.UUID, locatorHash string) error {
	return db.New(tx).DeleteProvenanceLocator(ctx, db.DeleteProvenanceLocatorParams{
		AssertionID: uuidToPgUUID(assertionID),
		LocatorHash: locatorHash,
	})
}

// SourceKindRequiresExistenceCheck reports whether a provenance source_kind has a
// backing content table whose row must be confirmed to exist at write time. The
// content kinds do; user / agent_session / anarlog_transcript do NOT (their
// locators are non-UUID refs or a table that does not exist yet). The write API
// gates on this BEFORE calling ExistsContentRow, so a "no-check" kind is never
// confused with a "row missing" result.
func SourceKindRequiresExistenceCheck(sourceKind string) bool {
	switch sourceKind {
	case SourceKindCommsMessage, SourceKindTelegramMessage, SourceKindMessagesMessage,
		SourceKindMeetingNote, SourceKindCalendarEvent, SourceKindPhoneCall:
		return true
	default:
		return false
	}
}

// ExistsContentRow checks whether the content row referenced by a provenance
// locator exists, dispatching on sourceKind. It is defined ONLY for the content
// kinds (those SourceKindRequiresExistenceCheck returns true for); a no-check
// kind (user / agent_session / anarlog_transcript) or an unknown kind returns an
// error rather than a silent false, so the caller cannot misread "no check
// performed" as "row missing" — gate on SourceKindRequiresExistenceCheck first.
func (r *AssertionRepository) ExistsContentRow(ctx context.Context, sourceKind string, id uuid.UUID) (bool, error) {
	return existsContentRow(ctx, r.queries, sourceKind, id)
}

// ExistsContentRowTx is the tx-bound variant of ExistsContentRow. The write API
// uses it so the existence check runs inside the assert tx — it sees a content row
// created earlier in the same tx and is transactionally consistent with a
// concurrent soft delete.
func (r *AssertionRepository) ExistsContentRowTx(ctx context.Context, tx pgx.Tx, sourceKind string, id uuid.UUID) (bool, error) {
	return existsContentRow(ctx, db.New(tx), sourceKind, id)
}

func existsContentRow(ctx context.Context, q db.Querier, sourceKind string, id uuid.UUID) (bool, error) {
	pgID := uuidToPgUUID(id)
	switch sourceKind {
	case SourceKindCommsMessage:
		return q.ExistsCommsMessage(ctx, pgID)
	case SourceKindTelegramMessage:
		return q.ExistsTelegramMessage(ctx, pgID)
	case SourceKindMessagesMessage:
		return q.ExistsMessagesMessage(ctx, pgID)
	case SourceKindMeetingNote:
		return q.ExistsMeetingNote(ctx, pgID)
	case SourceKindCalendarEvent:
		return q.ExistsCalendarEvent(ctx, pgID)
	case SourceKindPhoneCall:
		return q.ExistsPhoneCall(ctx, pgID)
	default:
		return false, errors.New("ExistsContentRow called for a source kind with no backing-row check: " + sourceKind)
	}
}
