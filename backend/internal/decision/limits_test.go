package decision_test

import (
	"testing"

	"personal-crm/backend/internal/decision"
	"personal-crm/backend/internal/judgment"
	"personal-crm/backend/internal/threadctx"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A method measures a request against its own limits without sending it, and
// judging returns over_limit, sending nothing, exactly when the measurement
// exceeds a limit; under the limits the whole context is sent (PR3.c4,
// PR3.c10). The tree has 7 questions per matched contact plus one, so 64 is
// reachable and 65 is not: one over the question limit is a 64-question tree
// against a limit of 63.
func TestJudge_LimitsDecideBeforeSending(t *testing.T) {
	long := dm("I'll send the photos on Friday", "ME: thanks! can you bring the salad?", "sure, and I'll call you tomorrow")
	nine := nineContacts()
	size := func(c threadctx.Context) decision.Size {
		s, err := newMethod(definition(roomy), &fake{answer: plain}).Measure(t.Context(), c)
		require.NoError(t, err)
		return s
	}
	tokens := size(long).Tokens
	require.Equal(t, 64, size(nine).Questions)

	cases := []struct {
		name    string
		context threadctx.Context
		limits  decision.Limits
		over    bool
	}{
		{"a context just under the token limit", long, decision.Limits{Tokens: tokens + 1, Questions: 64}, false},
		{"a context at the token limit", long, decision.Limits{Tokens: tokens, Questions: 64}, false},
		{"a context just over the token limit", long, decision.Limits{Tokens: tokens - 1, Questions: 64}, true},
		{"a tree of 64 questions under a limit of 64", nine, decision.Limits{Tokens: 1 << 20, Questions: 64}, false},
		{"a tree one question over its limit", nine, decision.Limits{Tokens: 1 << 20, Questions: 63}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fake{answer: plain}
			m := newMethod(definition(tc.limits), f)

			s, err := m.Measure(t.Context(), tc.context)
			require.NoError(t, err)
			assert.Empty(t, f.requests, "measuring sends nothing")
			assert.Equal(t, tc.over, s.Exceeds())
			assert.Equal(t, tc.limits.Tokens, s.TokenLimit)
			assert.Equal(t, tc.limits.Questions, s.QuestionLimit)

			res, err := m.Judge(t.Context(), tc.context)
			require.NoError(t, err)
			if tc.over {
				assert.Equal(t, decision.Incomplete{Reason: judgment.IncompleteReasonOverLimit}, res)
				assert.Empty(t, f.requests, "an over-limit request is never sent")
				return
			}
			judged(t, res)
			require.Len(t, f.requests, 1)
			sent := f.requests[0]
			assert.Len(t, sent.Entries(), len(tc.context.Entries), "the whole context is sent")
			for i, e := range sent.Entries() {
				assert.Len(t, e.Text, len(tc.context.Entries[i].Text), "entry %d is sent whole", i)
			}
		})
	}
}

// A call that fails returns call_failed, not an error that ends the run
// (PR3.c4).
func TestJudge_EndpointErrorIsCallFailed(t *testing.T) {
	f := &fake{err: errEndpoint}
	res, err := newMethod(definition(roomy), f).Judge(t.Context(), dm("are you around?"))
	require.NoError(t, err)
	assert.Equal(t, decision.Incomplete{Reason: judgment.IncompleteReasonCallFailed}, res)
	assert.Len(t, f.requests, 1)
}

// nineContacts is a group thread with nine matched contacts.
func nineContacts() threadctx.Context {
	c := group("PERSON_2: lunch on saturday?")
	delete(c.Participants, threadctx.PersonRole(4))
	for n := 2; n <= 10; n++ {
		id := uuid.New()
		c.Participants[threadctx.PersonRole(n)] = threadctx.Participant{ContactID: &id}
	}
	return c
}
