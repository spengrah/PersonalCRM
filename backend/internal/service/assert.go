package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/fnv"
	"sort"
	"strconv"
	"strings"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrAssertValidation is the sentinel a write rejection wraps. The write API
// REJECTS (returns an error) rather than silently dropping a bad assertion, so
// callers can distinguish a validation failure (4xx-shaped) from an
// infrastructure error.
var ErrAssertValidation = errors.New("assert: validation failed")

// validationError wraps a human-readable reason in ErrAssertValidation.
func validationError(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrAssertValidation, fmt.Sprintf(format, args...))
}

// ProvenanceLocator is one corroborating source locator supplied with an assert
// request. The write API computes its locator_hash (D5) and validates the
// source_kind + (for content kinds) the referenced row's existence.
type ProvenanceLocator struct {
	SourceKind      string  // closed enum (repository.SourceKind*)
	SourceID        string  // content-row UUID-as-text, or a stable ref for user/agent_session
	ProducerKind    string  // extractor | agent | user
	ProducerVersion string  // re-verifiability; "" allowed
	Field           *string // which column/field the quote came from
	StartOffset     *int32
	EndOffset       *int32
	ChunkID         *string
	InputHash       string // extractor input hash; "" allowed
	Quote           *string
}

// AssertRequest is the input to Assert. Exactly one of {ObjectNodeID, ValueText,
// ValueNum, ValueDate, ValueBool} should be set per the predicate's kind (the
// validator enforces it). ValidFrom/ValidTo are world-truth bounds from content
// evidence (nil = open-ended); knowledge-time is always now (KnowledgeFromOverride
// is the migration/import escape hatch).
type AssertRequest struct {
	SubjectNodeID uuid.UUID
	PredicateKey  string

	ObjectNodeID *uuid.UUID
	ValueText    *string
	ValueNum     *float64
	ValueDate    *time.Time
	ValueBool    *bool

	ValidFrom *time.Time
	ValidTo   *time.Time

	Confidence int16
	// Salience defaults to the predicate's default_salience when zero-and-unset;
	// callers that want a non-default salience set SalienceOverride.
	SalienceOverride *int16

	Locators []ProvenanceLocator

	// ForceConfirm routes an otherwise auto-apply predicate to 'proposed' (a
	// human/agent wants to review it before it lands accepted).
	ForceConfirm bool

	// KnowledgeFromOverride preserves historical knowledge-time for a backfill /
	// migration (the source row's created_at). Live producers leave it nil →
	// knowledge_from = now. This is the ONLY caller permitted to override.
	KnowledgeFromOverride *time.Time
}

// ClosureRequest closes a single-cardinality slot with no successor ("between
// jobs"): the current accepted assertion for (subject, predicate) is closed
// 'ended' and the current value becomes a gap.
type ClosureRequest struct {
	SubjectNodeID uuid.UUID
	PredicateKey  string
}

// AcceptRequest carries the optional knobs for Accept. Empty is the common case.
type AcceptRequest struct{}

// RejectRequest carries the optional knobs for Reject.
type RejectRequest struct{}

// RetractRequest carries the optional knobs for Retract.
type RetractRequest struct{}

// AssertService is the single validated write path for the assertion store (D6).
// Every producer (extractor, agent, UI, migration) asserts facts/edges through
// it; it owns proposition identity, cardinality/supersession, the bi-temporal
// clocks, provenance dedup, and event emission — all in one transaction.
type AssertService struct {
	pool          *pgxpool.Pool
	nodeRepo      *repository.NodeRepository
	entityRepo    *repository.EntityRepository
	predicateRepo *repository.PredicateRepository
	assertionRepo *repository.AssertionRepository
	bus           busPublisher
}

// busPublisher is the narrow event-bus surface AssertService needs. *events.Bus
// satisfies it. Nil-safe is NOT supported — the write contract requires events.
type busPublisher interface {
	PublishTx(ctx context.Context, tx pgx.Tx, env *events.Envelope) error
}

// NewAssertService wires the write API over the pool + graph repos + bus.
func NewAssertService(
	pool *pgxpool.Pool,
	nodeRepo *repository.NodeRepository,
	entityRepo *repository.EntityRepository,
	predicateRepo *repository.PredicateRepository,
	assertionRepo *repository.AssertionRepository,
	bus busPublisher,
) *AssertService {
	return &AssertService{
		pool:          pool,
		nodeRepo:      nodeRepo,
		entityRepo:    entityRepo,
		predicateRepo: predicateRepo,
		assertionRepo: assertionRepo,
		bus:           bus,
	}
}

// --------------------------------------------------------------------------
// Pure helpers — deterministic encoding, proposition identity, locator hash.
// These take no DB and are unit-tested in isolation (assert_propkey_test.go).
// --------------------------------------------------------------------------

// encodeLengthPrefixed joins components as "<len>:<value>" per field, so a
// delimiter or NUL byte inside any component cannot forge a collision across
// components (a plain "|"-join could: a value_text containing "|" would alias a
// different tuple). This is the SAME encoding both proposition_key and
// locator_hash use (D4a / D5).
func encodeLengthPrefixed(components ...string) string {
	var b strings.Builder
	for _, c := range components {
		b.WriteString(strconv.Itoa(len(c)))
		b.WriteByte(':')
		b.WriteString(c)
	}
	return b.String()
}

// canonicalEdge returns the canonical (predicateKey, subject, object) for an
// edge, applying symmetric pair-ordering and inverse-predicate token+orientation
// canonicalization (D4a). For a fact (no object) it returns the inputs unchanged.
//
//   - symmetric: store once with subject<=object in UUID byte order.
//   - inverse pair: the canonical predicate is min(key, inverse); if the incoming
//     predicate is NOT the canonical one, swap subject<->object and use the
//     canonical key. So parent_of(A,B) and child_of(B,A) collapse to one row.
//
// object is nil for a fact predicate (returned nil). predicate is the loaded
// catalog row for predicateKey.
func canonicalEdge(predicate *repository.Predicate, subject uuid.UUID, object *uuid.UUID) (canonKey string, canonSubject uuid.UUID, canonObject *uuid.UUID) {
	canonKey = predicate.Key
	canonSubject = subject
	canonObject = object

	if object == nil {
		return canonKey, canonSubject, canonObject
	}

	// Inverse canonicalization: rewrite to the lexicographically-smaller key's
	// direction. min(key, inverse) is the canonical predicate. The swap uses
	// fresh locals (NOT &canonSubject) to avoid a self-aliasing pointer.
	if predicate.InversePredicate != nil && *predicate.InversePredicate < predicate.Key {
		canonKey = *predicate.InversePredicate
		newSubject := *object
		newObject := subject
		canonSubject = newSubject
		canonObject = &newObject
	}

	// Symmetric ordering: store the pair subject<=object (UUID byte order). Apply
	// AFTER inverse rewrite (a symmetric predicate has no inverse, so the two are
	// mutually exclusive in the seed catalog, but ordering last is harmless).
	if predicate.Symmetric && canonObject != nil {
		if bytesGreater(canonSubject, *canonObject) {
			newSubject := *canonObject
			newObject := canonSubject
			canonSubject = newSubject
			canonObject = &newObject
		}
	}
	return canonKey, canonSubject, canonObject
}

