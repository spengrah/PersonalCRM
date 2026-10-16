package decision_test

import (
	"strings"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/decision"
	"personal-crm/backend/internal/judgment"
	"personal-crm/backend/internal/threadctx"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A timing expression resolves against the sending message's timestamp, never
// the time the run judges it, and the candidate the model selects becomes the
// commitment's date.
func TestJudge_CommitmentDateFromTheMessage(t *testing.T) {
	// Judged in October; the messages were sent on Wednesday 2026-01-14.
	defer accelerated.SetNowForTest(func() time.Time { return time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC) })()

	cases := []struct {
		name, message, expression string
		want                      *time.Time
	}{
		{"Friday in a January message judged in October", "I'll send you the photos on Friday", "Friday", day(2026, 1, 16)},
		{"tomorrow", "I'll call you tomorrow", "tomorrow", day(2026, 1, 15)},
		{"by the 17th", "I'll return the book by the 17th", "the 17th", day(2026, 1, 17)},
		{"no expression", "I'll send you the recipe", "", nil},
		{"this Wednesday in a Wednesday message", "I'll send the file by this Wednesday", "this Wednesday", day(2026, 1, 14)},
		{"next Monday in a Wednesday message", "I'll send the file next Monday", "next Monday", day(2026, 1, 19)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var dateOptions []threadctx.Option
			f := &fake{answer: func(q threadctx.Question) decision.Answer {
				switch q.ID {
				case "commitment_theirs/THEM":
					return pick(q, 0.9, key("open"))
				case "commitment_theirs/THEM/date":
					dateOptions = q.Options
					return pick(q, 0.8, func(o threadctx.Option) bool {
						return tc.expression != "" && strings.Contains(o.Label, `"`+tc.expression+`"`)
					})
				default:
					return plain(q)
				}
			}}
			res, err := newMethod(definition(roomy), f).Judge(t.Context(), dm(tc.message))
			require.NoError(t, err)
			theirs := commitment(t, judged(t, res), judgment.CommitmentDirectionTheirs)
			require.Equal(t, judgment.CommitmentStateOpen, theirs.Outcome)
			assert.Equal(t, tc.want, theirs.DueDate)
			if tc.want == nil {
				assert.Equal(t, []string{"unknown", "unlisted"}, optionKeys(threadctx.Question{Options: dateOptions}), "no candidate date is offered")
			}
		})
	}
}

func day(y int, m time.Month, d int) *time.Time {
	t := time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	return &t
}
