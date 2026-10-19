//go:build integration_testdb

package main

import (
	"regexp"
	"slices"
	"strings"
	"testing"

	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/decision"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// versionBeforeFlags is each compared method's version and wording version
// before the message-flag change.
var versionBeforeFlags = map[string][2]string{
	"clef-masked":          {"17befc38173f", "0cb9ed663195"},
	"clef-masked-reasoned": {"37506004e1fa", "48ba87f9d060"},
	"jev-masked":           {"11d6a6650a01", "0cb9ed663195"},
}

var allowedQuestion = regexp.MustCompile(`^(flag/(commitment|big_moment|claim)/m\d+|clearing/m\d+|ball_in_court/.+)$`)

// Every compared method's new version asks only flag, clearing and
// ball-in-court questions, with no whole-thread commitment question, no date or
// source-message question and no whole-thread big-moment question, and each
// compared method's version differs from its version before the change.
// spec: KNW-061.asks-only-flag-clearing-ball-in-court, KNW-061.no-whole-thread-questions
func TestJudgeMonthAsksOnlyFlagClearingAndBallInCourtQuestions(t *testing.T) {
	cases := []struct {
		name         string
		conversation bool
	}{{"a conversation", true}, {"a meeting", false}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newFlagsEnv(t, "")
			openRouter, venice := newVendor(t), newVendor(t)
			routeVendors(t, map[string]*vendor{"openrouter.ai": openRouter, "api.venice.ai": venice})
			registry := decision.NewRegistry(config.DecisionConfig{OpenRouterAPIKey: "key", VeniceAPIKey: "key"}, e.masker)
			if tc.conversation {
				th := e.newThread(t)
				th.say(t, jan(3, 9), false, promise)
				th.say(t, jan(4, 9), true, kept)
			} else {
				m := e.newMeeting(t)
				m.note(t, jan(3, 9), promise)
				m.note(t, jan(4, 9), kept)
			}
			vendors := map[string]*vendor{"clef-masked": openRouter, "clef-masked-reasoned": openRouter, "jev-masked": venice}

			for _, key := range registry.Keys() {
				vendors[key].reset()
				e.judgeMonth(t, registry, key)

				method, err := registry.Get(key)
				require.NoError(t, err)
				before := versionBeforeFlags[key]
				assert.NotEqual(t, before[0], method.Ref().Version, "%s: its version differs from before", key)
				assert.NotEqual(t, before[1], method.Ref().WordingVersion, "%s: its wording differs from before", key)
				calls := vendors[key].all()
				require.NotEmpty(t, calls, key)
				for _, c := range calls {
					for _, a := range c.Asks {
						assert.Regexp(t, allowedQuestion, a.ID, key)
						assert.False(t, strings.Contains(a.ID, "date") || strings.Contains(a.ID, "source"), "%s: %s", key, a.ID)
					}
					assert.Equal(t, tc.conversation, len(c.asks("ball_in_court")) > 0, "%s: ball-in-court only for a conversation", key)
					assert.True(t, slices.ContainsFunc(c.Asks, func(a ask) bool { return a.Kind == "flag" }), key)
				}
			}
		})
	}
}