// bytesGreater reports whether a > b in UUID byte order.
func bytesGreater(a, b uuid.UUID) bool {
	for i := 0; i < len(a); i++ {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return false
}

// normalizeValue returns the canonical string form of the assertion's payload
// for the proposition key (D4a step 3): text → lower(trim()); date → ISO; num →
// canonical float repr; bool → "t"/"f". Exactly one of the value fields is set
// for a fact; for an edge the object UUID is used (handled by the caller).
func normalizeValue(req *AssertRequest) string {
	switch {
	case req.ValueText != nil:
		return strings.ToLower(strings.TrimSpace(*req.ValueText))
	case req.ValueNum != nil:
		// strconv.FormatFloat with -1 precision is the shortest round-trippable
		// repr, deterministic for a given float64.
		return strconv.FormatFloat(*req.ValueNum, 'g', -1, 64)
	case req.ValueDate != nil:
		return req.ValueDate.UTC().Format("2006-01-02")
	case req.ValueBool != nil:
		if *req.ValueBool {
			return "t"
		}
		return "f"
	default:
		return ""
	}
}

// validTimeBucket returns the valid-time component of the proposition key for a
// predicate's proposition_bucket granularity (D4a). 'none' → no valid-time
// component (""); otherwise validFrom is truncated to day/month/year in UTC (or
// the literal "open" when validFrom is nil). Computed in Go (immutable,
// timezone-pinned to UTC) — never the brittle SQL date_trunc.
func validTimeBucket(bucket string, validFrom *time.Time) string {
	if bucket == repository.PredicateBucketNone {
		return ""
	}
	if validFrom == nil {
		return "open"
	}
	u := validFrom.UTC()
	switch bucket {
	case repository.PredicateBucketYear:
		return strconv.Itoa(u.Year())
	case repository.PredicateBucketMonth:
		return u.Format("2006-01")
	default: // day
		return u.Format("2006-01-02")
	}
}

// computePropositionKey builds the deterministic dedup key (D4a) from the
// CANONICAL (subject, predicate, object/value, valid-time-bucket). The caller
// passes the already-canonicalized key/subject/object (from canonicalEdge) plus
// the predicate (for the bucket granularity + edge-vs-fact discrimination) and
// the request (for the fact value + valid_from). Length-prefixed so no component
// can alias another.
func computePropositionKey(predicate *repository.Predicate, canonKey string, canonSubject uuid.UUID, canonObject *uuid.UUID, req *AssertRequest) string {
	var normalized string
	if canonObject != nil {
		normalized = canonObject.String()
	} else {
		normalized = normalizeValue(req)
	}
	bucket := validTimeBucket(predicate.PropositionBucket, req.ValidFrom)
	return encodeLengthPrefixed(
		canonSubject.String(),
		canonKey,
		normalized,
		bucket,
	)
}

// computeLocatorHash is the sha256-hex over the length-prefixed encoding of the
// full locator identity (D5): (source_kind, source_id, field, start_offset,
// end_offset, chunk_id, producer_kind, producer_version, input_hash). A
// same-locator re-emit hashes identically (→ ON CONFLICT no-op); a different
// span/version hashes differently (→ a new corroborating row).
func computeLocatorHash(loc ProvenanceLocator) string {
	encoded := encodeLengthPrefixed(
		loc.SourceKind,
		loc.SourceID,
		strPtrOrEmpty(loc.Field),
		int32PtrToString(loc.StartOffset),
		int32PtrToString(loc.EndOffset),
		strPtrOrEmpty(loc.ChunkID),
		loc.ProducerKind,
		loc.ProducerVersion,
		loc.InputHash,
	)
	sum := sha256.Sum256([]byte(encoded))
	return hex.EncodeToString(sum[:])
}

func strPtrOrEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func int32PtrToString(v *int32) string {
	if v == nil {
		return ""
	}
	return strconv.FormatInt(int64(*v), 10)
}

// slotLockKey folds a single-cardinality slot identity into a deterministic int64
// for pg_advisory_xact_lock (D6 step 4). FNV-64a is stable across the process and
// the only requirement is that all asserts touching the SAME slot fold to the
// SAME int64 (collisions across DIFFERENT slots merely over-serialize, never
// corrupt). Asymmetric slot = (subject, canonical predicate); a symmetric-single
// write takes one lock per participant, so this is called once per participant.
func slotLockKey(predicateKey string, participant uuid.UUID) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(encodeLengthPrefixed(predicateKey, participant.String())))
	// fnv.Sum64 is uint64; reinterpret the bits as int64 (pg_advisory_xact_lock
	// takes a signed bigint — the full 64-bit space is valid).
	return int64(h.Sum64()) //nolint:gosec // intentional bit-reinterpretation
}

// --------------------------------------------------------------------------
// Lifecycle entry points.
// --------------------------------------------------------------------------

// Assert proposes-or-corroborates a fact/edge in its own transaction. Returns the
// resulting assertion (the new row, or the existing one a corroboration appended
// to / accepted). A validation failure REJECTS the write (wraps
// ErrAssertValidation); the tx rolls back.
func (s *AssertService) Assert(ctx context.Context, req AssertRequest) (a *repository.Assertion, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() {
		if rb := tx.Rollback(ctx); rb != nil && !errors.Is(rb, pgx.ErrTxClosed) && err == nil {
			err = rb
		}
	}()
	a, err = s.AssertTx(ctx, tx, req)
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return a, nil
}

// AssertTx runs the 6-step write contract (D6) within the caller's tx. The caller
// owns commit/rollback (this is how SP9's contact-create dual-emits assertions in
// the contact tx). It NEVER commits or rolls back the tx itself.
func (s *AssertService) AssertTx(ctx context.Context, tx pgx.Tx, req AssertRequest) (*repository.Assertion, error) {
	// Step 1: validate against the catalog + nodes + provenance.
	predicate, canonKey, canonSubject, canonObject, err := s.validate(ctx, tx, &req)
	if err != nil {
		return nil, err
	}

	now := accelerated.GetCurrentTime().UTC()
	knowledgeFrom := now
	if req.KnowledgeFromOverride != nil {
		knowledgeFrom = req.KnowledgeFromOverride.UTC()
	}

	// Step 2: compute the proposition key + per-locator hashes.
	propKey := computePropositionKey(predicate, canonKey, canonSubject, canonObject, &req)

	// Determine the landing status (auto-apply vs always-confirm / force-confirm).
	landingAccepted := predicate.DefaultReviewPolicy == repository.PredicateReviewAutoIfConfident && !req.ForceConfirm

	// Step 3: resolve proposition identity (dedup).
	existing, err := s.assertionRepo.FindLivePropositionTx(ctx, tx, propKey)
	if err != nil && !errors.Is(err, db.ErrNotFound) {
		return nil, fmt.Errorf("find live proposition: %w", err)
	}
	if err == nil {
		// Match → corroborate (append provenance, re-aggregate, maybe accept).
		return s.corroborate(ctx, tx, predicate, existing, &req, landingAccepted, now)
	}

	// No match → step 4/5: cardinality/supersession + write the new row.
	return s.writeNew(ctx, tx, predicate, canonKey, canonSubject, canonObject, propKey, &req, landingAccepted, knowledgeFrom, now)
}

// --------------------------------------------------------------------------
// Step 1: validation (D6). Any failure REJECTS the write (never silently drops).
// Returns the loaded predicate + the canonicalized (key, subject, object).
// --------------------------------------------------------------------------

func (s *AssertService) validate(ctx context.Context, tx pgx.Tx, req *AssertRequest) (predicate *repository.Predicate, canonKey string, canonSubject uuid.UUID, canonObject *uuid.UUID, err error) {
	if req.SubjectNodeID == uuid.Nil {
		return nil, "", uuid.Nil, nil, validationError("subject_node_id is required")
	}

	// Predicate exists in the catalog.
	predicate, err = s.predicateRepo.GetPredicate(ctx, req.PredicateKey)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return nil, "", uuid.Nil, nil, validationError("unknown predicate %q", req.PredicateKey)
		}
		return nil, "", uuid.Nil, nil, fmt.Errorf("load predicate: %w", err)
	}

	// Subject node exists, is not soft-deleted, and its type/entity-subtype matches
	// predicate.subject_type. The validator accepts an entity-subtype subject (e.g.
	// `within` place→place), not only person.
	if err := s.validateSubjectType(ctx, tx, req.SubjectNodeID, predicate); err != nil {
		return nil, "", uuid.Nil, nil, err
	}

	// Payload shape: edge vs fact (the cross-table "object iff edge" invariant the
	// column CHECK can't express).
	switch predicate.Kind {
	case repository.PredicateKindEdge:
		if req.ObjectNodeID == nil {
			return nil, "", uuid.Nil, nil, validationError("edge predicate %q requires object_node_id", predicate.Key)
		}
		if hasAnyScalar(req) {
			return nil, "", uuid.Nil, nil, validationError("edge predicate %q must not carry a scalar value", predicate.Key)
		}
		// Object node exists, not soft-deleted, type matches object_type.
		if err := s.validateObjectType(ctx, tx, *req.ObjectNodeID, predicate); err != nil {
			return nil, "", uuid.Nil, nil, err
		}
	case repository.PredicateKindFact:
		if req.ObjectNodeID != nil {
			return nil, "", uuid.Nil, nil, validationError("fact predicate %q must not carry object_node_id", predicate.Key)
		}
		if err := validateFactValue(req, predicate); err != nil {
			return nil, "", uuid.Nil, nil, err
		}
	default:
		return nil, "", uuid.Nil, nil, validationError("predicate %q has unknown kind %q", predicate.Key, predicate.Kind)
	}

	// Producer + confidence + provenance.
	if req.Confidence < 0 || req.Confidence > 100 {
		return nil, "", uuid.Nil, nil, validationError("confidence %d out of range [0,100]", req.Confidence)
	}
	if len(req.Locators) == 0 {
		return nil, "", uuid.Nil, nil, validationError("at least one provenance locator is required")
	}
	if err := s.validateLocators(ctx, tx, predicate, req.Locators); err != nil {
		return nil, "", uuid.Nil, nil, err
	}

	canonKey, canonSubject, canonObject = canonicalEdge(predicate, req.SubjectNodeID, req.ObjectNodeID)
	return predicate, canonKey, canonSubject, canonObject, nil
}

