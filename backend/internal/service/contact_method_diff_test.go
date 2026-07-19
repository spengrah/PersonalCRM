package service

import (
	"testing"

	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// preStateRow builds a pre-state row the way the database would: value_normalized
// is the trigger's output, which the C6 mirror reproduces.
func preStateRow(methodType, value string) repository.ContactMethod {
	return repository.ContactMethod{
		ID:              uuid.New(),
		Type:            methodType,
		Value:           value,
		ValueNormalized: repository.NormalizeContactMethodValueForUniqueness(methodType, value),
	}
}

func foldRow(methodType, value string) *foldedMethod {
	return &foldedMethod{ID: uuid.New(), Type: methodType, Value: value}
}

// diffNewMethodsFromFold is what decides whether a rematch job is minted, and it
// had no direct coverage. Its doc comment claims the semantic diff makes
// idempotency fall out for free — a replayed payload folds to a final state
// equal to pre-state, so the diff is empty and nothing is published. That claim
// is the property pinned here.
func TestDiffNewMethodsFromFold(t *testing.T) {
	t.Parallel()

	existingEmail := preStateRow("email", "existing@example.test")

	cases := []struct {
		name   string
		before []repository.ContactMethod
		after  []*foldedMethod
		want   []Method
	}{
		{
			name:   "a value newly present is reported",
			before: nil,
			after:  []*foldedMethod{foldRow("email", "added@example.test")},
			want:   []Method{{Type: "email", Value: "added@example.test"}},
		},
		{
			// The replay property. A payload applied twice folds to a final
			// state equal to pre-state; if this diff were computed over
			// physical inserts instead of values newly PRESENT, a replay would
			// re-publish and mint a second rematch job.
			name:   "replaying a payload produces an empty diff",
			before: []repository.ContactMethod{existingEmail},
			after:  []*foldedMethod{foldRow("email", existingEmail.Value)},
			want:   nil,
		},
		{
			// Values are compared through the mirror, not raw. A pre-existing
			// row reached by a differently-spelled but equivalently-normalizing
			// value is NOT newly present.
			name:   "a differently spelled handle resolving to the same key is not new",
			before: []repository.ContactMethod{preStateRow("discord", "handle")},
			after:  []*foldedMethod{foldRow("discord", "@@handle")},
			want:   nil,
		},
		{
			name:   "a removal reports nothing",
			before: []repository.ContactMethod{existingEmail},
			after:  nil,
			want:   nil,
		},
		{
			// Same type, genuinely different value: newly present alongside the
			// one that was already there.
			name:   "only the newly present value of a pair is reported",
			before: []repository.ContactMethod{existingEmail},
			after: []*foldedMethod{
				foldRow("email", existingEmail.Value),
				foldRow("email", "second@example.test"),
			},
			want: []Method{{Type: "email", Value: "second@example.test"}},
		},
		{
			// A stored-value-only edit (the in-place update path) DOES change
			// the key here, because case is part of neither normalizer for
			// handles... but for email the mirror lowercases, so a case-only
			// email edit is not newly present.
			name:   "a case-only email edit is not newly present",
			before: []repository.ContactMethod{preStateRow("email", "Person@Example.test")},
			after:  []*foldedMethod{foldRow("email", "person@example.test")},
			want:   nil,
		},
		{
			name:   "a type change makes the value newly present under the new type",
			before: []repository.ContactMethod{preStateRow("telegram", "handle")},
			after:  []*foldedMethod{foldRow("discord", "handle")},
			want:   []Method{{Type: "discord", Value: "handle"}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := diffNewMethodsFromFold(tc.before, tc.after)
			if len(tc.want) == 0 {
				assert.Empty(t, got)
				return
			}
			require.Len(t, got, len(tc.want))
			assert.Equal(t, tc.want, got)
		})
	}
}
