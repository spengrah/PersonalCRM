package service

import (
	"testing"
	"time"

	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestValidateFactValue covers the pure fact-payload checks: the value column for
// the predicate's value_type is set, parses, and (text) is non-empty after trim;
// exactly one scalar; no mismatched scalar.
func TestValidateFactValue(t *testing.T) {
	textPred := factPredicate("how_met", repository.PredicateValueTypeText, repository.PredicateBucketNone)
	numPred := factPredicate("score", repository.PredicateValueTypeNum, repository.PredicateBucketNone)
	datePred := factPredicate("birthday", repository.PredicateValueTypeDate, repository.PredicateBucketNone)
	boolPred := factPredicate("traveling", repository.PredicateValueTypeBool, repository.PredicateBucketDay)

	tests := []struct {
		name    string
		req     *AssertRequest
		pred    *repository.Predicate
		wantErr bool
	}{
		{"text ok", &AssertRequest{ValueText: strPtr("met at a wedding")}, textPred, false},
		{"text empty after trim", &AssertRequest{ValueText: strPtr("   ")}, textPred, true},
		{"text wrong type for num pred", &AssertRequest{ValueText: strPtr("x")}, numPred, true},
		{"num ok", &AssertRequest{ValueNum: numPtr(42)}, numPred, false},
		{"num missing", &AssertRequest{ValueText: strPtr("x")}, numPred, true},
		{"date ok", &AssertRequest{ValueDate: datePtr(time.Now())}, datePred, false},
		{"bool ok", &AssertRequest{ValueBool: boolPtr(true)}, boolPred, false},
		{"two scalars set", &AssertRequest{ValueText: strPtr("x"), ValueNum: numPtr(1)}, textPred, true},
		{"no scalar set", &AssertRequest{}, textPred, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateFactValue(tc.req, tc.pred)
			if tc.wantErr {
				require.Error(t, err)
				require.ErrorIs(t, err, ErrAssertValidation)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

// TestScalarCount counts the value_* fields set.
func TestScalarCount(t *testing.T) {
	assert.Equal(t, 0, scalarCount(&AssertRequest{}))
	assert.Equal(t, 1, scalarCount(&AssertRequest{ValueText: strPtr("x")}))
	assert.Equal(t, 1, scalarCount(&AssertRequest{ValueBool: boolPtr(false)}))
	assert.Equal(t, 2, scalarCount(&AssertRequest{ValueText: strPtr("x"), ValueDate: datePtr(time.Now())}))
}

// TestIsValidSourceKind covers the closed enum membership.
func TestIsValidSourceKind(t *testing.T) {
	for _, k := range []string{
		repository.SourceKindCommsMessage, repository.SourceKindTelegramMessage,
		repository.SourceKindMessagesMessage, repository.SourceKindMeetingNote,
		repository.SourceKindAnarlogTranscript, repository.SourceKindCalendarEvent,
		repository.SourceKindPhoneCall, repository.SourceKindUser, repository.SourceKindAgentSession,
	} {
		assert.True(t, isValidSourceKind(k), k)
	}
	assert.False(t, isValidSourceKind("not_a_kind"))
	assert.False(t, isValidSourceKind(""))
}

// TestIsValidProducerKind covers the producer-kind enum.
func TestIsValidProducerKind(t *testing.T) {
	assert.True(t, isValidProducerKind(repository.ProducerKindExtractor))
	assert.True(t, isValidProducerKind(repository.ProducerKindAgent))
	assert.True(t, isValidProducerKind(repository.ProducerKindUser))
	assert.False(t, isValidProducerKind("robot"))
}

// TestStrongerTrust folds the strongest producer across an existing tier + new
// locators (user > agent > extractor).
func TestStrongerTrust(t *testing.T) {
	extractor := []ProvenanceLocator{{ProducerKind: repository.ProducerKindExtractor}}
	user := []ProvenanceLocator{{ProducerKind: repository.ProducerKindUser}}

	// Fresh (nil existing) extractor → "extractor".
	got := strongerTrust(nil, extractor)
	require.NotNil(t, got)
	assert.Equal(t, "extractor", *got)

	// Existing extractor + a user locator → upgrades to "user".
	existing := "extractor"
	got = strongerTrust(&existing, user)
	require.NotNil(t, got)
	assert.Equal(t, "user", *got)

	// Existing user + an extractor locator → stays "user" (never downgrades).
	existingUser := "user"
	got = strongerTrust(&existingUser, extractor)
	require.NotNil(t, got)
	assert.Equal(t, "user", *got)

	// No existing, no locators → nil.
	assert.Nil(t, strongerTrust(nil, nil))
}

// TestMinStartMaxEnd covers the window-union helpers (nil = open = -inf/+inf).
func TestMinStartMaxEnd(t *testing.T) {
	early := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	late := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

	// minStart: nil on either side → open (nil).
	assert.Nil(t, minStart(nil, &early))
	assert.Nil(t, minStart(&early, nil))
	got := minStart(&early, &late)
	require.NotNil(t, got)
	assert.True(t, got.Equal(early), "minStart returns the earlier")

	// maxEnd: nil on either side → open (nil).
	assert.Nil(t, maxEnd(nil, &late))
	assert.Nil(t, maxEnd(&early, nil))
	got = maxEnd(&early, &late)
	require.NotNil(t, got)
	assert.True(t, got.Equal(late), "maxEnd returns the later")
}

// TestCanonicalEdge_FactUnchanged asserts a fact (nil object) passes through
// unchanged (no canonicalization).
func TestCanonicalEdge_FactUnchanged(t *testing.T) {
	pred := factPredicate("how_met", repository.PredicateValueTypeText, repository.PredicateBucketNone)
	subject := uuid.New()
	key, subj, obj := canonicalEdge(pred, subject, nil)
	assert.Equal(t, pred.Key, key)
	assert.Equal(t, subject, subj)
	assert.Nil(t, obj)
}

// TestSlotLockKey_Deterministic asserts the advisory-lock key is stable for a
// given slot and differs across slots.
func TestSlotLockKey_Deterministic(t *testing.T) {
	a := uuid.New()
	b := uuid.New()
	assert.Equal(t, slotLockKey("lives_in", a), slotLockKey("lives_in", a))
	assert.NotEqual(t, slotLockKey("lives_in", a), slotLockKey("lives_in", b))
	assert.NotEqual(t, slotLockKey("lives_in", a), slotLockKey("works_at", a))
}

// TestTransitionToken maps each create/terminal kind to its one-shot source_id
// token (retract reuses the superseded token — no :retracted).
func TestTransitionToken(t *testing.T) {
	assert.Equal(t, "proposed", transitionToken(events.KindAssertionProposed))
	assert.Equal(t, "accepted", transitionToken(events.KindAssertionAccepted))
	assert.Equal(t, "superseded", transitionToken(events.KindAssertionSuperseded))
	assert.Equal(t, "rejected", transitionToken(events.KindAssertionRejected))
}