// validateSubjectType confirms the subject node exists (live) and its type /
// entity-subtype matches predicate.subject_type.
func (s *AssertService) validateSubjectType(ctx context.Context, tx pgx.Tx, subjectID uuid.UUID, predicate *repository.Predicate) error {
	node, err := s.nodeRepo.GetNodeTx(ctx, tx, subjectID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return validationError("subject node %s does not exist or is soft-deleted", subjectID)
		}
		return fmt.Errorf("load subject node: %w", err)
	}
	return s.matchNodeType(ctx, tx, node, predicate.SubjectType, "subject")
}

// validateObjectType confirms the object node exists (live) and its type /
// entity-subtype matches predicate.object_type.
func (s *AssertService) validateObjectType(ctx context.Context, tx pgx.Tx, objectID uuid.UUID, predicate *repository.Predicate) error {
	if predicate.ObjectType == nil {
		return validationError("edge predicate %q has no object_type in the catalog", predicate.Key)
	}
	node, err := s.nodeRepo.GetNodeTx(ctx, tx, objectID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return validationError("object node %s does not exist or is soft-deleted", objectID)
		}
		return fmt.Errorf("load object node: %w", err)
	}
	return s.matchNodeType(ctx, tx, node, *predicate.ObjectType, "object")
}

// matchNodeType checks a node against an expected predicate type token. The token
// is 'person'/'venue' (a node type) OR an entity subtype (e.g. 'place', 'tag',
// 'organization', 'topic'). For an entity node the subtype is resolved from the
// entity row.
func (s *AssertService) matchNodeType(ctx context.Context, tx pgx.Tx, node *repository.Node, expected, role string) error {
	switch node.Type {
	case repository.NodeTypePerson, repository.NodeTypeVenue:
		if node.Type != expected {
			return validationError("%s node type %q does not match predicate %s_type %q", role, node.Type, role, expected)
		}
		return nil
	case repository.NodeTypeEntity:
		entity, err := s.entityRepo.GetEntityTx(ctx, tx, node.ID)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				return validationError("%s entity node %s has no entity row", role, node.ID)
			}
			return fmt.Errorf("load %s entity: %w", role, err)
		}
		if entity.Subtype != expected {
			return validationError("%s entity subtype %q does not match predicate %s_type %q", role, entity.Subtype, role, expected)
		}
		return nil
	default:
		return validationError("%s node %s has unknown type %q", role, node.ID, node.Type)
	}
}

// validateFactValue checks the fact payload: the value column for the predicate's
// value_type is set, parses, and (for text) is non-empty after trim; no other
// scalar is set.
func validateFactValue(req *AssertRequest, predicate *repository.Predicate) error {
	if predicate.ValueType == nil {
		return validationError("fact predicate %q has no value_type in the catalog", predicate.Key)
	}
	if scalarCount(req) != 1 {
		return validationError("fact predicate %q requires exactly one value field set", predicate.Key)
	}
	switch *predicate.ValueType {
	case repository.PredicateValueTypeText:
		if req.ValueText == nil {
			return validationError("fact predicate %q expects value_text", predicate.Key)
		}
		if strings.TrimSpace(*req.ValueText) == "" {
			return validationError("fact predicate %q value_text must be non-empty after trim", predicate.Key)
		}
	case repository.PredicateValueTypeNum:
		if req.ValueNum == nil {
			return validationError("fact predicate %q expects value_num", predicate.Key)
		}
	case repository.PredicateValueTypeDate:
		if req.ValueDate == nil {
			return validationError("fact predicate %q expects value_date", predicate.Key)
		}
	case repository.PredicateValueTypeBool:
		if req.ValueBool == nil {
			return validationError("fact predicate %q expects value_bool", predicate.Key)
		}
	default:
		return validationError("fact predicate %q has unknown value_type %q", predicate.Key, *predicate.ValueType)
	}
	return nil
}

// validateLocators checks each provenance locator: producer_kind set, source_kind
// in the closed enum, content-kind rows exist, and phone_call/calendar_event do
// not back a fact (metadata-only sources).
func (s *AssertService) validateLocators(ctx context.Context, tx pgx.Tx, predicate *repository.Predicate, locators []ProvenanceLocator) error {
	for i, loc := range locators {
		if !isValidProducerKind(loc.ProducerKind) {
			return validationError("locator[%d] producer_kind %q is not in {extractor,agent,user}", i, loc.ProducerKind)
		}
		if !isValidSourceKind(loc.SourceKind) {
			return validationError("locator[%d] source_kind %q is not in the closed enum", i, loc.SourceKind)
		}
		// phone_call / calendar_event are metadata sources; they may not back a fact.
		if predicate.Kind == repository.PredicateKindFact &&
			(loc.SourceKind == repository.SourceKindPhoneCall || loc.SourceKind == repository.SourceKindCalendarEvent) {
			return validationError("locator[%d] source_kind %q may not back a fact assertion", i, loc.SourceKind)
		}
		// Content-kind rows must exist at write time.
		if repository.SourceKindRequiresExistenceCheck(loc.SourceKind) {
			id, parseErr := uuid.Parse(loc.SourceID)
			if parseErr != nil {
				return validationError("locator[%d] source_id %q is not a UUID for source_kind %q", i, loc.SourceID, loc.SourceKind)
			}
			exists, err := s.assertionRepo.ExistsContentRowTx(ctx, tx, loc.SourceKind, id)
			if err != nil {
				return fmt.Errorf("check locator[%d] source row: %w", i, err)
			}
			if !exists {
				return validationError("locator[%d] %s source row %s does not exist", i, loc.SourceKind, loc.SourceID)
			}
		} else if loc.SourceID == "" {
			// user / agent_session / anarlog_transcript carry no row check, but a
			// stable source_id is still required (it keys idempotent re-runs).
			return validationError("locator[%d] source_id is required for source_kind %q", i, loc.SourceKind)
		}
	}
	return nil
}

// hasAnyScalar reports whether any value_* field is set.
func hasAnyScalar(req *AssertRequest) bool {
	return scalarCount(req) > 0
}

// scalarCount counts how many value_* fields are set.
func scalarCount(req *AssertRequest) int {
	n := 0
	if req.ValueText != nil {
		n++
	}
	if req.ValueNum != nil {
		n++
	}
	if req.ValueDate != nil {
		n++
	}
	if req.ValueBool != nil {
		n++
	}
	return n
}

func isValidProducerKind(k string) bool {
	switch k {
	case repository.ProducerKindExtractor, repository.ProducerKindAgent, repository.ProducerKindUser:
		return true
	default:
		return false
	}
}

func isValidSourceKind(k string) bool {
	switch k {
	case repository.SourceKindCommsMessage, repository.SourceKindTelegramMessage,
		repository.SourceKindMessagesMessage, repository.SourceKindMeetingNote,
		repository.SourceKindAnarlogTranscript, repository.SourceKindCalendarEvent,
		repository.SourceKindPhoneCall, repository.SourceKindUser, repository.SourceKindAgentSession:
		return true
	default:
		return false
	}
}

// --------------------------------------------------------------------------
// Step 3 match branch: corroborate an existing live proposition.
// --------------------------------------------------------------------------

// corroborate appends the incoming provenance to a matched live assertion,
// re-aggregates confidence (max) + trust_tier, emits provenance_added per NEW
// locator, and — if the matched row is 'proposed' and this write should land
// accepted — runs the step-4 supersession and transitions it to accepted.
func (s *AssertService) corroborate(ctx context.Context, tx pgx.Tx, predicate *repository.Predicate, existing *repository.Assertion, req *AssertRequest, landingAccepted bool, now time.Time) (*repository.Assertion, error) {
	// Append provenance + emit provenance_added per genuinely-new locator.
	if err := s.appendProvenance(ctx, tx, existing, req.Locators); err != nil {
		return nil, err
	}

	// Re-aggregate confidence (max) + trust_tier across the (now wider) locator set.
	newConfidence := existing.Confidence
	if req.Confidence > newConfidence {
		newConfidence = req.Confidence
	}
	newTrust := strongerTrust(existing.TrustTier, req.Locators)
	if newConfidence != existing.Confidence || !trustEqual(existing.TrustTier, newTrust) {
		if err := s.assertionRepo.UpdateAssertionConfidenceTrustTx(ctx, tx, existing.ID, newConfidence, newTrust); err != nil {
			return nil, fmt.Errorf("re-aggregate confidence/trust: %w", err)
		}
		existing.Confidence = newConfidence
		existing.TrustTier = newTrust
	}

	// Upgrade proposed → accepted when this corroboration is also an acceptance:
	// a stale proposed row must not shadow the live-proposition index against an
	// accepting writer.
	if existing.Status == repository.AssertionStatusProposed && landingAccepted {
		// Run the single-cardinality conflict check AT ACCEPT TIME (a same-value
		// prior in another bucket is widened+merged; different-value priors are
		// superseded by this now-accepting row).
		survivor, merged, err := s.resolveAcceptConflicts(ctx, tx, predicate, existing, now)
		if err != nil {
			return nil, err
		}
		if merged {
			return survivor, nil
		}
		if err := s.assertionRepo.TransitionStatusTx(ctx, tx, existing.ID, repository.AssertionStatusAccepted, nil, nil); err != nil {
			return nil, fmt.Errorf("transition proposed→accepted: %w", err)
		}
		existing.Status = repository.AssertionStatusAccepted
		if err := s.emitAssertionEvent(ctx, tx, events.KindAssertionAccepted, existing, now); err != nil {
			return nil, err
		}
	}

	return existing, nil
}

