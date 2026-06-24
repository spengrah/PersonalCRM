package repository

import (
	"testing"

	"personal-crm/backend/internal/db"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Pure unit coverage of the predicate converter: it translates a generated read
// row (pgtype-wrapped) into the domain struct, surfacing NULL nullable fields as
// nil pointers. Reads never project the embedding column (the nullable vector is
// not selected — see predicate.sql), so Embedding is always nil here. DB-backed
// round-trips and the CHECK rejections live in the integration suite.

func TestConvertPredicateRow(t *testing.T) {
	t.Run("edge predicate with inverse", func(t *testing.T) {
		got := convertPredicateRow(predicateRowFromGet(&db.GetPredicateRow{
			Key:                 "parent_of",
			Kind:                PredicateKindEdge,
			SubjectType:         "person",
			ObjectType:          pgtype.Text{String: "person", Valid: true},
			ValueType:           pgtype.Text{Valid: false},
			Cardinality:         PredicateCardinalityMulti,
			Symmetric:           false,
			InversePredicate:    pgtype.Text{String: "child_of", Valid: true},
			TemporalProfile:     PredicateTemporalPermanent,
			BaseRateDays:        pgtype.Int4{Valid: false},
			TypicalDurationDays: pgtype.Int4{Valid: false},
			DefaultSalience:     80,
			DefaultReviewPolicy: PredicateReviewAlwaysConfirm,
			PropositionBucket:   PredicateBucketNone,
			Status:              PredicateStatusCurated,
			Description:         "Person is a parent of another person",
			Synonyms:            []string{},
		}))
		assert.Equal(t, "parent_of", got.Key)
		assert.Equal(t, PredicateKindEdge, got.Kind)
		require.NotNil(t, got.ObjectType)
		assert.Equal(t, "person", *got.ObjectType)
		assert.Nil(t, got.ValueType)
		require.NotNil(t, got.InversePredicate)
		assert.Equal(t, "child_of", *got.InversePredicate)
		assert.Nil(t, got.BaseRateDays)
		assert.Nil(t, got.TypicalDurationDays)
		assert.Equal(t, int16(80), got.DefaultSalience)
		assert.Equal(t, PredicateReviewAlwaysConfirm, got.DefaultReviewPolicy)
		assert.Equal(t, PredicateBucketNone, got.PropositionBucket)
		// Reads never select the embedding column, so it is always nil here.
		assert.Nil(t, got.Embedding)
	})

	t.Run("fact predicate with priors", func(t *testing.T) {
		base := int32(0)
		dur := int32(180)
		got := convertPredicateRow(predicateRowFromGet(&db.GetPredicateRow{
			Key:                 "job_seeking",
			Kind:                PredicateKindFact,
			SubjectType:         "person",
			ObjectType:          pgtype.Text{Valid: false},
			ValueType:           pgtype.Text{String: PredicateValueTypeBool, Valid: true},
			Cardinality:         PredicateCardinalitySingle,
			TemporalProfile:     PredicateTemporalBounded,
			BaseRateDays:        pgtype.Int4{Int32: base, Valid: true},
			TypicalDurationDays: pgtype.Int4{Int32: dur, Valid: true},
			DefaultSalience:     60,
			DefaultReviewPolicy: PredicateReviewAutoIfConfident,
			PropositionBucket:   PredicateBucketDay,
			Status:              PredicateStatusCurated,
			Synonyms:            []string{},
		}))
		assert.Nil(t, got.ObjectType)
		require.NotNil(t, got.ValueType)
		assert.Equal(t, PredicateValueTypeBool, *got.ValueType)
		require.NotNil(t, got.BaseRateDays)
		assert.Equal(t, base, *got.BaseRateDays)
		require.NotNil(t, got.TypicalDurationDays)
		assert.Equal(t, dur, *got.TypicalDurationDays)
	})

	t.Run("each generated row adapter normalizes onto the same shape", func(t *testing.T) {
		// The four generated row types are field-identical; the adapters convert
		// them onto predicateRow so the single converter handles every read path.
		create := convertPredicateRow(predicateRowFromCreate(&db.CreatePredicateRow{
			Key: "x", Kind: PredicateKindFact, SubjectType: "person",
			ValueType:   pgtype.Text{String: PredicateValueTypeText, Valid: true},
			Cardinality: PredicateCardinalitySingle, TemporalProfile: PredicateTemporalMutable,
			DefaultReviewPolicy: PredicateReviewAutoIfConfident, PropositionBucket: PredicateBucketDay,
			Status: PredicateStatusProvisional, Synonyms: []string{},
		}))
		assert.Equal(t, "x", create.Key)

		list := convertPredicateRow(predicateRowFromList(&db.ListPredicatesByStatusRow{
			Key: "y", Kind: PredicateKindEdge, SubjectType: "person",
			ObjectType:  pgtype.Text{String: "person", Valid: true},
			Cardinality: PredicateCardinalityMulti, TemporalProfile: PredicateTemporalMutable,
			DefaultReviewPolicy: PredicateReviewAutoIfConfident, PropositionBucket: PredicateBucketNone,
			Status: PredicateStatusCurated, Synonyms: []string{},
		}))
		assert.Equal(t, "y", list.Key)

		curated := convertPredicateRow(predicateRowFromCurated(&db.ListCuratedPredicatesRow{
			Key: "z", Kind: PredicateKindFact, SubjectType: "person",
			ValueType:   pgtype.Text{String: PredicateValueTypeDate, Valid: true},
			Cardinality: PredicateCardinalitySingle, TemporalProfile: PredicateTemporalPermanent,
			DefaultReviewPolicy: PredicateReviewAutoIfConfident, PropositionBucket: PredicateBucketNone,
			Status: PredicateStatusCurated, Synonyms: []string{},
		}))
		assert.Equal(t, "z", curated.Key)
	})
}
