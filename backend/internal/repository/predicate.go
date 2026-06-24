package repository

import (
	"context"
	"errors"

	"personal-crm/backend/internal/db"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/pgvector/pgvector-go"
)

// Predicate kind constants. An edge predicate points at another node; a fact
// predicate carries a typed scalar.
const (
	PredicateKindEdge = "edge"
	PredicateKindFact = "fact"
)

// Predicate value-type constants (the typed-scalar shapes a fact predicate may
// carry).
const (
	PredicateValueTypeText = "text"
	PredicateValueTypeNum  = "num"
	PredicateValueTypeDate = "date"
	PredicateValueTypeBool = "bool"
)

// Predicate cardinality constants.
const (
	PredicateCardinalitySingle = "single"
	PredicateCardinalityMulti  = "multi"
)

// Predicate temporal-profile constants.
const (
	PredicateTemporalPermanent = "permanent"
	PredicateTemporalMutable   = "mutable"
	PredicateTemporalBounded   = "bounded"
)

// Predicate review-policy constants.
const (
	PredicateReviewAutoIfConfident = "auto-if-confident"
	PredicateReviewAlwaysConfirm   = "always-confirm"
)

// Predicate proposition-bucket constants (the valid-time dedup-key granularity).
const (
	PredicateBucketDay   = "day"
	PredicateBucketMonth = "month"
	PredicateBucketYear  = "year"
	PredicateBucketNone  = "none"
)

// Predicate status constants.
const (
	PredicateStatusCurated     = "curated"
	PredicateStatusProvisional = "provisional"
)

// Predicate is a catalog row declaring an edge or fact type the assertion store
// can reference. ObjectType is set for edges, ValueType for facts (the kind/
// payload CHECK enforces exactly one). InversePredicate, BaseRateDays,
// TypicalDurationDays, and Embedding are nullable; Embedding is populated
// separately (NULL until then).
type Predicate struct {
	Key                 string           `json:"key"`
	Kind                string           `json:"kind"`
	SubjectType         string           `json:"subject_type"`
	ObjectType          *string          `json:"object_type,omitempty"`
	ValueType           *string          `json:"value_type,omitempty"`
	Cardinality         string           `json:"cardinality"`
	Symmetric           bool             `json:"symmetric"`
	InversePredicate    *string          `json:"inverse_predicate,omitempty"`
	TemporalProfile     string           `json:"temporal_profile"`
	BaseRateDays        *int32           `json:"base_rate_days,omitempty"`
	TypicalDurationDays *int32           `json:"typical_duration_days,omitempty"`
	DefaultSalience     int16            `json:"default_salience"`
	DefaultReviewPolicy string           `json:"default_review_policy"`
	PropositionBucket   string           `json:"proposition_bucket"`
	Status              string           `json:"status"`
	Description         string           `json:"description"`
	Synonyms            []string         `json:"synonyms"`
	Embedding           *pgvector.Vector `json:"embedding,omitempty"`
}

// CreatePredicateRequest is the input for minting a (typically provisional)
// predicate. Nullable fields are pointers; an embedding is never set here (it
// defaults to NULL).
type CreatePredicateRequest struct {
	Key                 string
	Kind                string
	SubjectType         string
	ObjectType          *string
	ValueType           *string
	Cardinality         string
	Symmetric           bool
	InversePredicate    *string
	TemporalProfile     string
	BaseRateDays        *int32
	TypicalDurationDays *int32
	DefaultSalience     int16
	DefaultReviewPolicy string
	PropositionBucket   string
	Status              string
	Description         string
	Synonyms            []string
}

// PredicateRepository handles predicate-catalog persistence.
type PredicateRepository struct {
	queries db.Querier
}

// NewPredicateRepository creates a new PredicateRepository.
func NewPredicateRepository(queries db.Querier) *PredicateRepository {
	return &PredicateRepository{queries: queries}
}

// predicateRow holds the columns the read/RETURNING queries project (every
// predicate column EXCEPT embedding — see predicate.sql for why the nullable
// vector is never selected here). The four generated row types are
// field-identical; the small adapters below normalize them onto this one shape
// so a single converter (convertPredicateRow) builds the domain struct.
type predicateRow struct {
	Key                 string
	Kind                string
	SubjectType         string
	ObjectType          pgtype.Text
	ValueType           pgtype.Text
	Cardinality         string
	Symmetric           bool
	InversePredicate    pgtype.Text
	TemporalProfile     string
	BaseRateDays        pgtype.Int4
	TypicalDurationDays pgtype.Int4
	DefaultSalience     int16
	DefaultReviewPolicy string
	PropositionBucket   string
	Status              string
	Description         string
	Synonyms            []string
	CreatedAt           pgtype.Timestamptz
}