// appendProvenance inserts each incoming locator (ON CONFLICT no-op) and emits
// assertion.provenance_added only for a genuinely-new locator (the :execrows
// affected count tells us), keyed by locator_hash for idempotency.
func (s *AssertService) appendProvenance(ctx context.Context, tx pgx.Tx, assertion *repository.Assertion, locators []ProvenanceLocator) error {
	for _, loc := range locators {
		hash := computeLocatorHash(loc)
		inserted, err := s.assertionRepo.InsertProvenanceTx(ctx, tx, repository.InsertProvenanceParams{
			AssertionID:     assertion.ID,
			LocatorHash:     hash,
			SourceKind:      loc.SourceKind,
			SourceID:        loc.SourceID,
			ProducerKind:    loc.ProducerKind,
			ProducerVersion: loc.ProducerVersion,
			Field:           loc.Field,
			StartOffset:     loc.StartOffset,
			EndOffset:       loc.EndOffset,
			ChunkID:         loc.ChunkID,
			InputHash:       loc.InputHash,
			Quote:           loc.Quote,
		})
		if err != nil {
			return fmt.Errorf("insert provenance: %w", err)
		}
		if inserted {
			if err := s.emitProvenanceAddedEvent(ctx, tx, assertion, hash); err != nil {
				return err
			}
		}
	}
	return nil
}

// --------------------------------------------------------------------------
// Steps 4-5 (no-match): cardinality/supersession + write the new row.
// --------------------------------------------------------------------------

func (s *AssertService) writeNew(ctx context.Context, tx pgx.Tx, predicate *repository.Predicate, canonKey string, canonSubject uuid.UUID, canonObject *uuid.UUID, propKey string, req *AssertRequest, landingAccepted bool, knowledgeFrom, now time.Time) (*repository.Assertion, error) {
	status := repository.AssertionStatusProposed
	if landingAccepted {
		status = repository.AssertionStatusAccepted
	}

	salience := predicate.DefaultSalience
	if req.SalienceOverride != nil {
		salience = *req.SalienceOverride
	}
	if salience < 0 || salience > 100 {
		return nil, validationError("salience %d out of range [0,100]", salience)
	}

	// effective_from = COALESCE(valid_from, now) — the overlap-probe + supersession
	// boundary. A NULL valid_from (the common "as of now" edit) probes as
	// [now, valid_to) and the STORED valid_from stays NULL/open.
	effectiveFrom := now
	if req.ValidFrom != nil {
		effectiveFrom = req.ValidFrom.UTC()
	}

	// Degenerate-range guard: an explicit past valid_to with a now/unknown start is
	// an incoherent "true until a past date but start unknown" assertion → REJECT.
	if req.ValidTo != nil && !req.ValidTo.UTC().After(effectiveFrom) {
		return nil, validationError("valid_to %s is not after effective_from %s (degenerate/empty range)", req.ValidTo.UTC(), effectiveFrom)
	}

	// Single-cardinality + accepted-landing writes run the supersession check under
	// the advisory lock. Multi or proposed-landing skip it. A past-bounded backfill
	// (valid_to <= now) is a statement about the PAST → coexists, never supersedes.
	needsConflictCheck := landingAccepted && predicate.Cardinality == repository.PredicateCardinalitySingle
	pastBounded := req.ValidTo != nil && !req.ValidTo.UTC().After(now)

	if needsConflictCheck && !pastBounded {
		// Acquire the advisory lock(s) BEFORE the conflict check (serializes the
		// empty-slot race a FOR UPDATE cannot). For a symmetric-single predicate the
		// invariant is per-participant → one lock per participant, taken in lock-key
		// order to avoid deadlock.
		if err := s.acquireSlotLocks(ctx, tx, predicate, canonKey, canonSubject, canonObject); err != nil {
			return nil, err
		}

		// Probe the accepted rows whose valid-time overlaps the new effective window.
		conflicts, err := s.findOverlappingAccepted(ctx, tx, predicate, canonKey, canonSubject, canonObject, effectiveFrom, req.ValidTo)
		if err != nil {
			return nil, err
		}

		// Same-value reaffirmation: an overlapping accepted row with the SAME
		// bucket-independent proposition signature is the SAME fact in a different
		// bucket → widen it (no new row, no supersession).
		newSignature := propositionSignature(canonKey, canonSubject, normalizedPayload(canonObject, req))
		if widen := findSameValue(conflicts, newSignature); widen != nil {
			return s.widenReaffirmation(ctx, tx, predicate, widen, canonKey, canonSubject, canonObject, req, effectiveFrom)
		}

		// Different-value conflict(s). Insert the new row first (insert-new-then-
		// close-prior order under the DEFERRABLE self-FK), then close each prior.
		inserted, recovered, err := s.insertNewAndEmit(ctx, tx, predicate, canonKey, canonSubject, canonObject, propKey, req, status, salience, knowledgeFrom, now, landingAccepted)
		if err != nil {
			return nil, err
		}
		if recovered {
			// A concurrent writer won this slot; the loser corroborated its row.
			// Conflict resolution against OTHER slots is the winner's job — skip it.
			return inserted, nil
		}
		if err := s.closeConflicts(ctx, tx, conflicts, inserted, effectiveFrom, now); err != nil {
			return nil, err
		}
		return inserted, nil
	}

	// No conflict check (multi / proposed / past-bounded backfill) → just insert.
	inserted, _, err := s.insertNewAndEmit(ctx, tx, predicate, canonKey, canonSubject, canonObject, propKey, req, status, salience, knowledgeFrom, now, landingAccepted)
	return inserted, err
}

// insertNewAndEmit inserts the new assertion (with savepoint-recover), emits the
// create-transition event, and appends provenance. The recovered bool is true
// when a concurrent writer won the live slot and the loser corroborated instead
// (in which case the returned assertion is the corroborated existing row).
func (s *AssertService) insertNewAndEmit(ctx context.Context, tx pgx.Tx, predicate *repository.Predicate, canonKey string, canonSubject uuid.UUID, canonObject *uuid.UUID, propKey string, req *AssertRequest, status string, salience int16, knowledgeFrom, now time.Time, landingAccepted bool) (assertion *repository.Assertion, recovered bool, err error) {
	inserted, err := s.insertAssertionWithRecover(ctx, tx, predicate, canonKey, canonSubject, canonObject, propKey, req, status, salience, knowledgeFrom)
	if err != nil {
		return nil, false, err
	}
	if inserted == nil {
		// A concurrent writer won the live-proposition slot; re-read + corroborate.
		existing, rerr := s.assertionRepo.FindLivePropositionTx(ctx, tx, propKey)
		if rerr != nil {
			return nil, false, fmt.Errorf("re-read after concurrent insert: %w", rerr)
		}
		corr, cerr := s.corroborate(ctx, tx, predicate, existing, req, landingAccepted, now)
		return corr, true, cerr
	}

	createKind := events.KindAssertionProposed
	if status == repository.AssertionStatusAccepted {
		createKind = events.KindAssertionAccepted
	}
	if err := s.emitAssertionEvent(ctx, tx, createKind, inserted, now); err != nil {
		return nil, false, err
	}
	if err := s.appendProvenance(ctx, tx, inserted, req.Locators); err != nil {
		return nil, false, err
	}
	return inserted, false, nil
}

// findOverlappingAccepted runs the valid-time overlap probe (FOR UPDATE) for the
// slot. Asymmetric → (subject, predicate); symmetric → either participant in
// either position. effectiveFrom is the new-side lower bound (COALESCE(valid_from,
// now)); newValidTo is the new row's valid_to (nil = open).
func (s *AssertService) findOverlappingAccepted(ctx context.Context, tx pgx.Tx, predicate *repository.Predicate, canonKey string, canonSubject uuid.UUID, canonObject *uuid.UUID, effectiveFrom time.Time, newValidTo *time.Time) ([]repository.Assertion, error) {
	if predicate.Symmetric && canonObject != nil {
		return s.assertionRepo.FindAcceptedForSlotSymmetricTx(ctx, tx, canonKey, canonSubject, *canonObject, effectiveFrom, utcPtr(newValidTo))
	}
	return s.assertionRepo.FindAcceptedForSlotTx(ctx, tx, canonSubject, canonKey, effectiveFrom, utcPtr(newValidTo))
}

// normalizedPayload returns the normalized value form for the new request: the
// canonical object UUID for an edge, else the normalized scalar.
func normalizedPayload(canonObject *uuid.UUID, req *AssertRequest) string {
	if canonObject != nil {
		return canonObject.String()
	}
	return normalizeValue(req)
}

