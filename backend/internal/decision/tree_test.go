package decision_test

import (
	"maps"
	"slices"
	"strings"
	"testing"

	"personal-crm/backend/internal/judgment"
	"personal-crm/backend/internal/threadctx"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// One thread context is one request holding the whole question tree: per
// matched contact, ball-in-court (conversations only) and, per direction, a
// commitment question, its date and its source message keyed by message key;
// one big-moment question; nothing asked of or between non-contacts.
func TestJudge_OneRequestHoldsTheWholeTree(t *testing.T) {
	perParty := func(role string, conversation bool) []string {
		var ids []string
		if conversation {
			ids = append(ids, "ball_in_court/"+role)
		}
		for _, q := range []string{"commitment_mine/" + role, "commitment_theirs/" + role} {
			ids = append(ids, q, q+"/date", judgment.SourceMessageQuestionID(q))
		}
		return ids
	}
	cases := []struct {
		name    string
		context threadctx.Context
		want    []string
		// sources maps a source-message question to the message keys its
		// options name.
		sources map[string][]string
	}{
		{
			name:    "a one-to-one chat",
			context: dm("I'll send you the photos by Friday", "ME: great, thanks!", "also, are you free saturday?"),
			want:    append(perParty("THEM", true), "big_moment"),
			sources: map[string][]string{
				"commitment_mine/THEM/source_message":   {"synthetic:b"},
				"commitment_theirs/THEM/source_message": {"synthetic:a", "synthetic:c"},
			},
		},
		{
			name:    "a group chat with one non-contact participant",
			context: group("PERSON_2: I'll bring the cake", "PERSON_4: count me in", "ME: I'll book the table"),
			want:    append(append(perParty("PERSON_2", true), perParty("PERSON_3", true)...), "big_moment"),
			sources: map[string][]string{
				"commitment_theirs/PERSON_2/source_message": {"synthetic:a"},
				"commitment_theirs/PERSON_3/source_message": nil,
				"commitment_mine/PERSON_3/source_message":   {"synthetic:c"},
			},
		},
		{
			name:    "a meeting context",
			context: meeting("PERSON_2 will send the budget", "I owe PERSON_3 the slides"),
			want:    append(append(perParty("PERSON_2", false), perParty("PERSON_3", false)...), "big_moment"),
			sources: map[string][]string{
				"commitment_theirs/PERSON_2/source_message": {"synthetic:a", "synthetic:b"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fake{answer: plain}
			res, err := newMethod(definition(roomy), f).Judge(t.Context(), tc.context)
			require.NoError(t, err)
			j := judged(t, res)

			require.Len(t, f.requests, 1, "one request per thread context")
			req := f.requests[0]
			assert.ElementsMatch(t, tc.want, questionIDs(req))
			for _, q := range req.Questions() {
				switch {
				case strings.HasPrefix(q.ID, "ball_in_court/"):
					assert.Equal(t, []string{"i_owe_reply", "they_owe_reply", "nobody_owes", "not_applicable"}, optionKeys(q))
				case strings.Count(q.ID, "/") == 1 && strings.HasPrefix(q.ID, "commitment_"):
					assert.Equal(t, threadctx.QuestionNoul, q.Kind, q.ID)
				}
				if q.ID != "big_moment" {
					assert.Contains(t, q.Instructions, "ME", "%s concerns the user", q.ID)
					role := strings.Split(q.ID, "/")[1]
					for other := range tc.context.Participants {
						if other != threadctx.RoleMe && string(other) != role {
							assert.NotContains(t, q.Instructions, string(other), "%s concerns only the user and %s", q.ID, role)
						}
					}
				}
				for _, o := range q.Options {
					assert.NotContains(t, o.Key, "synthetic:", "no message key leaves the Pi")
				}
			}
			for id, keys := range tc.sources {
				got := slices.Collect(maps.Keys(answered(j, id).Probabilities))
				assert.ElementsMatch(t, append(keys, "none", "unlisted"), got, "%s options are keyed by message key", id)
			}
		})
	}
}

func optionKeys(q threadctx.Question) []string {
	keys := make([]string, len(q.Options))
	for i, o := range q.Options {
		keys[i] = o.Key
	}
	return keys
}
