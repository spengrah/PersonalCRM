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

	// GLOBAL lock order — for a single-cardinality write that may land accepted,
	// acquire the slot advisory lock(s) BEFORE any row read/lock (the dedup lookup
	// below, the corroborate-upgrade row update, or writeNew's conflict probe). This
	// keeps advisory-before-row across BOTH the match (corroborate→accept) and
	// no-match (writeNew) paths, so a concurrent AcceptTx (which also locks advisory
	// first) cannot deadlock against this tx. Re-acquisition deeper down is
	// re-entrant (harmless). Multi-cardinality + proposed-landing writes take no slot
	// lock.
	if landingAccepted && predicate.Cardinality == repository.PredicateCardinalitySingle {
		if err := s.acquireSlotLocks(ctx, tx, predicate, canonKey, canonSubject, canonObject); err != nil {
			return nil, err
		}
	}

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

	// Degenerate-range guard (in validation, BEFORE the dedup lookup, so it fires
	// even when an existing live row would otherwise corroborate): a now/unknown
	// start (valid_from NULL → effective_from = now) combined with an explicit past
	// valid_to is an incoherent "true until a past date but start unknown"
	// assertion → REJECT (routed to manual review). A fully-bounded historical fact
	// has an explicit valid_from < valid_to and passes.
	effectiveFrom := accelerated.GetCurrentTime().UTC()
	if req.ValidFrom != nil {
		effectiveFrom = req.ValidFrom.UTC()
	}
	if req.ValidTo != nil && !req.ValidTo.UTC().After(effectiveFrom) {
		return nil, "", uuid.Nil, nil, validationError("valid_to %s is not after effective_from %s (degenerate/empty range)", req.ValidTo.UTC(), effectiveFrom)
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
	// [now, valid_to) and the STORED valid_from stays NULL/open. (The degenerate
	// valid_to <= effective_from range is already rejected in validate(), before the
	// dedup lookup, so it cannot slip through a corroboration match.)
	effectiveFrom := now
	if req.ValidFrom != nil {
		effectiveFrom = req.ValidFrom.UTC()
	}

	// Single-cardinality + accepted-landing writes consult the slot. Multi or
	// proposed-landing skip the slot entirely. A past-bounded backfill (valid_to <=
	// now) is a statement about the PAST → it COEXISTS with a different-value current
	// (never supersedes); but a SAME-value past stint still merges into the slot's
	// same-value row (it is the same fact, just a non-contiguous interval).
	needsConflictCheck := landingAccepted && predicate.Cardinality == repository.PredicateCardinalitySingle
	pastBounded := req.ValidTo != nil && !req.ValidTo.UTC().After(now)

	if needsConflictCheck {
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

		// Same-value reaffirmation (runs even for a past-bounded write): overlapping
		// accepted row(s) with the SAME bucket-independent proposition signature are
		// the SAME fact in different buckets → widen ONE survivor over the union of all
		// of them (no new row, no supersession). A new window can bridge several
		// same-value stints; all merge.
		newSignature := propositionSignature(canonKey, canonSubject, normalizedPayload(canonObject, req))
		if same := sameValueConflicts(conflicts, newSignature); len(same) > 0 {
			return s.widenReaffirmation(ctx, tx, predicate, same, canonKey, canonSubject, canonObject, req)
		}

		// No same-value match. A past-bounded backfill of a DIFFERENT value coexists
		// (it is historical) — fall through to a plain insert. Otherwise this is a
		// present/future successor: insert the new row first (insert-new-then-close-
		// prior under the DEFERRABLE self-FK), then close each different-value prior.
		if !pastBounded {
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
	}

	// Plain insert: multi / proposed-landing, OR a single-card past-bounded backfill
	// of a different value (coexists, no supersession).
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

// sameValueConflicts returns ALL conflict rows with the SAME bucket-independent
// signature as newSignature. A new window can bridge several non-contiguous
// same-value stints (e.g. X[2010,2015) + X[2020,∞) both overlap a new X[2012,2022));
// every one must collapse into a single survivor, else the single-cardinality slot
// would keep two live rows.
func sameValueConflicts(conflicts []repository.Assertion, newSignature string) []*repository.Assertion {
	var out []*repository.Assertion
	for i := range conflicts {
		if assertionSignature(&conflicts[i]) == newSignature {
			out = append(out, &conflicts[i])
		}
	}
	return out
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

// widenReaffirmation collapses one-or-more same-value accepted rows (same[0] is
// the survivor; same[1:] are bridged stints) into a single survivor whose window
// is the union of all of them + the new evidence, recomputes its proposition_key,
// appends the new provenance, and emits provenance_added. No new row, no
// supersession event for the survivor. The bridged rows + any recomputed-key
// collider are MERGED (provenance moved, closed superseded). The widened row is
// the survivor. A new window can bridge several non-contiguous same-value stints
// (e.g. X[2010,2015)+X[2020,∞) under a new X[2012,2022)); all merge into one.
func (s *AssertService) widenReaffirmation(ctx context.Context, tx pgx.Tx, predicate *repository.Predicate, same []*repository.Assertion, canonKey string, canonSubject uuid.UUID, canonObject *uuid.UUID, req *AssertRequest) (*repository.Assertion, error) {
	survivor := same[0]

	// Union the survivor window with the new evidence + every bridged stint. nil
	// start = -inf, nil end = +inf (open). Merge the bridged stints into the survivor
	// first so the union covers them and the slot keeps exactly one live row.
	widenedFrom := minStart(survivor.ValidFrom, req.ValidFrom)
	widenedTo := maxEnd(survivor.ValidTo, req.ValidTo)
	for _, other := range same[1:] {
		widenedFrom = minStart(widenedFrom, other.ValidFrom)
		widenedTo = maxEnd(widenedTo, other.ValidTo)
		if err := s.mergeSameValue(ctx, tx, other, survivor); err != nil {
			return nil, err
		}
	}

	// If the survivor is bounded by a PENDING future successor (superseded_by set
	// while still accepted), widening valid_to past that bound would un-bound it and
	// leave two current rows once the successor's date passes. Keep the existing
	// valid_to as the upper cap (only the backward lower-bound extension is safe).
	if survivor.SupersededBy != nil {
		widenedTo = utcPtr(survivor.ValidTo)
	}

	// Recompute the proposition_key from the widened valid_from bucket.
	widenedReq := *req
	widenedReq.ValidFrom = widenedFrom
	newKey := computePropositionKey(predicate, canonKey, canonSubject, canonObject, &widenedReq)

	// Collision: another LIVE row (NOT in the overlap probe — e.g. a disjoint stint)
	// already holds the recomputed key. Merge it into the survivor instead of
	// UPDATE-ing into the unique index (which would 23505).
	if newKey != survivor.PropositionKey {
		collider, err := s.assertionRepo.FindLivePropositionTx(ctx, tx, newKey)
		if err != nil && !errors.Is(err, db.ErrNotFound) {
			return nil, fmt.Errorf("widen collision check: %w", err)
		}
		if err == nil && collider.ID != survivor.ID {
			widenedFrom = minStart(widenedFrom, collider.ValidFrom)
			widenedTo = maxEnd(widenedTo, collider.ValidTo)
			widenedReq.ValidFrom = widenedFrom
			newKey = computePropositionKey(predicate, canonKey, canonSubject, canonObject, &widenedReq)
			if err := s.mergeSameValue(ctx, tx, collider, survivor); err != nil {
				return nil, err
			}
		}
	}

	existing := survivor
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
	provs, err := s.assertionRepo.ListProvenanceTx(ctx, tx, loser.ID)
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
	// Same-value reaffirmation at accept time: overlapping accepted row(s) with the
	// SAME value are the same fact in different buckets → WIDEN one survivor over the
	// union of all of them + the accepting row's window, MERGE the accepting
	// (proposed) row and every bridged stint into the survivor. Mirrors the writeNew
	// widen rule (P1: Accept must widen, not supersede, a same value; and a new
	// window may bridge several same-value stints).
	acceptingSignature := assertionSignature(accepting)
	if same := sameValueConflicts(conflicts, acceptingSignature); len(same) > 0 {
		survivor := same[0]
		widenedFrom := minStart(survivor.ValidFrom, accepting.ValidFrom)
		widenedTo := maxEnd(survivor.ValidTo, accepting.ValidTo)
		// Merge every bridged stint into the survivor (union windows + provenance,
		// close them) so the single-cardinality slot keeps exactly one live row.
		for _, other := range same[1:] {
			widenedFrom = minStart(widenedFrom, other.ValidFrom)
			widenedTo = maxEnd(widenedTo, other.ValidTo)
			if err := s.mergeSameValue(ctx, tx, other, survivor); err != nil {
				return nil, false, err
			}
		}
		// Do NOT clear a pending-successor bound (superseded_by set): widening past it
		// would leave two current rows once the successor date passes (only the
		// backward lower-bound extends). Mirrors widenReaffirmation.
		if survivor.SupersededBy != nil {
			widenedTo = utcPtr(survivor.ValidTo)
		}
		survivor.ValidFrom = widenedFrom
		survivor.ValidTo = widenedTo
		// Carry the survivor's VALUE into the key recompute: for a fact (canonObject
		// nil) computePropositionKey reads the request's value, so a value-less request
		// would write an empty value component and break dedup of later same-value
		// writes. Edges ignore the value (keyed on canonObject), so this is safe there.
		widenReq := &AssertRequest{
			ValidFrom: survivor.ValidFrom,
			ValueText: survivor.ValueText,
			ValueNum:  survivor.ValueNum,
			ValueDate: survivor.ValueDate,
			ValueBool: survivor.ValueBool,
		}
		newKey := computePropositionKey(predicate, canonKey, canonSubject, canonObject, widenReq)
		// MERGE/close the accepting (loser) row FIRST so it is no longer live —
		// otherwise WidenAssertionValidity could assign the survivor a key the still-
		// live accepting row holds and 23505 on idx_assertion_live_proposition.
		if err := s.mergeSameValue(ctx, tx, accepting, survivor); err != nil {
			return nil, false, err
		}
		if err := s.assertionRepo.WidenAssertionValidityTx(ctx, tx, survivor.ID, survivor.ValidFrom, survivor.ValidTo, newKey); err != nil {
			return nil, false, fmt.Errorf("widen survivor at accept: %w", err)
		}
		survivor.PropositionKey = newKey
		return survivor, true, nil
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
	// knowledge_to >= the closed row's knowledge_from (a future KnowledgeFromOverride
	// could otherwise push knowledge_from past now → assertion_knowledge_range CHECK).
	knowledgeTo := now
	if current.KnowledgeFrom.After(knowledgeTo) {
		knowledgeTo = current.KnowledgeFrom.UTC()
	}
	closure := repository.ClosureReasonEnded
	if err := s.assertionRepo.CloseAssertionTx(ctx, tx, repository.CloseAssertionParams{
		ID:            current.ID,
		ValidTo:       &now,
		Status:        repository.AssertionStatusSuperseded,
		ClosureReason: &closure,
		SupersededBy:  nil,
		KnowledgeTo:   &knowledgeTo,
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
	// GLOBAL lock order — advisory slot lock(s) BEFORE any row lock (matches the
	// writeNew order), so Accept cannot deadlock against a concurrent assert that
	// holds the advisory lock and then row-locks via FindAcceptedForSlot FOR UPDATE.
	// A plain (unlocked) read gets the predicate + canonical slot first; the
	// authoritative status check happens after the row lock below.
	pre, err := s.assertionRepo.GetAssertionTx(ctx, tx, assertionID)
	if err != nil {
		return nil, err
	}
	predicate, err := s.predicateRepo.GetPredicate(ctx, pre.PredicateKey)
	if err != nil {
		return nil, fmt.Errorf("load predicate: %w", err)
	}
	if predicate.Cardinality == repository.PredicateCardinalitySingle {
		canonKey, canonSubject, canonObject := canonicalEdge(predicate, pre.SubjectNodeID, pre.ObjectNodeID)
		if err := s.acquireSlotLocks(ctx, tx, predicate, canonKey, canonSubject, canonObject); err != nil {
			return nil, err
		}
	}
	// Now row-lock so the proposed-status check + the accept are atomic vs a
	// concurrent Accept/Reject of the same row (re-read under the lock; a concurrent
	// transition between the plain read and here is caught by the status check).
	assertion, err := s.assertionRepo.GetAssertionForUpdateTx(ctx, tx, assertionID)
	if err != nil {
		return nil, err
	}
	if assertion.Status != repository.AssertionStatusProposed {
		return nil, validationError("assertion %s is %q, only a proposed assertion may be accepted", assertionID, assertion.Status)
	}
	// Single-cardinality conflict resolution at accept time (the slot advisory lock
	// is already held above; resolveAcceptConflicts re-acquires it re-entrantly).
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
// has been reached (the daily rollover sweep). In one tx it flips
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
// Node merge: re-point a loser node's assertions onto the winner (D9).
// --------------------------------------------------------------------------

// repointedAssertion is one loser assertion after its node references are
// rewritten loser→winner and its proposition_key recomputed. It carries the
// canonical (key, subject, object) the row would have on the normal write path,
// so the slot-lock + collision steps reuse the standard helpers.
type repointedAssertion struct {
	row          *repository.Assertion // the loser-side row (pointer into the slice)
	predicate    *repository.Predicate
	canonKey     string
	canonSubject uuid.UUID
	canonObject  *uuid.UUID
	newKey       string
	live         bool // status is proposed/accepted AND knowledge_to IS NULL
	// selfLoop is true when the rewrite collapses an edge BETWEEN the loser and the
	// winner into a self-edge (subject == object) — e.g. knows(loser, winner). A
	// self-edge is meaningless, so a live one is closed (not re-pointed) and a
	// terminal one is left as-is (dead history pointing at the tombstoned loser).
	selfLoop bool
}

// MergeAssertionsTx re-points every assertion touching the loser node onto the
// winner (D9). It is the graph half of a contact merge: the merge tx tombstones
// the loser node (merged_into=winner, deleted_at=now) and calls this to migrate
// the assertion store. It runs inside the caller's merge tx (never commits).
//
// Procedure (D9 step 1-3):
//  1. For each loser assertion, rewrite subject/object loser→winner, re-apply
//     symmetric/inverse canonicalization, and recompute proposition_key — in Go.
//  2. Lock EVERY slot implied by a recomputed LIVE single-cardinality assertion
//     (the slot may be the winner's, an unrelated node's — introduced_by(A,loser)
//     re-points to a slot owned by A, not the winner — or both participants of a
//     symmetric edge). Collect, sort by lock key (deadlock-safe), acquire each.
//  3. Per row: a TERMINAL row (closed history) is plainly re-pointed (no live
//     index to collide with, no event). A LIVE row either MERGES into a colliding
//     winner-side proposition (provenance moved, loser closed superseded — avoids
//     the idx_assertion_live_proposition 23505), or is re-pointed into the winner
//     slot and then runs the D6-step-4 valid-time supersession against any
//     different-value overlapping prior so exactly one stays current.
//
// Concurrency: the caller's merge tx tombstones the loser node (deleted_at) BEFORE
// this runs, and the single-cardinality re-points hold the per-slot advisory lock,
// so a concurrent single-card assert on an affected slot serializes. The repoint
// UPDATE is additionally savepoint-protected against a raced identical proposition
// (multi-card has no slot lock). What this does NOT guard is a writer that asserts
// a BRAND-NEW fact/edge on the loser node AFTER this one-time scan but before the
// merge commits (write-skew on the loser node) — that row would strand on the
// tombstoned loser. SP1 has NO concurrent assertion producers (extractors/agents
// are SP3/SP4; the only writers are the synchronous, user-serialized contact
// create/update/merge paths), so this cannot occur today; SP3 must add loser-node
// serialization (e.g. a node advisory lock) when concurrent producers arrive.
func (s *AssertService) MergeAssertionsTx(ctx context.Context, tx pgx.Tx, loser, winner uuid.UUID) error {
	rows, err := s.assertionRepo.ListAssertionsTouchingNodeTx(ctx, tx, loser)
	if err != nil {
		return fmt.Errorf("list loser assertions: %w", err)
	}

	// Pass 1: rewrite + recompute, and collect the slot locks the LIVE
	// single-cardinality rows imply.
	plans := make([]repointedAssertion, 0, len(rows))
	lockKeys := make(map[int64]struct{})
	for i := range rows {
		plan, err := s.planRepoint(ctx, &rows[i], loser, winner)
		if err != nil {
			return err
		}
		plans = append(plans, plan)
		// A self-loop row is closed, not re-pointed into a slot, so it implies no lock.
		if plan.live && !plan.selfLoop && plan.predicate.Cardinality == repository.PredicateCardinalitySingle {
			for _, k := range slotLockKeysFor(plan.predicate, plan.canonKey, plan.canonSubject, plan.canonObject) {
				lockKeys[k] = struct{}{}
			}
		}
	}

	// Acquire every implied slot lock in sorted order (deadlock-safe across
	// concurrent asserts touching any affected slot).
	if err := s.acquireSortedSlotLocks(ctx, tx, lockKeys); err != nil {
		return err
	}

	// Pass 2: apply each row. Process in list order (oldest-first) so a re-pointed
	// row a later row would collide with is already live at its new key.
	now := accelerated.GetCurrentTime().UTC()
	for i := range plans {
		if err := s.applyRepoint(ctx, tx, &plans[i], winner, now); err != nil {
			return err
		}
	}
	return nil
}

// planRepoint rewrites one loser assertion's node references onto the winner,
// re-canonicalizes, and recomputes its proposition_key — without writing.
func (s *AssertService) planRepoint(ctx context.Context, row *repository.Assertion, loser, winner uuid.UUID) (repointedAssertion, error) {
	predicate, err := s.predicateRepo.GetPredicate(ctx, row.PredicateKey)
	if err != nil {
		return repointedAssertion{}, fmt.Errorf("load predicate %q for merge: %w", row.PredicateKey, err)
	}

	// Rewrite loser→winner in whichever position(s) it appears (subject and object
	// cannot both be the loser — an edge connects two distinct nodes).
	newSubject := row.SubjectNodeID
	if newSubject == loser {
		newSubject = winner
	}
	var newObject *uuid.UUID
	if row.ObjectNodeID != nil {
		o := *row.ObjectNodeID
		if o == loser {
			o = winner
		}
		newObject = &o
	}

	// An edge BETWEEN loser and winner collapses to a self-edge after the rewrite
	// (both ends become the winner) — meaningless, so it is closed, not re-pointed.
	selfLoop := newObject != nil && newSubject == *newObject

	canonKey, canonSubject, canonObject := canonicalEdge(predicate, newSubject, newObject)
	newKey := computePropositionKey(predicate, canonKey, canonSubject, canonObject, assertionAsRequest(row))

	return repointedAssertion{
		row:          row,
		predicate:    predicate,
		canonKey:     canonKey,
		canonSubject: canonSubject,
		canonObject:  canonObject,
		newKey:       newKey,
		live:         isLiveAssertion(row),
		selfLoop:     selfLoop,
	}, nil
}

// applyRepoint writes one planned re-point. A terminal row is plainly re-pointed
// (its proposition_key is recomputed for consistency but never collides — the
// live-proposition index excludes terminal rows). A live row merges into a
// colliding winner proposition or re-points + supersedes (D9 step 3).
func (s *AssertService) applyRepoint(ctx context.Context, tx pgx.Tx, plan *repointedAssertion, winner uuid.UUID, now time.Time) error {
	// An edge between loser and winner collapses to a self-edge. A live one is
	// closed superseded (a person does not "know"/"partner" themselves); a terminal
	// one is left untouched as dead history (it still references the tombstoned
	// loser, resolvable via the merge alias).
	if plan.selfLoop {
		if !plan.live {
			return nil
		}
		return s.closeSelfLoop(ctx, tx, plan.row, now)
	}

	if !plan.live {
		return s.repointRow(ctx, tx, plan)
	}

	// Live row. Check for a DIFFERENT live winner-side proposition already holding
	// the recomputed key (the loser row still carries its OLD loser-based key, so a
	// hit here is genuinely another row).
	collider, err := s.assertionRepo.FindLivePropositionTx(ctx, tx, plan.newKey)
	if err != nil && !errors.Is(err, db.ErrNotFound) {
		return fmt.Errorf("merge collision check: %w", err)
	}
	if err == nil && collider.ID != plan.row.ID {
		// Collision: same proposition already live on the winner. Move the loser's
		// provenance onto it and close the loser superseded (no UPDATE into the
		// unique index → no 23505).
		return s.mergeSameValue(ctx, tx, plan.row, collider)
	}

	// No collision (as of the check above): re-point the loser row into the winner
	// slot. The check→UPDATE window is covered by a nested savepoint — if a writer
	// raced an identical live proposition onto the winner between the check and the
	// UPDATE, the repoint 23505s on idx_assertion_live_proposition; we recover by
	// re-finding the now-present collider and merging the loser into it (rather than
	// letting the unique violation abort the whole merge tx). A single-cardinality
	// repoint additionally holds the slot advisory lock, so only the multi-card path
	// can actually reach the race; the savepoint covers both uniformly.
	recovered, err := s.repointWithRecover(ctx, tx, plan)
	if err != nil {
		return err
	}
	if recovered {
		return nil
	}
	moveKind := events.KindAssertionProposed
	if plan.row.Status == repository.AssertionStatusAccepted {
		moveKind = events.KindAssertionAccepted
	}
	if err := s.emitMergeMoveEvent(ctx, tx, plan.row, moveKind, winner, now); err != nil {
		return err
	}

	// A re-pointed ACCEPTED single-cardinality row now sits in the winner's slot and
	// may overlap an accepted prior on that slot. Proposed / multi rows coexist
	// (no slot). The re-pointed row is the incoming successor (D9 step 3).
	if plan.row.Status != repository.AssertionStatusAccepted ||
		plan.predicate.Cardinality != repository.PredicateCardinalitySingle {
		return nil
	}
	effectiveFrom := now
	if plan.row.ValidFrom != nil {
		effectiveFrom = plan.row.ValidFrom.UTC()
	}
	// A past-bounded re-pointed row is historical → it coexists, never supersedes.
	if plan.row.ValidTo != nil && !plan.row.ValidTo.UTC().After(now) {
		return nil
	}
	conflicts, err := s.findOverlappingAccepted(ctx, tx, plan.predicate, plan.canonKey, plan.canonSubject, plan.canonObject, effectiveFrom, plan.row.ValidTo)
	if err != nil {
		return fmt.Errorf("merge slot overlap probe: %w", err)
	}
	// The probe (subject+predicate, or symmetric participants) returns the
	// just-re-pointed row itself — exclude it before classifying, or it would be
	// "merged into itself" / "superseded by itself".
	conflicts = excludeAssertion(conflicts, plan.row.ID)
	// Split the overlapping priors by value. SAME-value priors are the SAME fact in
	// different valid-time buckets (so they did NOT collide on proposition_key
	// above) → WIDEN the re-pointed row over their union and merge them in, per the
	// D6 same-value reaffirmation rule (not a supersession). DIFFERENT-value priors
	// are superseded by the re-pointed successor.
	signature := assertionSignature(plan.row)
	same := sameValueConflicts(conflicts, signature)
	var inheritedSuccessor *uuid.UUID
	if len(same) > 0 {
		inheritedSuccessor, err = s.widenMergedSurvivor(ctx, tx, plan, same)
		if err != nil {
			return err
		}
	}
	different := differentValueConflicts(conflicts, signature)
	// The survivor's inherited pending-future-successor is a DIFFERENT-value row
	// (the future move it is bounded by); it is the survivor's successor, NOT a
	// competitor to supersede, so exclude it from the supersession set.
	if inheritedSuccessor != nil {
		different = excludeAssertion(different, *inheritedSuccessor)
	}
	return s.closeConflicts(ctx, tx, different, plan.row, effectiveFrom, now)
}

// widenMergedSurvivor folds same-value priors (same fact in other buckets) into
// the just-re-pointed row: union the windows, merge each prior's provenance into
// the survivor + close it superseded, and recompute the survivor's
// proposition_key over the widened window. Mirrors widenReaffirmation but the
// survivor is the re-pointed merge row (already live at the winner slot).
// It returns the pending-future-successor id the survivor inherited (nil if none),
// so the caller can exclude it from the different-value supersession set — that
// successor is the survivor's own future move, not a competitor.
func (s *AssertService) widenMergedSurvivor(ctx context.Context, tx pgx.Tx, plan *repointedAssertion, same []*repository.Assertion) (*uuid.UUID, error) {
	survivor := plan.row
	widenedFrom := survivor.ValidFrom
	widenedTo := survivor.ValidTo
	// Track the tightest pending-future-successor (id + bound) across the survivor
	// AND any absorbed row: widening valid_to past that bound — or dropping its
	// superseded_by linkage — would leave the survivor AND that successor both
	// current once the successor's date passes (and the rollover worker, which keys
	// on superseded_by IS NOT NULL, would never terminalize the survivor). A
	// bounded-with-pending-successor row always carries a non-nil valid_to. We keep
	// the EARLIEST bound + its successor across all the same-value rows being folded.
	pendingBound := survivor.ValidTo
	pendingSuccessor := survivor.SupersededBy
	if survivor.SupersededBy == nil {
		pendingBound = nil
	}
	for _, other := range same {
		widenedFrom = minStart(widenedFrom, other.ValidFrom)
		widenedTo = maxEnd(widenedTo, other.ValidTo)
		if other.SupersededBy != nil && other.ValidTo != nil {
			if pendingBound == nil || other.ValidTo.Before(*pendingBound) {
				pendingBound = utcPtr(other.ValidTo)
				pendingSuccessor = other.SupersededBy
			}
		}
		if err := s.mergeSameValue(ctx, tx, other, survivor); err != nil {
			return nil, err
		}
	}
	// Cap the upper extension at the tightest pending-successor bound, if any.
	if pendingBound != nil {
		widenedTo = minEnd(widenedTo, pendingBound)
	}
	widenReq := assertionAsRequest(survivor)
	widenReq.ValidFrom = widenedFrom
	newKey := computePropositionKey(plan.predicate, plan.canonKey, plan.canonSubject, plan.canonObject, widenReq)
	if err := s.assertionRepo.WidenAssertionValidityTx(ctx, tx, survivor.ID, widenedFrom, widenedTo, newKey); err != nil {
		return nil, fmt.Errorf("widen merged survivor: %w", err)
	}
	// Inherit the pending-successor linkage (superseded_by + the capped bound) so the
	// rollover worker terminalizes the survivor when the successor's date arrives.
	if pendingSuccessor != nil && survivor.SupersededBy == nil {
		if err := s.assertionRepo.BoundPendingSuccessorTx(ctx, tx, survivor.ID, *pendingBound, *pendingSuccessor); err != nil {
			return nil, fmt.Errorf("inherit pending successor on merged survivor: %w", err)
		}
		survivor.SupersededBy = pendingSuccessor
	}
	survivor.ValidFrom = widenedFrom
	survivor.ValidTo = widenedTo
	survivor.PropositionKey = newKey
	return pendingSuccessor, nil
}

// excludeAssertion returns conflicts with the row matching id removed (the
// overlap probe returns the just-re-pointed row itself, which must not be
// classified as its own same/different-value prior).
func excludeAssertion(conflicts []repository.Assertion, id uuid.UUID) []repository.Assertion {
	out := make([]repository.Assertion, 0, len(conflicts))
	for i := range conflicts {
		if conflicts[i].ID != id {
			out = append(out, conflicts[i])
		}
	}
	return out
}

// differentValueConflicts returns the conflict rows whose value differs from
// signature (the complement of sameValueConflicts).
func differentValueConflicts(conflicts []repository.Assertion, signature string) []repository.Assertion {
	out := make([]repository.Assertion, 0, len(conflicts))
	for i := range conflicts {
		if assertionSignature(&conflicts[i]) != signature {
			out = append(out, conflicts[i])
		}
	}
	return out
}

// emitMergeMoveEvent emits the transition event for a row re-pointed onto the
// winner during a merge, so SP3/SP4 derived signals recompute against the new
// subject. It carries the row's live kind (accepted/proposed) and is keyed by a
// DEDICATED '<assertion_id>:merged:<winner_id>' source_id — NOT the one-shot
// ':accepted'/':proposed' token the row already emitted on its original insert —
// so the move is a genuinely new event (not deduped against the insert). Keying
// by the WINNER (not a bare ':merged') keeps a retry of the SAME merge idempotent
// while still emitting a fresh event for a CHAINED merge (A→B then B→C moves the
// row a second time, to a different winner).
func (s *AssertService) emitMergeMoveEvent(ctx context.Context, tx pgx.Tx, row *repository.Assertion, kind events.Kind, winner uuid.UUID, now time.Time) error {
	return s.publishAssertionEnvelope(ctx, tx, kind, row, row.ID.String()+":merged:"+winner.String(), now)
}

// repointRow applies the node-reference UPDATE (subject and/or object) for a
// planned re-point, stamping the recomputed proposition_key. The merge service
// reaches RepointAssertionSubject/Object ONLY here.
func (s *AssertService) repointRow(ctx context.Context, tx pgx.Tx, plan *repointedAssertion) error {
	row := plan.row
	if row.SubjectNodeID != plan.canonSubject {
		if err := s.assertionRepo.RepointAssertionSubjectTx(ctx, tx, row.ID, plan.canonSubject, plan.newKey); err != nil {
			return fmt.Errorf("repoint assertion subject: %w", err)
		}
		row.SubjectNodeID = plan.canonSubject
		row.PropositionKey = plan.newKey
	}
	if plan.canonObject != nil && (row.ObjectNodeID == nil || *row.ObjectNodeID != *plan.canonObject) {
		if err := s.assertionRepo.RepointAssertionObjectTx(ctx, tx, row.ID, *plan.canonObject, plan.newKey); err != nil {
			return fmt.Errorf("repoint assertion object: %w", err)
		}
		o := *plan.canonObject
		row.ObjectNodeID = &o
		row.PropositionKey = plan.newKey
	}
	// Inverse/symmetric canonicalization can swap subject↔object without changing the
	// stored id set, leaving the key recomputed but neither UPDATE above firing (e.g.
	// a symmetric edge whose pair order is unchanged). Persist the recomputed key so
	// it always reflects the canonical orientation.
	if row.PropositionKey != plan.newKey {
		if err := s.assertionRepo.RepointAssertionSubjectTx(ctx, tx, row.ID, row.SubjectNodeID, plan.newKey); err != nil {
			return fmt.Errorf("repoint assertion key: %w", err)
		}
		row.PropositionKey = plan.newKey
	}
	return nil
}

// repointWithRecover re-points a live loser row inside a nested savepoint. On a
// 23505 against idx_assertion_live_proposition (a concurrent writer raced an
// identical live proposition onto the winner after applyRepoint's collision check
// but before this UPDATE), it rolls back JUST the savepoint (the outer merge tx
// stays usable), re-reads the now-present collider, and merges the loser into it —
// returning recovered=true. Any other error propagates. recovered=false means the
// re-point committed normally and the caller continues with the move event +
// supersession.
func (s *AssertService) repointWithRecover(ctx context.Context, tx pgx.Tx, plan *repointedAssertion) (recovered bool, err error) {
	sp, err := tx.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin repoint savepoint: %w", err)
	}
	if err := s.repointRow(ctx, sp, plan); err != nil {
		_ = sp.Rollback(ctx)
		if isLivePropositionViolation(err) {
			collider, ferr := s.assertionRepo.FindLivePropositionTx(ctx, tx, plan.newKey)
			if ferr != nil {
				return false, fmt.Errorf("re-find collider after merge repoint conflict: %w", ferr)
			}
			if err := s.mergeSameValue(ctx, tx, plan.row, collider); err != nil {
				return false, err
			}
			return true, nil
		}
		return false, err
	}
	if err := sp.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit repoint savepoint: %w", err)
	}
	return false, nil
}

// closeSelfLoop terminalizes a live edge that collapsed to a self-edge after the
// merge rewrite (it can never sanely be live). It closes the row superseded with
// no successor (closure_reason='ended', like a slot closure) and emits the event,
// leaving the row's node references at the loser (dead history). knowledge_to is
// clamped >= knowledge_from for the assertion_knowledge_range CHECK.
func (s *AssertService) closeSelfLoop(ctx context.Context, tx pgx.Tx, row *repository.Assertion, now time.Time) error {
	knowledgeTo := now
	if row.KnowledgeFrom.After(knowledgeTo) {
		knowledgeTo = row.KnowledgeFrom.UTC()
	}
	// valid_to = now closes a currently-true edge. But a FUTURE-dated edge
	// (valid_from > now) would get valid_to < valid_from → the assertion_valid_range
	// CHECK fails. For such a never-yet-current row keep its EXISTING valid_to (it is
	// terminal regardless), mirroring closeBoundary on the supersession path.
	validTo := &now
	if row.ValidFrom != nil && !now.After(*row.ValidFrom) {
		validTo = utcPtr(row.ValidTo)
	}
	closure := repository.ClosureReasonEnded
	if err := s.assertionRepo.CloseAssertionTx(ctx, tx, repository.CloseAssertionParams{
		ID:            row.ID,
		ValidTo:       validTo,
		Status:        repository.AssertionStatusSuperseded,
		ClosureReason: &closure,
		SupersededBy:  nil,
		KnowledgeTo:   &knowledgeTo,
	}); err != nil {
		return fmt.Errorf("close merge self-loop: %w", err)
	}
	return s.emitAssertionEvent(ctx, tx, events.KindAssertionSuperseded, row, now)
}

// slotLockKeysFor returns the advisory slot-lock key(s) a single-cardinality
// recomputed assertion implies: asymmetric → one on (subject, canonical
// predicate); symmetric edge → one per participant. Mirrors acquireSlotLocks's
// key derivation (without taking the lock) so the merge can collect every
// implied slot up front.
func slotLockKeysFor(predicate *repository.Predicate, canonKey string, canonSubject uuid.UUID, canonObject *uuid.UUID) []int64 {
	if predicate.Symmetric && canonObject != nil {
		return []int64{slotLockKey(canonKey, canonSubject), slotLockKey(canonKey, *canonObject)}
	}
	return []int64{slotLockKey(canonKey, canonSubject)}
}

// acquireSortedSlotLocks takes every collected slot lock in ascending key order
// (deadlock-safe).
func (s *AssertService) acquireSortedSlotLocks(ctx context.Context, tx pgx.Tx, keys map[int64]struct{}) error {
	ordered := make([]int64, 0, len(keys))
	for k := range keys {
		ordered = append(ordered, k)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i] < ordered[j] })
	for _, k := range ordered {
		if err := s.assertionRepo.AcquirePropositionSlotLockTx(ctx, tx, k); err != nil {
			return fmt.Errorf("acquire merge slot lock: %w", err)
		}
	}
	return nil
}

// isLiveAssertion reports whether a stored row is live (proposed/accepted and
// knowledge-open) — i.e. it participates in idx_assertion_live_proposition.
func isLiveAssertion(a *repository.Assertion) bool {
	return (a.Status == repository.AssertionStatusProposed || a.Status == repository.AssertionStatusAccepted) &&
		a.KnowledgeTo == nil
}

// assertionAsRequest projects a stored assertion's payload + valid_from into the
// minimal AssertRequest computePropositionKey reads (the fact value fields for a
// fact, valid_from for the bucket). The object is keyed via canonObject, so it is
// not carried here.
func assertionAsRequest(a *repository.Assertion) *AssertRequest {
	return &AssertRequest{
		ValueText: a.ValueText,
		ValueNum:  a.ValueNum,
		ValueDate: a.ValueDate,
		ValueBool: a.ValueBool,
		ValidFrom: a.ValidFrom,
	}
}

// EnsurePlaceTx find-or-creates the place entity node for a location label and
// returns its node id. The entity is resolved by (subtype='place',
// normalized_name=lower(trim(label))) so repeated asserts of the same place
// (across contacts or edits) collapse to one place node. A fresh place mints a
// node + entity pair in the caller's tx. The node's canonical_label preserves
// the original casing/spacing (it is what the location cache column displays);
// normalized_name is the lowercased dedup key.
//
// Concurrency note: two concurrent first-asserts of the same brand-new place can
// race the find→create window and hit the entity (subtype, normalized_name)
// unique on the second insert (23505). The node+entity insert runs in a nested
// savepoint, so the loser rolls back JUST the savepoint (the outer tx stays
// usable), re-finds the winner's place node, and returns it — no caller-visible
// error. The backfill command is single-threaded, so it never races itself.
func (s *AssertService) EnsurePlaceTx(ctx context.Context, tx pgx.Tx, label string) (uuid.UUID, error) {
	normalized := strings.ToLower(strings.TrimSpace(label))
	if normalized == "" {
		return uuid.Nil, validationError("place label is empty")
	}
	existing, err := s.entityRepo.FindEntityBySubtypeNameTx(ctx, tx, repository.EntitySubtypePlace, normalized)
	if err == nil {
		return existing.NodeID, nil
	}
	if !errors.Is(err, db.ErrNotFound) {
		return uuid.Nil, fmt.Errorf("find place entity: %w", err)
	}

	// Create node + entity inside a nested savepoint so a concurrent writer that
	// raced us to the same (subtype, normalized_name) — two contacts asserting the
	// same brand-new place in flight — surfaces as a 23505 we can recover from by
	// re-finding the winner's node, WITHOUT aborting the outer tx.
	id := uuid.New()
	sp, err := tx.Begin(ctx)
	if err != nil {
		return uuid.Nil, fmt.Errorf("begin place savepoint: %w", err)
	}
	if _, err := s.nodeRepo.CreateNodeTx(ctx, sp, id, repository.NodeTypeEntity, strings.TrimSpace(label)); err != nil {
		_ = sp.Rollback(ctx)
		return uuid.Nil, fmt.Errorf("create place node: %w", err)
	}
	if _, err := s.entityRepo.CreateEntityTx(ctx, sp, repository.CreateEntityRequest{
		NodeID:         id,
		Subtype:        repository.EntitySubtypePlace,
		NormalizedName: normalized,
	}); err != nil {
		_ = sp.Rollback(ctx)
		if isPlaceNameViolation(err) {
			// The winner created the place between our find and our insert; re-find
			// on the outer tx and return its node id.
			winner, findErr := s.entityRepo.FindEntityBySubtypeNameTx(ctx, tx, repository.EntitySubtypePlace, normalized)
			if findErr != nil {
				return uuid.Nil, fmt.Errorf("re-find place after conflict: %w", findErr)
			}
			return winner.NodeID, nil
		}
		return uuid.Nil, fmt.Errorf("create place entity: %w", err)
	}
	if err := sp.Commit(ctx); err != nil {
		return uuid.Nil, fmt.Errorf("commit place savepoint: %w", err)
	}
	return id, nil
}

// isPlaceNameViolation reports whether err is a 23505 on the entity
// (subtype, normalized_name) unique index — the concurrent same-place race.
func isPlaceNameViolation(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != pgerrcode.UniqueViolation {
		return false
	}
	return pgErr.ConstraintName == "idx_entity_subtype_name"
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

// minEnd returns the EARLIER of two valid_to bounds; nil = open (+inf), which is
// the maximum, so a nil side loses (the other, finite side wins). Two nils → nil.
func minEnd(a, b *time.Time) *time.Time {
	if a == nil {
		return utcPtr(b)
	}
	if b == nil {
		return utcPtr(a)
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