// propositionSignature is the bucket-INDEPENDENT proposition identity (length-
// prefixed canonical subject + predicate + normalized value). Two assertions have
// the SAME signature iff they represent the same fact/edge regardless of when it
// was stated — so a same-value reaffirmation (same fact, different valid-time
// bucket) matches while a DIFFERENT edge does not. For a symmetric edge the
// canonical pair (subject<=object) is encoded, so partner_of(A,B) and
// partner_of(A,C) get DISTINCT signatures even when A is the larger UUID (both
// would store object=A, but their subjects B and C differ).
func propositionSignature(canonKey string, canonSubject uuid.UUID, normalizedValue string) string {
	return encodeLengthPrefixed(canonSubject.String(), canonKey, normalizedValue)
}

// assertionSignature computes the bucket-independent proposition signature of a
// stored assertion row (canonicalized — but stored rows are ALREADY in canonical
// form, so subject/object are taken as-is).
func assertionSignature(a *repository.Assertion) string {
	return propositionSignature(a.PredicateKey, a.SubjectNodeID, assertionNormalizedValue(a))
}

// findSameValue returns the first conflict row with the SAME bucket-independent
// proposition signature as newSignature (a same-value reaffirmation), or nil.
func findSameValue(conflicts []repository.Assertion, newSignature string) *repository.Assertion {
	for i := range conflicts {
		if assertionSignature(&conflicts[i]) == newSignature {
			return &conflicts[i]
		}
	}
	return nil
}

// assertionNormalizedValue returns the normalized payload form of a stored
// assertion row (mirrors normalizeValue/normalizedPayload for a persisted row).
func assertionNormalizedValue(a *repository.Assertion) string {
	switch {
	case a.ObjectNodeID != nil:
		return a.ObjectNodeID.String()
	case a.ValueText != nil:
		return strings.ToLower(strings.TrimSpace(*a.ValueText))
	case a.ValueNum != nil:
		return strconv.FormatFloat(*a.ValueNum, 'g', -1, 64)
	case a.ValueDate != nil:
		return a.ValueDate.UTC().Format("2006-01-02")
	case a.ValueBool != nil:
		if *a.ValueBool {
			return "t"
		}
		return "f"
	default:
		return ""
	}
}

