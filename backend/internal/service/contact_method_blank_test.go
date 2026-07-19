package service

import (
	"testing"

	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A value that is non-empty but normalizes to empty is as unsatisfiable as a
// blank one, and until this check existed it was accepted: the row persisted
// with an empty value_normalized, which the unique index caps at one per
// (contact, type) and which identity and sync queries filter out with
// a non-empty-value_normalized predicate. The result is a row that exists, cannot be matched
// on, and occupies the slot — inert rather than corrupting, which is why it was
// non-blocking, but reachable the moment the form starts sending operations.
//
// Reachability is type-dependent and the table says so: email and gchat are
// already protected by the handler's email validator, phone/signal/whatsapp are
// length-checked only, and telegram/discord/twitter have no format rule at all.
// The handle and phone rows are the ones that reach this check in production.
func TestRequireTypeAndValue_RejectsValuesEmptyAfterNormalization(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		methodType string
		value      string
	}{
		{"handle of only at-signs", "discord", "@@@"},
		{"single at-sign handle", "telegram", "@"},
		{"twitter handle of spaces", "twitter", "   "},
		{"phone with no digits", "phone", "()- "},
		{"signal with no digits", "signal", "+"},
		{"whatsapp of spaces", "whatsapp", "  "},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// Precondition: the value is genuinely non-blank, so the literal
			// empty-string check cannot be what rejects it. Without this the
			// test would pass against a build that has no normalization check
			// at all.
			require.NotEmpty(t, tc.value)
			require.Empty(t, repository.NormalizeContactMethodValueForUniqueness(tc.methodType, tc.value),
				"fixture does not normalize to empty, so it does not exercise this rule")

			for _, op := range []ContactMethodOperation{
				{Op: MethodOpAdd, Type: tc.methodType, Value: tc.value},
				{Op: MethodOpUpdate, MethodID: uuidPtr(uuid.New()), Type: tc.methodType, Value: tc.value},
			} {
				err := validateOperationShapes([]ContactMethodOperation{op})
				require.Error(t, err, "%s with a value empty after normalization was accepted", op.Op)
				assert.ErrorIs(t, err, ErrInvalidOperations)
				assert.Contains(t, err.Error(), "empty once normalized")
			}
		})
	}
}

// The companion direction: a value that survives normalization is not rejected
// by the new check. Without this, deleting the whole rule and rejecting every
// add would leave the table above green.
func TestRequireTypeAndValue_AcceptsValuesThatSurviveNormalization(t *testing.T) {
	t.Parallel()

	for _, op := range []ContactMethodOperation{
		{Op: MethodOpAdd, Type: "discord", Value: "@@handle"},
		{Op: MethodOpAdd, Type: "phone", Value: "(555) 555-0100"},
		{Op: MethodOpAdd, Type: "email", Value: "person@example.test"},
	} {
		assert.NoError(t, validateOperationShapes([]ContactMethodOperation{op}), "op %+v", op)
	}
}

func uuidPtr(id uuid.UUID) *uuid.UUID { return &id }
