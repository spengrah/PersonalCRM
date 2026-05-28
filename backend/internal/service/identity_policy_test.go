package service

import (
	"context"
	"testing"

	"personal-crm/backend/internal/identity"

	"github.com/stretchr/testify/require"
)

// TestMatchOrCreateTx_EmptyAfterNormalization_PolicyBranch is the
// always-running (no-DB) proof of the empty-after-normalization policy
// switch. The empty branch returns BEFORE MatchOrCreateTx touches the
// repository or the tx (Normalize is pure; the empty-check precedes any
// GetByIdentifierTx/findContactByMethodTx/UpsertTx), so a real
// *IdentityService built with a nil repo and called with a nil tx never
// dereferences either on this path.
//
//   - NormalizationFailEmpty -> error, nil result (the fatal callers).
//   - NormalizationSkipEmpty -> (nil, nil) (the loop callers' no-op).
func TestMatchOrCreateTx_EmptyAfterNormalization_PolicyBranch(t *testing.T) {
	svc := NewIdentityService(nil) // nil repo: empty branch returns before any repo use
	cases := []struct {
		name    string
		policy  NormalizationPolicy
		wantErr bool
	}{
		{"FailEmpty returns error", NormalizationFailEmpty, true},
		{"SkipEmpty returns nil,nil", NormalizationSkipEmpty, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// "+" normalizes to "" for phone; nil tx is never
			// dereferenced on the empty branch.
			res, err := svc.MatchOrCreateTx(context.Background(), nil, MatchRequest{
				RawIdentifier: "+",
				Type:          identity.IdentifierTypePhone,
				Source:        "test",
			}, tc.policy)
			if tc.wantErr {
				require.Error(t, err)
				require.Contains(t, err.Error(), "empty identifier after normalization")
			} else {
				require.NoError(t, err)
			}
			// Both policies return a nil result on the empty branch.
			require.Nil(t, res)
		})
	}
}