// acquireSlotLocks takes the per-slot advisory lock(s) for a single-cardinality
// accepted write. Asymmetric → one lock on (subject, canonical predicate).
// Symmetric → one lock per participant (subject AND object), taken in lock-key
// order so two concurrent writes touching the same pair acquire in the same order
// (deadlock-safe).
func (s *AssertService) acquireSlotLocks(ctx context.Context, tx pgx.Tx, predicate *repository.Predicate, canonKey string, canonSubject uuid.UUID, canonObject *uuid.UUID) error {
	var keys []int64
	if predicate.Symmetric && canonObject != nil {
		keys = []int64{
			slotLockKey(canonKey, canonSubject),
			slotLockKey(canonKey, *canonObject),
		}
	} else {
		keys = []int64{slotLockKey(canonKey, canonSubject)}
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	for _, k := range keys {
		if err := s.assertionRepo.AcquirePropositionSlotLockTx(ctx, tx, k); err != nil {
			return fmt.Errorf("acquire slot lock: %w", err)
		}
	}
	return nil
}

// widenReaffirmation extends a same-value accepted row's window to cover the new
// evidence (lower valid_from / raise valid_to), recomputes its proposition_key
// from the widened valid_from bucket, appends the new provenance, and emits
// provenance_added. No new row, no supersession event. On a recomputed-key
// collision with ANOTHER live same-value row (non-contiguous stints in different
// buckets), it MERGES the two: provenance onto the survivor, close the other
// superseded. The widened row is the survivor.
func (s *AssertService) widenReaffirmation(ctx context.Context, tx pgx.Tx, predicate *repository.Predicate, existing *repository.Assertion, canonKey string, canonSubject uuid.UUID, canonObject *uuid.UUID, req *AssertRequest, effectiveFrom time.Time) (*repository.Assertion, error) {
	// Union the windows. nil start = -inf (open), nil end = +inf (open). The new
	// side uses the request's content bounds (effectiveFrom is the probe boundary,
	// but the STORED widened window unions real evidence: a NULL new valid_from
	// keeps the existing/open start).
	widenedFrom := minStart(existing.ValidFrom, req.ValidFrom)
	widenedTo := maxEnd(existing.ValidTo, req.ValidTo)

	// If the existing row is bounded by a PENDING future successor (superseded_by
	// set while still accepted, valid_to in the future), widening valid_to past that
	// bound would un-bound it and leave TWO current rows once the successor's date
	// passes. Re-affirming the same value must NOT clear that bound — keep the
	// existing valid_to as the upper cap (only the backward lower-bound extension is
	// safe). The pending successor remains the future value.
	if existing.SupersededBy != nil {
		widenedTo = utcPtr(existing.ValidTo)
	}

	// Recompute the proposition_key from the widened valid_from bucket.
	widenedReq := *req
	widenedReq.ValidFrom = widenedFrom
	newKey := computePropositionKey(predicate, canonKey, canonSubject, canonObject, &widenedReq)

	// Collision: another LIVE row already holds the recomputed key (a non-contiguous
	// same-value stint in that bucket). Merge it into this survivor instead of
	// UPDATE-ing into the unique index (which would 23505).
	if newKey != existing.PropositionKey {
		collider, err := s.assertionRepo.FindLivePropositionTx(ctx, tx, newKey)
		if err != nil && !errors.Is(err, db.ErrNotFound) {
			return nil, fmt.Errorf("widen collision check: %w", err)
		}
		if err == nil && collider.ID != existing.ID {
			// Union the survivor window with the collider's too, then merge.
			widenedFrom = minStart(widenedFrom, collider.ValidFrom)
			widenedTo = maxEnd(widenedTo, collider.ValidTo)
			widenedReq.ValidFrom = widenedFrom
			newKey = computePropositionKey(predicate, canonKey, canonSubject, canonObject, &widenedReq)
			if err := s.mergeSameValue(ctx, tx, collider, existing); err != nil {
				return nil, err
			}
		}
	}

	if err := s.assertionRepo.WidenAssertionValidityTx(ctx, tx, existing.ID, widenedFrom, widenedTo, newKey); err != nil {
		return nil, fmt.Errorf("widen assertion validity: %w", err)
	}
	existing.ValidFrom = widenedFrom
	existing.ValidTo = widenedTo
	existing.PropositionKey = newKey

	// Append the corroborating provenance + re-aggregate confidence/trust.
	if err := s.appendProvenance(ctx, tx, existing, req.Locators); err != nil {
		return nil, err
	}
	newConfidence := existing.Confidence
	if req.Confidence > newConfidence {
		newConfidence = req.Confidence
	}
	newTrust := strongerTrust(existing.TrustTier, req.Locators)
	if newConfidence != existing.Confidence || !trustEqual(existing.TrustTier, newTrust) {
		if err := s.assertionRepo.UpdateAssertionConfidenceTrustTx(ctx, tx, existing.ID, newConfidence, newTrust); err != nil {
			return nil, fmt.Errorf("re-aggregate confidence/trust on widen: %w", err)
		}
		existing.Confidence = newConfidence
		existing.TrustTier = newTrust
	}
	return existing, nil
}

// mergeSameValue collapses a colliding same-value live row (loser) into the
// survivor: move the loser's provenance onto the survivor (ON CONFLICT no-op),
// fold the loser's confidence (max) + trust_tier (strongest) into the survivor,
// and close the loser superseded with superseded_by = survivor. Emits
// provenance_added per moved locator + superseded for the loser.
func (s *AssertService) mergeSameValue(ctx context.Context, tx pgx.Tx, loser, survivor *repository.Assertion) error {
	provs, err := s.assertionRepo.ListProvenance(ctx, loser.ID)
	if err != nil {
		return fmt.Errorf("list loser provenance: %w", err)
	}
	for _, p := range provs {
		inserted, err := s.assertionRepo.InsertProvenanceTx(ctx, tx, repository.InsertProvenanceParams{
			AssertionID:     survivor.ID,
			LocatorHash:     p.LocatorHash,
			SourceKind:      p.SourceKind,
			SourceID:        p.SourceID,
			ProducerKind:    p.ProducerKind,
			ProducerVersion: p.ProducerVersion,
			Field:           p.Field,
			StartOffset:     p.StartOffset,
			EndOffset:       p.EndOffset,
			ChunkID:         p.ChunkID,
			InputHash:       p.InputHash,
			Quote:           p.Quote,
		})
		if err != nil {
			return fmt.Errorf("move provenance to survivor: %w", err)
		}
		if inserted {
			if err := s.emitProvenanceAddedEvent(ctx, tx, survivor, p.LocatorHash); err != nil {
				return err
			}
		}
	}
	// Fold the loser's confidence (max) + trust_tier (strongest) into the survivor —
	// the survivor now owns the loser's provenance, so its aggregate must reflect it.
	newConfidence := survivor.Confidence
	if loser.Confidence > newConfidence {
		newConfidence = loser.Confidence
	}
	newTrust := strongerTrustTier(survivor.TrustTier, loser.TrustTier)
	if newConfidence != survivor.Confidence || !trustEqual(survivor.TrustTier, newTrust) {
		if err := s.assertionRepo.UpdateAssertionConfidenceTrustTx(ctx, tx, survivor.ID, newConfidence, newTrust); err != nil {
			return fmt.Errorf("fold loser confidence/trust into survivor: %w", err)
		}
		survivor.Confidence = newConfidence
		survivor.TrustTier = newTrust
	}
	now := accelerated.GetCurrentTime().UTC()
	knowledgeTo := now
	if loser.KnowledgeFrom.After(knowledgeTo) {
		knowledgeTo = loser.KnowledgeFrom.UTC()
	}
	closure := repository.ClosureReasonSuperseded
	if err := s.assertionRepo.CloseAssertionTx(ctx, tx, repository.CloseAssertionParams{
		ID:            loser.ID,
		ValidTo:       loser.ValidTo,
		Status:        repository.AssertionStatusSuperseded,
		ClosureReason: &closure,
		SupersededBy:  &survivor.ID,
		KnowledgeTo:   &knowledgeTo,
	}); err != nil {
		return fmt.Errorf("close merged loser: %w", err)
	}
	return s.emitAssertionEvent(ctx, tx, events.KindAssertionSuperseded, loser, now)
}

// closeConflicts closes each different-value overlapping prior for a present /
// future successor (D6 step 4). The newRow is the just-inserted successor.
//
// A prior is BOUNDED (kept accepted/knowledge-open, valid_to=effectiveFrom,
// superseded_by=newRow — the rollover job terminalizes it when the bound passes)
// when it is genuinely CURRENT at some point strictly before the successor's
// boundary: the successor is future (effectiveFrom > now) AND the prior starts
// before the boundary (open start, or valid_from < effectiveFrom). This covers a
// chained future successor: A→B(July1)→C(Aug1) bounds B to Aug1 (B is current
// July–Aug), it does NOT terminalize B and leave a July gap.
//
// Otherwise the prior is TERMINALIZED (status=superseded, knowledge_to=now): a
// present successor (effectiveFrom <= now), or a future successor that starts
// at/before the prior's own valid_from (the prior never becomes current — a
// present edit cancelling a pending future successor, or a future successor that
// fully precedes another pending one).
func (s *AssertService) closeConflicts(ctx context.Context, tx pgx.Tx, conflicts []repository.Assertion, newRow *repository.Assertion, effectiveFrom, now time.Time) error {
	future := effectiveFrom.After(now)
	for i := range conflicts {
		prior := &conflicts[i]
		if prior.ID == newRow.ID {
			continue
		}
		// The prior is current before the boundary iff its start precedes it.
		startsBeforeBoundary := prior.ValidFrom == nil || effectiveFrom.After(*prior.ValidFrom)

		if future && startsBeforeBoundary {
			// Bound the still-current prior; keep it accepted (current until the date).
			if err := s.assertionRepo.BoundPendingSuccessorTx(ctx, tx, prior.ID, effectiveFrom, newRow.ID); err != nil {
				return fmt.Errorf("bound pending successor: %w", err)
			}
			continue
		}

		// Terminalize the prior. boundTo is effectiveFrom for a prior that is current
		// up to the present boundary; for a never-current pending-future row (its
		// valid_from is at/after the boundary) keep its EXISTING valid_to so the
		// valid_range CHECK is never inverted to a zero-length/backward window.
		boundTo := closeBoundary(prior, effectiveFrom)
		knowledgeTo := now
		if prior.KnowledgeFrom.After(knowledgeTo) {
			knowledgeTo = prior.KnowledgeFrom.UTC()
		}
		closure := repository.ClosureReasonSuperseded
		if err := s.assertionRepo.CloseAssertionTx(ctx, tx, repository.CloseAssertionParams{
			ID:            prior.ID,
			ValidTo:       boundTo,
			Status:        repository.AssertionStatusSuperseded,
			ClosureReason: &closure,
			SupersededBy:  &newRow.ID,
			KnowledgeTo:   &knowledgeTo,
		}); err != nil {
			return fmt.Errorf("close prior on supersession: %w", err)
		}
		if err := s.emitAssertionEvent(ctx, tx, events.KindAssertionSuperseded, prior, now); err != nil {
			return err
		}
	}
	return nil
}

// closeBoundary picks the valid_to to stamp on a TERMINALIZED prior. A prior that
// is current up to the boundary (open start, or valid_from < effectiveFrom) is
// closed at effectiveFrom. A never-current pending-future prior (valid_from at/after
// effectiveFrom) keeps its EXISTING valid_to: stamping effectiveFrom (which is <= its
// valid_from) would invert the range and stamping its own valid_from would make a
// zero-length window — both violate the valid_range CHECK. It is terminal regardless.
func closeBoundary(prior *repository.Assertion, effectiveFrom time.Time) *time.Time {
	if prior.ValidFrom != nil && !effectiveFrom.After(*prior.ValidFrom) {
		return utcPtr(prior.ValidTo)
	}
	ef := effectiveFrom
	return &ef
}

// supersedeConflicts runs the single-cardinality conflict resolution for an
// already-existing row transitioning to accepted (the proposed→accepted upgrade
// and the Accept path). It acquires the slot lock(s), probes the overlapping
// accepted rows (excluding the accepting row), and closes the different-value
// priors (present/future branch). It does NOT re-check same-value (an accept of a
// proposed row is a distinct proposition from any same-value accepted row, so
// they coexist until the user reconciles; SP1 keeps accept-time minimal).
// resolveAcceptConflicts returns (survivor, merged). When merged is true, the
// accepting (proposed) row was a same-value reaffirmation of an existing accepted
// row: it was absorbed (provenance + confidence folded onto the survivor, the
// accepting row closed superseded), and survivor is the live row the caller should
// return — the caller must NOT then transition the accepting row to accepted.
func (s *AssertService) resolveAcceptConflicts(ctx context.Context, tx pgx.Tx, predicate *repository.Predicate, accepting *repository.Assertion, now time.Time) (survivor *repository.Assertion, merged bool, err error) {
	if predicate.Cardinality != repository.PredicateCardinalitySingle {
		return nil, false, nil
	}
	canonKey, canonSubject, canonObject := canonicalEdge(predicate, accepting.SubjectNodeID, accepting.ObjectNodeID)
	if err := s.acquireSlotLocks(ctx, tx, predicate, canonKey, canonSubject, canonObject); err != nil {
		return nil, false, err
	}
	effectiveFrom := now
	if accepting.ValidFrom != nil {
		effectiveFrom = accepting.ValidFrom.UTC()
	}
	// A past-bounded accepting row coexists (it is historical) → no supersession.
	if accepting.ValidTo != nil && !accepting.ValidTo.UTC().After(now) {
		return nil, false, nil
	}
	conflicts, err := s.findOverlappingAccepted(ctx, tx, predicate, canonKey, canonSubject, canonObject, effectiveFrom, accepting.ValidTo)
	if err != nil {
		return nil, false, err
	}
	// Same-value reaffirmation at accept time: an overlapping accepted row with the
	// SAME value is the same fact in a different bucket → WIDEN it to cover the
	// accepting row's window and MERGE the accepting (proposed) row into it. Mirrors
	// the writeNew widen rule (P1: Accept must widen, not supersede, a same value).
	acceptingSignature := assertionSignature(accepting)
	if existing := findSameValue(conflicts, acceptingSignature); existing != nil {
		existing.ValidFrom = minStart(existing.ValidFrom, accepting.ValidFrom)
		existing.ValidTo = maxEnd(existing.ValidTo, accepting.ValidTo)
		widenReq := &AssertRequest{ValidFrom: existing.ValidFrom}
		newKey := computePropositionKey(predicate, canonKey, canonSubject, canonObject, widenReq)
		if err := s.assertionRepo.WidenAssertionValidityTx(ctx, tx, existing.ID, existing.ValidFrom, existing.ValidTo, newKey); err != nil {
			return nil, false, fmt.Errorf("widen survivor at accept: %w", err)
		}
		existing.PropositionKey = newKey
		if err := s.mergeSameValue(ctx, tx, accepting, existing); err != nil {
			return nil, false, err
		}
		return existing, true, nil
	}

	return nil, false, s.closeConflicts(ctx, tx, conflicts, accepting, effectiveFrom, now)
}

// insertAssertionWithRecover inserts the new assertion, recovering from the
// identical-value concurrent-insert 23505 on idx_assertion_live_proposition via a
// nested savepoint: on the unique violation it rolls back the savepoint (so the
// outer tx is not aborted) and returns (nil, nil) to signal the caller to re-read
// + corroborate. Any other error propagates.
func (s *AssertService) insertAssertionWithRecover(ctx context.Context, tx pgx.Tx, predicate *repository.Predicate, canonKey string, canonSubject uuid.UUID, canonObject *uuid.UUID, propKey string, req *AssertRequest, status string, salience int16, knowledgeFrom time.Time) (*repository.Assertion, error) {
	params := repository.InsertAssertionParams{
		SubjectNodeID:  canonSubject,
		PredicateKey:   canonKey,
		ObjectNodeID:   canonObject,
		KnowledgeFrom:  knowledgeFrom,
		Confidence:     req.Confidence,
		Salience:       salience,
		Status:         status,
		PropositionKey: propKey,
		// trust_tier denorm: the strongest producer across the row's locators.
		TrustTier: strongerTrust(nil, req.Locators),
	}
	// Carry the fact scalar (edge object is set via ObjectNodeID above).
	if predicate.Kind == repository.PredicateKindFact {
		params.ValueText = req.ValueText
		params.ValueNum = req.ValueNum
		params.ValueDate = req.ValueDate
		params.ValueBool = req.ValueBool
	}
	// valid_from is set ONLY from content evidence (never defaulted to now).
	params.ValidFrom = utcPtr(req.ValidFrom)
	params.ValidTo = utcPtr(req.ValidTo)

	sp, err := tx.Begin(ctx) // nested savepoint
	if err != nil {
		return nil, fmt.Errorf("begin savepoint: %w", err)
	}
	inserted, err := s.assertionRepo.InsertAssertionTx(ctx, sp, params)
	if err != nil {
		_ = sp.Rollback(ctx)
		if isLivePropositionViolation(err) {
			return nil, nil // signal: concurrent writer won; recover via re-read
		}
		return nil, fmt.Errorf("insert assertion: %w", err)
	}
	if err := sp.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit savepoint: %w", err)
	}
	return inserted, nil
}

