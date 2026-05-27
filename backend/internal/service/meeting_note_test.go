package service

import (
	"testing"

	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// TestSnapshotContains covers the membership check used by
// ResolveLink's snapshot validation path.
func TestSnapshotContains(t *testing.T) {
	eventID := uuid.New()
	callID := uuid.New()
	snap := []repository.ConflictCandidateSummary{
		{Kind: repository.LinkedKindEvent, ID: eventID, OverlapCount: 2},
		{Kind: repository.LinkedKindPhoneCall, ID: callID, OverlapCount: 1},
	}

	require.True(t, snapshotContains(snap, repository.LinkedKindEvent, eventID))
	require.True(t, snapshotContains(snap, repository.LinkedKindPhoneCall, callID))
	require.False(t, snapshotContains(snap, repository.LinkedKindEvent, callID),
		"id alone is not enough — kind must match")
	require.False(t, snapshotContains(snap, repository.LinkedKindEvent, uuid.New()),
		"random id rejected")
	require.False(t, snapshotContains(nil, repository.LinkedKindEvent, eventID),
		"nil snapshot returns false")
}

// TestTruncateRunes covers rune-aware truncation including UTF-8 edges.
func TestTruncateRunes(t *testing.T) {
	require.Equal(t, "", truncateRunes("anything", 0))
	require.Equal(t, "abc", truncateRunes("abc", 10), "shorter than max returns original")
	require.Equal(t, "abc", truncateRunes("abcdef", 3))
	// Multi-byte runes: "héllo" is 5 runes, 6 bytes. Cutting at 3 runes
	// returns "hél" — must not split mid-byte.
	got := truncateRunes("héllo", 3)
	require.Equal(t, "hél", got)
}

// TestResolveLinkInputFromKind is a sanity test for the convenience
// constructor used by the handler.
func TestResolveLinkInputFromKind(t *testing.T) {
	id := uuid.New()
	got := ResolveLinkInputFromKind(repository.LinkedKindEvent, id)
	require.Equal(t, repository.LinkedKindEvent, got.Kind)
	require.Equal(t, id, got.ID)
}
