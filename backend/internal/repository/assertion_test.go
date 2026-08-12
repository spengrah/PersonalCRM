package repository

import (
	"testing"
	"time"

	"personal-crm/backend/internal/db"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Pure unit coverage of the assertion-store converters: they translate generated
// db.* rows into the domain structs, handling the nullable payload / temporal /
// supersession fields. DB-backed round-trips + the CHECK constraints live in the
// integration suite.

func TestConvertDbAssertion(t *testing.T) {
	id := uuid.New()
	subject := uuid.New()
	object := uuid.New()
	successor := uuid.New()
	knowledgeFrom := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	validFrom := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	created := time.Date(2026, 6, 1, 12, 0, 0, 1, time.UTC)

	t.Run("edge assertion, open-ended, accepted", func(t *testing.T) {
		got := convertDbAssertion(&db.Assertion{
			ID:             id,
			SubjectNodeID:  subject,
			PredicateKey:   "partner_of",
			ObjectNodeID:   &object,
			KnowledgeFrom:  knowledgeFrom,
			Confidence:     90,
			Salience:       85,
			Status:         AssertionStatusAccepted,
			PropositionKey: "key-1",
			CreatedAt:      created,
		})
		assert.Equal(t, id, got.ID)
		assert.Equal(t, subject, got.SubjectNodeID)
		require.NotNil(t, got.ObjectNodeID)
		assert.Equal(t, object, *got.ObjectNodeID)
		assert.Nil(t, got.ValueText)
		assert.Nil(t, got.ValueNum)
		assert.Nil(t, got.ValueDate)
		assert.Nil(t, got.ValueBool)
		assert.Nil(t, got.ValidFrom)
		assert.Nil(t, got.ValidTo)
		assert.Equal(t, knowledgeFrom, got.KnowledgeFrom)
		assert.Nil(t, got.KnowledgeTo)
		assert.Nil(t, got.ClosureReason)
		assert.Nil(t, got.SupersededBy)
		assert.Nil(t, got.TrustTier)
		assert.Equal(t, AssertionStatusAccepted, got.Status)
	})

	t.Run("fact assertion with all nullable fields set, superseded", func(t *testing.T) {
		valueText := "123 Main St"
		validTo := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
		knowledgeTo := time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC)
		closure := ClosureReasonSuperseded
		trust := "user"
		got := convertDbAssertion(&db.Assertion{
			ID:             id,
			SubjectNodeID:  subject,
			PredicateKey:   "home_address",
			ValueText:      &valueText,
			ValidFrom:      &validFrom,
			ValidTo:        &validTo,
			KnowledgeFrom:  knowledgeFrom,
			KnowledgeTo:    &knowledgeTo,
			Confidence:     70,
			Salience:       45,
			Status:         AssertionStatusSuperseded,
			ClosureReason:  &closure,
			SupersededBy:   &successor,
			TrustTier:      &trust,
			PropositionKey: "key-2",
			CreatedAt:      created,
		})
		require.NotNil(t, got.ValueText)
		assert.Equal(t, "123 Main St", *got.ValueText)
		assert.Nil(t, got.ObjectNodeID)
		require.NotNil(t, got.ValidFrom)
		assert.Equal(t, validFrom, *got.ValidFrom)
		require.NotNil(t, got.ValidTo)
		assert.Equal(t, validTo, *got.ValidTo)
		require.NotNil(t, got.KnowledgeTo)
		assert.Equal(t, knowledgeTo, *got.KnowledgeTo)
		require.NotNil(t, got.ClosureReason)
		assert.Equal(t, closure, *got.ClosureReason)
		require.NotNil(t, got.SupersededBy)
		assert.Equal(t, successor, *got.SupersededBy)
		require.NotNil(t, got.TrustTier)
		assert.Equal(t, trust, *got.TrustTier)
	})

	t.Run("numeric + bool + date payloads convert", func(t *testing.T) {
		numVal := 42.5
		num := convertDbAssertion(&db.Assertion{
			ValueNum:      &numVal,
			KnowledgeFrom: knowledgeFrom,
			Status:        AssertionStatusProposed,
		})
		require.NotNil(t, num.ValueNum)
		assert.Equal(t, 42.5, *num.ValueNum)

		boolVal := true
		b := convertDbAssertion(&db.Assertion{
			ValueBool:     &boolVal,
			KnowledgeFrom: knowledgeFrom,
			Status:        AssertionStatusProposed,
		})
		require.NotNil(t, b.ValueBool)
		assert.True(t, *b.ValueBool)

		d := time.Date(1990, 3, 14, 0, 0, 0, 0, time.UTC)
		dateAsrt := convertDbAssertion(&db.Assertion{
			ValueDate:     &d,
			KnowledgeFrom: knowledgeFrom,
			Status:        AssertionStatusProposed,
		})
		require.NotNil(t, dateAsrt.ValueDate)
		assert.Equal(t, d, *dateAsrt.ValueDate)
	})
}

func TestConvertDbProvenance(t *testing.T) {
	assertionID := uuid.New()
	field := "body"
	chunk := "chunk-7"
	quote := "we live in Brooklyn now"

	t.Run("full locator with span + quote", func(t *testing.T) {
		startOffset := int32(10)
		endOffset := int32(30)
		got := convertDbProvenance(&db.AssertionProvenance{
			AssertionID:     assertionID,
			LocatorHash:     "hash-abc",
			SourceKind:      SourceKindCommsMessage,
			SourceID:        "msg-1",
			ProducerKind:    ProducerKindExtractor,
			ProducerVersion: "v2",
			Field:           &field,
			StartOffset:     &startOffset,
			EndOffset:       &endOffset,
			ChunkID:         &chunk,
			InputHash:       "input-hash-1",
			Quote:           &quote,
		})
		assert.Equal(t, assertionID, got.AssertionID)
		assert.Equal(t, "hash-abc", got.LocatorHash)
		assert.Equal(t, SourceKindCommsMessage, got.SourceKind)
		assert.Equal(t, ProducerKindExtractor, got.ProducerKind)
		assert.Equal(t, "v2", got.ProducerVersion)
		require.NotNil(t, got.Field)
		assert.Equal(t, field, *got.Field)
		require.NotNil(t, got.StartOffset)
		assert.Equal(t, int32(10), *got.StartOffset)
		require.NotNil(t, got.EndOffset)
		assert.Equal(t, int32(30), *got.EndOffset)
		require.NotNil(t, got.ChunkID)
		assert.Equal(t, chunk, *got.ChunkID)
		assert.Equal(t, "input-hash-1", got.InputHash)
		require.NotNil(t, got.Quote)
		assert.Equal(t, quote, *got.Quote)
	})

	t.Run("user locator with no span/chunk/quote", func(t *testing.T) {
		got := convertDbProvenance(&db.AssertionProvenance{
			AssertionID:  assertionID,
			LocatorHash:  "hash-user",
			SourceKind:   SourceKindUser,
			SourceID:     "edit:contact-1:home_address",
			ProducerKind: ProducerKindUser,
		})
		assert.Nil(t, got.Field)
		assert.Nil(t, got.StartOffset)
		assert.Nil(t, got.EndOffset)
		assert.Nil(t, got.ChunkID)
		assert.Nil(t, got.Quote)
		assert.Equal(t, SourceKindUser, got.SourceKind)
	})
}