// --------------------------------------------------------------------------
// AssertClosure / Accept / Reject / Retract / EnsureLatentPerson.
// --------------------------------------------------------------------------

// AssertClosure closes a single-cardinality slot with NO successor ("between
// jobs"): the current accepted assertion for (subject, predicate) is closed
// 'ended' / superseded / knowledge_to=now / valid_to=now, and the current value
// becomes a gap. Returns ErrNotFound (via the gap) if there is no current row.
func (s *AssertService) AssertClosure(ctx context.Context, req ClosureRequest) (err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if rb := tx.Rollback(ctx); rb != nil && !errors.Is(rb, pgx.ErrTxClosed) && err == nil {
			err = rb
		}
	}()
	if err = s.AssertClosureTx(ctx, tx, req); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// AssertClosureTx is the tx-bound variant of AssertClosure.
func (s *AssertService) AssertClosureTx(ctx context.Context, tx pgx.Tx, req ClosureRequest) error {
	predicate, err := s.predicateRepo.GetPredicate(ctx, req.PredicateKey)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return validationError("unknown predicate %q", req.PredicateKey)
		}
		return fmt.Errorf("load predicate: %w", err)
	}
	now := accelerated.GetCurrentTime().UTC()
	// Close the current accepted row for the slot (the subject's own slot; a
	// closure targets the subject's predicate directly, not the canonical edge —
	// closure is a fact/single-edge concept and the read uses subject+predicate).
	current, err := s.assertionRepo.GetCurrentAcceptedTx(ctx, tx, req.SubjectNodeID, predicate.Key, now)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return nil // already a gap; nothing to close (idempotent)
		}
		return fmt.Errorf("load current for closure: %w", err)
	}
	closure := repository.ClosureReasonEnded
	if err := s.assertionRepo.CloseAssertionTx(ctx, tx, repository.CloseAssertionParams{
		ID:            current.ID,
		ValidTo:       &now,
		Status:        repository.AssertionStatusSuperseded,
		ClosureReason: &closure,
		SupersededBy:  nil,
		KnowledgeTo:   &now,
	}); err != nil {
		return fmt.Errorf("close current on closure: %w", err)
	}
	return s.emitAssertionEvent(ctx, tx, events.KindAssertionSuperseded, current, now)
}

// Accept transitions a proposed assertion to accepted (a human/agent confirms a
// review item). It runs the single-cardinality supersession AT ACCEPT TIME so a
// proposed row that becomes accepted supersedes the prior accepted slot.
func (s *AssertService) Accept(ctx context.Context, assertionID uuid.UUID, _ AcceptRequest) (a *repository.Assertion, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() {
		if rb := tx.Rollback(ctx); rb != nil && !errors.Is(rb, pgx.ErrTxClosed) && err == nil {
			err = rb
		}
	}()
	a, err = s.AcceptTx(ctx, tx, assertionID)
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return a, nil
}

// AcceptTx is the tx-bound variant of Accept.
func (s *AssertService) AcceptTx(ctx context.Context, tx pgx.Tx, assertionID uuid.UUID) (*repository.Assertion, error) {
	now := accelerated.GetCurrentTime().UTC()
	// Row-lock so the proposed-status check + the accept are atomic vs a concurrent
	// Accept/Reject of the same row.
	assertion, err := s.assertionRepo.GetAssertionForUpdateTx(ctx, tx, assertionID)
	if err != nil {
		return nil, err
	}
	if assertion.Status != repository.AssertionStatusProposed {
		return nil, validationError("assertion %s is %q, only a proposed assertion may be accepted", assertionID, assertion.Status)
	}
	predicate, err := s.predicateRepo.GetPredicate(ctx, assertion.PredicateKey)
	if err != nil {
		return nil, fmt.Errorf("load predicate: %w", err)
	}
	// Single-cardinality conflict resolution at accept time (locks the slot, then
	// widens a same-value prior or supersedes different-value priors).
	survivor, merged, err := s.resolveAcceptConflicts(ctx, tx, predicate, assertion, now)
	if err != nil {
		return nil, err
	}
	if merged {
		// The accepting row was a same-value reaffirmation absorbed into survivor;
		// it is already closed. Return the live survivor instead of accepting it.
		return survivor, nil
	}
	if err := s.assertionRepo.TransitionStatusTx(ctx, tx, assertionID, repository.AssertionStatusAccepted, nil, nil); err != nil {
		return nil, fmt.Errorf("transition proposed→accepted: %w", err)
	}
	assertion.Status = repository.AssertionStatusAccepted
	if err := s.emitAssertionEvent(ctx, tx, events.KindAssertionAccepted, assertion, now); err != nil {
		return nil, err
	}
	return assertion, nil
}

// Reject transitions a proposed assertion to rejected (a review item declined). It
// never enters "current". Emits assertion.rejected.
func (s *AssertService) Reject(ctx context.Context, assertionID uuid.UUID, _ RejectRequest) (a *repository.Assertion, err error) {
	return s.terminalTransition(ctx, assertionID, repository.AssertionStatusProposed,
		repository.AssertionStatusRejected, repository.ClosureReasonRejected, events.KindAssertionRejected)
}

// Retract transitions an accepted assertion to retracted (an explicit correction
// or last-provenance-loss). A retract is the closure of an accepted row with no
// successor — semantically a closure — so it emits assertion.superseded (NOT a
// new kind); the row carries closure_reason='retracted' for any consumer that
// needs to distinguish.
func (s *AssertService) Retract(ctx context.Context, assertionID uuid.UUID, _ RetractRequest) (a *repository.Assertion, err error) {
	return s.terminalTransition(ctx, assertionID, repository.AssertionStatusAccepted,
		repository.AssertionStatusRetracted, repository.ClosureReasonRetracted, events.KindAssertionSuperseded)
}

// terminalTransition is the shared Reject/Retract path: require the from-status,
// set the terminal status + knowledge_to + closure_reason, and emit the event.
func (s *AssertService) terminalTransition(ctx context.Context, assertionID uuid.UUID, fromStatus, toStatus, closureReason string, kind events.Kind) (a *repository.Assertion, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() {
		if rb := tx.Rollback(ctx); rb != nil && !errors.Is(rb, pgx.ErrTxClosed) && err == nil {
			err = rb
		}
	}()
	now := accelerated.GetCurrentTime().UTC()
	// Row-lock so the from-status check + the update are atomic (a concurrent
	// Accept/Reject on the same row blocks here, so they cannot both succeed).
	assertion, err := s.assertionRepo.GetAssertionForUpdateTx(ctx, tx, assertionID)
	if err != nil {
		return nil, err
	}
	if assertion.Status != fromStatus {
		return nil, validationError("assertion %s is %q, expected %q for this transition", assertionID, assertion.Status, fromStatus)
	}
	// knowledge_to >= knowledge_from (the row's KnowledgeFromOverride may put
	// knowledge_from in the future; clamp so the assertion_knowledge_range CHECK
	// holds).
	knowledgeTo := now
	if assertion.KnowledgeFrom.After(knowledgeTo) {
		knowledgeTo = assertion.KnowledgeFrom.UTC()
	}
	closure := closureReason
	if err := s.assertionRepo.TransitionStatusTx(ctx, tx, assertionID, toStatus, &knowledgeTo, &closure); err != nil {
		return nil, fmt.Errorf("terminal transition to %s: %w", toStatus, err)
	}
	assertion.Status = toStatus
	assertion.KnowledgeTo = &knowledgeTo
	assertion.ClosureReason = &closure
	if err := s.emitAssertionEvent(ctx, tx, kind, assertion, now); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return assertion, nil
}

