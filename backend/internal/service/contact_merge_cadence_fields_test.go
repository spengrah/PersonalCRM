package service

import (
	"testing"
	"time"

	"personal-crm/backend/internal/repository"

	"github.com/stretchr/testify/require"
)

func TestBuildMergeCadenceFields_AwaitingReplyUntil(t *testing.T) {
	earlier := time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)
	later := earlier.AddDate(0, 0, 7)
	mergedCadence := ""

	for _, tc := range []struct {
		name   string
		target *time.Time
		source *time.Time
		want   *time.Time
	}{
		{name: "takes later expiry", target: &earlier, source: &later, want: &later},
		{name: "keeps target only", target: &earlier, want: &earlier},
		{name: "keeps source only", source: &later, want: &later},
		{name: "nil when both absent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fields := buildMergeCadenceFields(
				&repository.Contact{AwaitingReplyUntil: tc.target},
				&repository.Contact{AwaitingReplyUntil: tc.source},
				&mergedCadence,
			)
			if tc.want == nil {
				require.Nil(t, fields.AwaitingReplyUntil)
				return
			}
			require.NotNil(t, fields.AwaitingReplyUntil)
			require.Equal(t, *tc.want, *fields.AwaitingReplyUntil)
		})
	}
}
