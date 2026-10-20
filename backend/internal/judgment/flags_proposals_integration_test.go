//go:build integration_testdb

package judgment_test

import (
	"testing"

	"personal-crm/backend/internal/judgment"
	"personal-crm/backend/internal/threadctx"

	"github.com/stretchr/testify/assert"
)

// A judgment of a new version compiles into a proposal from its ball-in-court
// outcomes alone.
func TestBenchReadCompilesNewVersionProposals(t *testing.T) {
	t.Run("a conversation judgment with flags and ball-in-court outcomes", func(t *testing.T) {
		// spec: IXN-053.lists-shown
		b := newBenchEnv(t)
		m := judgment.MethodRef{Key: b.methods.Keys()[0], Version: "v1", WordingVersion: "w1"}
		x, th := b.oneToOne(t)
		key := b.messageKey(t, th)
		j := windowed(m, th, b.day(1), "h1", key, []string{key}, judgment.Outcomes{
			BallInCourt: []judgment.ParticipantOutcome[judgment.BallInCourt]{bic(x, judgment.BallInCourtIOweReply, 0.9)},
		})
		j.Flags = flagsOf(key, 0.9, 0.2, 0.1)
		b.run(t, judgment.RunKindScheduled, j)

		p := b.proposal(t, x, m)

		assert.Equal(t, []string{x.String()}, listContacts(p, "i_owe_reply"))
		assert.Len(t, p.Items, 1, "the flags add no item")
		assert.Equal(t, []string{"i_owe_reply", "owes_me_reply", "commitments", "big_moment"}, listNames(p))
	})

	t.Run("a meeting judgment, which holds no outcome", func(t *testing.T) {
		// spec: IXN-053.lists-shown
		b := newBenchEnv(t)
		m := judgment.MethodRef{Key: b.methods.Keys()[0], Version: "v1", WordingVersion: "w1"}
		x, th := b.oneToOne(t)
		key := b.messageKey(t, th)
		j := windowed(m, th, b.day(1), "h1", key, []string{key}, judgment.Outcomes{})
		j.Kind = threadctx.KindMeeting
		j.Flags = flagsOf(key, 0.9, 0.2, 0.1)
		b.run(t, judgment.RunKindScheduled, j)

		p := b.proposal(t, x, m)

		assert.Empty(t, p.Items)
		assert.Equal(t, []string{"commitments", "big_moment"}, listNames(p))
	})
}