// RunRollover terminalizes the bounded-with-pending-successor rows whose bound
// has been reached (the daily rollover sweep, D6 step 4 / PR4). In one tx it flips
// each matching row (status='accepted' AND knowledge_to IS NULL AND superseded_by
// IS NOT NULL AND valid_to <= now) to status='superseded', closure_reason=
// 'superseded', knowledge_to=now, and emits assertion.superseded per row. It is a
// stateless catch-up sweep — a row already rolled over no longer matches, so
// re-running is a no-op (and downtime catches up all overdue rollovers). A
// successor-less bounded-past fact (superseded_by IS NULL) is NOT matched, so it
// stays a legitimate accepted historical fact. Returns the number rolled over.
//
// The UPDATE...RETURNING and the per-row event publishes are in ONE tx, so a
// publish failure rolls back the flip too — the next run re-selects and retries
// (no stranded row). Returns the count of rows terminalized.
func (s *AssertService) RunRollover(ctx context.Context) (rolledOver int, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() {
		if rb := tx.Rollback(ctx); rb != nil && !errors.Is(rb, pgx.ErrTxClosed) && err == nil {
			err = rb
		}
	}()
	now := accelerated.GetCurrentTime().UTC()
	rows, err := s.assertionRepo.RolloverDueBoundedSuccessorsTx(ctx, tx, now)
	if err != nil {
		return 0, fmt.Errorf("rollover due bounded successors: %w", err)
	}
	for i := range rows {
		if err := s.emitAssertionEvent(ctx, tx, events.KindAssertionSuperseded, &rows[i], now); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return len(rows), nil
}

// EnsureLatentPerson returns the node id for a person referenced by an edge but
// not yet in the CRM (a knows/introduced_by target). It mints a latent
// person node (no contact row) and returns its id. Idempotency is the caller's
// concern — a fresh node is created each call (latent-person dedup is deferred per
// the spec); callers that hold a known node id should pass it directly rather than
// minting a latent one.
func (s *AssertService) EnsureLatentPerson(ctx context.Context, tx pgx.Tx, label string) (uuid.UUID, error) {
	id := uuid.New()
	if _, err := s.nodeRepo.CreateNodeTx(ctx, tx, id, repository.NodeTypePerson, label); err != nil {
		return uuid.Nil, fmt.Errorf("create latent person node: %w", err)
	}
	return id, nil
}

// --------------------------------------------------------------------------
// Event emission.
// --------------------------------------------------------------------------

// emitAssertionEvent publishes a one-shot assertion lifecycle event keyed for
// idempotency by '<assertion_id>:<transition>'. The transition token is derived
// from the kind (proposed/accepted/superseded/rejected). A retract emits the
// superseded kind, so its key is '<id>:superseded' — still one-shot (an accepted
// row is closed at most once).
func (s *AssertService) emitAssertionEvent(ctx context.Context, tx pgx.Tx, kind events.Kind, assertion *repository.Assertion, now time.Time) error {
	sourceID := assertion.ID.String() + ":" + transitionToken(kind)
	return s.publishAssertionEnvelope(ctx, tx, kind, assertion, sourceID, now)
}

// emitProvenanceAddedEvent publishes a provenance_added event keyed per locator by
// '<assertion_id>:provenance:<locator_hash>' (many-per-assertion).
func (s *AssertService) emitProvenanceAddedEvent(ctx context.Context, tx pgx.Tx, assertion *repository.Assertion, locatorHash string) error {
	sourceID := assertion.ID.String() + ":provenance:" + locatorHash
	return s.publishAssertionEnvelope(ctx, tx, events.KindAssertionProvenanceAdded, assertion, sourceID, now())
}

// publishAssertionEnvelope marshals the shared payload + publishes the envelope.
func (s *AssertService) publishAssertionEnvelope(ctx context.Context, tx pgx.Tx, kind events.Kind, assertion *repository.Assertion, sourceID string, observedAt time.Time) error {
	payload, err := events.Marshal(kind, events.AssertionEventPayload{
		Version:       1,
		AssertionID:   assertion.ID,
		SubjectNodeID: assertion.SubjectNodeID,
		PredicateKey:  assertion.PredicateKey,
	})
	if err != nil {
		return fmt.Errorf("marshal %s payload: %w", kind, err)
	}
	env := &events.Envelope{
		Source:     "assertion",
		SourceID:   sourceID,
		Kind:       kind,
		Payload:    payload,
		ObservedAt: observedAt,
	}
	if err := s.bus.PublishTx(ctx, tx, env); err != nil {
		return fmt.Errorf("publish %s: %w", kind, err)
	}
	return nil
}

// transitionToken maps a kind to its one-shot source_id transition token.
func transitionToken(kind events.Kind) string {
	switch kind {
	case events.KindAssertionProposed:
		return "proposed"
	case events.KindAssertionAccepted:
		return "accepted"
	case events.KindAssertionSuperseded:
		return "superseded"
	case events.KindAssertionRejected:
		return "rejected"
	default:
		return string(kind)
	}
}

func now() time.Time { return accelerated.GetCurrentTime().UTC() }

// --------------------------------------------------------------------------
// Small value helpers.
// --------------------------------------------------------------------------

// utcPtr normalizes a nullable time to UTC (nil stays nil).
func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

// minStart returns the earlier of two valid_from bounds; nil = open (-inf), which
// is the minimum, so a nil on either side wins.
func minStart(a, b *time.Time) *time.Time {
	if a == nil || b == nil {
		return nil
	}
	if a.Before(*b) {
		return utcPtr(a)
	}
	return utcPtr(b)
}

// maxEnd returns the later of two valid_to bounds; nil = open (+inf), which is the
// maximum, so a nil on either side wins.
func maxEnd(a, b *time.Time) *time.Time {
	if a == nil || b == nil {
		return nil
	}
	if a.After(*b) {
		return utcPtr(a)
	}
	return utcPtr(b)
}

// producerRank ranks producer kinds for trust_tier (user strongest). The
// trust_tier denorm is the strongest producer across an assertion's locators.
func producerRank(producerKind string) int {
	switch producerKind {
	case repository.ProducerKindUser:
		return 3
	case repository.ProducerKindAgent:
		return 2
	case repository.ProducerKindExtractor:
		return 1
	default:
		return 0
	}
}

// trustTierForProducer maps a producer kind to the trust_tier denorm token.
func trustTierForProducer(producerKind string) string {
	switch producerKind {
	case repository.ProducerKindUser:
		return "user"
	case repository.ProducerKindAgent:
		return "agent"
	default:
		return "extractor"
	}
}

// strongerTrust returns the trust_tier denorm after folding in the incoming
// locators: the strongest producer across the existing tier + the new locators.
func strongerTrust(existing *string, locators []ProvenanceLocator) *string {
	bestRank := 0
	bestTier := ""
	if existing != nil {
		bestTier = *existing
		bestRank = trustTierRank(*existing)
	}
	for _, loc := range locators {
		if r := producerRank(loc.ProducerKind); r > bestRank {
			bestRank = r
			bestTier = trustTierForProducer(loc.ProducerKind)
		}
	}
	if bestTier == "" {
		return nil
	}
	return &bestTier
}

// strongerTrustTier returns the stronger of two nullable trust tiers (the higher
// rank). Used when merging a same-value loser's trust into a survivor.
func strongerTrustTier(a, b *string) *string {
	rankA, rankB := 0, 0
	if a != nil {
		rankA = trustTierRank(*a)
	}
	if b != nil {
		rankB = trustTierRank(*b)
	}
	if rankB > rankA {
		return b
	}
	return a
}

// trustTierRank ranks a stored trust_tier token (mirrors producerRank but over the
// denorm tokens user/agent/extractor).
func trustTierRank(tier string) int {
	switch tier {
	case "user":
		return 3
	case "agent":
		return 2
	case "extractor":
		return 1
	default:
		return 0
	}
}

// trustEqual compares two nullable trust tiers.
func trustEqual(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// isLivePropositionViolation reports whether err is a 23505 unique-violation on
// the live-proposition index (the identical-value concurrent-insert race). Scoped
// to that constraint so an UNRELATED unique violation is NOT silently treated as a
// recoverable concurrent insert.
func isLivePropositionViolation(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != pgerrcode.UniqueViolation {
		return false
	}
	return pgErr.ConstraintName == "idx_assertion_live_proposition"
}