func predicateRowFromGet(r *db.GetPredicateRow) predicateRow {
	return predicateRow(*r)
}

func predicateRowFromCreate(r *db.CreatePredicateRow) predicateRow {
	return predicateRow(*r)
}

func predicateRowFromList(r *db.ListPredicatesByStatusRow) predicateRow {
	return predicateRow(*r)
}

func predicateRowFromCurated(r *db.ListCuratedPredicatesRow) predicateRow {
	return predicateRow(*r)
}

// convertPredicateRow builds the domain Predicate from a read row. Embedding is
// always nil here — this layer never selects the column (it is populated and
// read in a later layer).
func convertPredicateRow(row predicateRow) Predicate {
	predicate := Predicate{
		Key:                 row.Key,
		Kind:                row.Kind,
		SubjectType:         row.SubjectType,
		Cardinality:         row.Cardinality,
		Symmetric:           row.Symmetric,
		TemporalProfile:     row.TemporalProfile,
		DefaultSalience:     row.DefaultSalience,
		DefaultReviewPolicy: row.DefaultReviewPolicy,
		PropositionBucket:   row.PropositionBucket,
		Status:              row.Status,
		Description:         row.Description,
		Synonyms:            row.Synonyms,
	}
	if row.ObjectType.Valid {
		predicate.ObjectType = &row.ObjectType.String
	}
	if row.ValueType.Valid {
		predicate.ValueType = &row.ValueType.String
	}
	if row.InversePredicate.Valid {
		predicate.InversePredicate = &row.InversePredicate.String
	}
	if row.BaseRateDays.Valid {
		predicate.BaseRateDays = &row.BaseRateDays.Int32
	}
	if row.TypicalDurationDays.Valid {
		predicate.TypicalDurationDays = &row.TypicalDurationDays.Int32
	}
	return predicate
}

// GetPredicate retrieves a predicate by key.
func (r *PredicateRepository) GetPredicate(ctx context.Context, key string) (*Predicate, error) {
	row, err := r.queries.GetPredicate(ctx, key)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, err
	}
	predicate := convertPredicateRow(predicateRowFromGet(row))
	return &predicate, nil
}

// ListCurated returns all curated-core predicates, ordered by key.
func (r *PredicateRepository) ListCurated(ctx context.Context) ([]Predicate, error) {
	rows, err := r.queries.ListCuratedPredicates(ctx)
	if err != nil {
		return nil, err
	}
	predicates := make([]Predicate, 0, len(rows))
	for _, row := range rows {
		predicates = append(predicates, convertPredicateRow(predicateRowFromCurated(row)))
	}
	return predicates, nil
}

// ListByStatus returns all predicates with the given status, ordered by key.
func (r *PredicateRepository) ListByStatus(ctx context.Context, status string) ([]Predicate, error) {
	rows, err := r.queries.ListPredicatesByStatus(ctx, status)
	if err != nil {
		return nil, err
	}
	predicates := make([]Predicate, 0, len(rows))
	for _, row := range rows {
		predicates = append(predicates, convertPredicateRow(predicateRowFromList(row)))
	}
	return predicates, nil
}

// CreateProvisional mints a predicate (the embedding stays NULL). The caller
// supplies the full typing; the kind/payload CHECK rejects an inconsistent row.
func (r *PredicateRepository) CreateProvisional(ctx context.Context, req CreatePredicateRequest) (*Predicate, error) {
	// A nil []string sent to the NOT NULL synonyms column inserts SQL NULL (not
	// the column DEFAULT), so substitute an empty array to preserve the contract.
	synonyms := req.Synonyms
	if synonyms == nil {
		synonyms = []string{}
	}
	row, err := r.queries.CreatePredicate(ctx, db.CreatePredicateParams{
		Key:                 req.Key,
		Kind:                req.Kind,
		SubjectType:         req.SubjectType,
		ObjectType:          stringToPgText(req.ObjectType),
		ValueType:           stringToPgText(req.ValueType),
		Cardinality:         req.Cardinality,
		Symmetric:           req.Symmetric,
		InversePredicate:    stringToPgText(req.InversePredicate),
		TemporalProfile:     req.TemporalProfile,
		BaseRateDays:        int32ToPgInt4(req.BaseRateDays),
		TypicalDurationDays: int32ToPgInt4(req.TypicalDurationDays),
		DefaultSalience:     req.DefaultSalience,
		DefaultReviewPolicy: req.DefaultReviewPolicy,
		PropositionBucket:   req.PropositionBucket,
		Status:              req.Status,
		Description:         req.Description,
		Synonyms:            synonyms,
	})
	if err != nil {
		return nil, err
	}
	predicate := convertPredicateRow(predicateRowFromCreate(row))
	return &predicate, nil
}
