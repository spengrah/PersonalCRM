package telegram

import (
	"context"
	"testing"

	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Mocks ---

type mockIdentityMatcher struct {
	calls   []service.MatchRequest
	results map[string]*service.MatchResult // keyed by RawIdentifier
}

func (m *mockIdentityMatcher) MatchOrCreate(_ context.Context, req service.MatchRequest) (*service.MatchResult, error) {
	m.calls = append(m.calls, req)
	if result, ok := m.results[req.RawIdentifier]; ok {
		return result, nil
	}
	return &service.MatchResult{MatchType: repository.MatchTypeUnmatched}, nil
}

type updateMatchCall struct {
	id        uuid.UUID
	contactID *uuid.UUID
	status    repository.MatchStatus
}

type mockExternalContactUpserter struct {
	upsertCalls      []repository.UpsertTelegramDiscoveryCandidateRequest
	getResult        *repository.ExternalContact
	updateMatchCalls []updateMatchCall
}

func (m *mockExternalContactUpserter) UpsertTelegramDiscoveryCandidate(_ context.Context, req repository.UpsertTelegramDiscoveryCandidateRequest) (*repository.ExternalContact, error) {
	m.upsertCalls = append(m.upsertCalls, req)
	return &repository.ExternalContact{ID: uuid.New(), Source: "telegram", SourceID: req.SourceID}, nil
}

func (m *mockExternalContactUpserter) GetBySource(_ context.Context, _, _ string, _ *string) (*repository.ExternalContact, error) {
	return m.getResult, nil
}

func (m *mockExternalContactUpserter) UpdateMatch(_ context.Context, id uuid.UUID, contactID *uuid.UUID, status repository.MatchStatus) (*repository.ExternalContact, error) {
	m.updateMatchCalls = append(m.updateMatchCalls, updateMatchCall{id: id, contactID: contactID, status: status})
	return nil, nil
}

// --- Tests ---

func TestBuildDisplayName(t *testing.T) {
	tests := []struct {
		name      string
		firstName *string
		lastName  *string
		want      *string
	}{
		{"both names", ptr("John"), ptr("Doe"), ptr("John Doe")},
		{"first only", ptr("John"), nil, ptr("John")},
		{"last only", nil, ptr("Doe"), ptr("Doe")},
		{"neither", nil, nil, nil},
		{"empty first", ptr(""), nil, nil},
		{"empty both", ptr(""), ptr(""), nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildDisplayName(tt.firstName, tt.lastName)
			if tt.want == nil {
				assert.Nil(t, got)
			} else {
				assert.Equal(t, *tt.want, *got)
			}
		})
	}
}

func TestMatchPeer_UsernameMatch(t *testing.T) {
	contactID := uuid.New()
	identityMock := &mockIdentityMatcher{
		results: map[string]*service.MatchResult{
			"alice": {ContactID: &contactID, MatchType: repository.MatchTypeExact},
		},
	}
	// Use a nil messageRepo — MatchPeer will call UpdateMessageContact which needs it,
	// but we're testing the matching logic flow, not the DB update.
	// For a full test, use integration tests.
	matcher := &PeerMatcher{
		identityService:     identityMock,
		externalContactRepo: &mockExternalContactUpserter{},
		discoveryMinMsgs:    3,
	}

	username := "alice"
	result, err := matcher.MatchPeer(context.Background(), 12345, &username, ptr("Alice"), nil, nil)

	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, contactID, *result)
	// Should have called MatchOrCreate with telegram type
	require.Len(t, identityMock.calls, 1)
	assert.Equal(t, "alice", identityMock.calls[0].RawIdentifier)
	assert.Equal(t, "telegram", string(identityMock.calls[0].Type))
}

func TestMatchPeer_PhoneFallback(t *testing.T) {
	contactID := uuid.New()
	identityMock := &mockIdentityMatcher{
		results: map[string]*service.MatchResult{
			// Username doesn't match
			"bob": {MatchType: repository.MatchTypeUnmatched},
			// Phone matches
			"+15551234567": {ContactID: &contactID, MatchType: repository.MatchTypeExact},
		},
	}
	matcher := &PeerMatcher{
		identityService:     identityMock,
		externalContactRepo: &mockExternalContactUpserter{},
		discoveryMinMsgs:    3,
	}

	username := "bob"
	phone := "+15551234567"
	result, err := matcher.MatchPeer(context.Background(), 12345, &username, ptr("Bob"), nil, &phone)

	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, contactID, *result)
	// Should have tried username first, then phone, then linked telegram identity
	require.Len(t, identityMock.calls, 3)
	assert.Equal(t, "telegram", string(identityMock.calls[0].Type)) // username attempt
	assert.Equal(t, "phone", string(identityMock.calls[1].Type))    // phone match
	assert.Equal(t, "telegram", string(identityMock.calls[2].Type)) // link telegram identity
	assert.NotNil(t, identityMock.calls[2].KnownContactID)          // with KnownContactID
}

func TestMatchPeer_UsernamePriority(t *testing.T) {
	contactA := uuid.New()
	contactB := uuid.New()
	identityMock := &mockIdentityMatcher{
		results: map[string]*service.MatchResult{
			"charlie":      {ContactID: &contactA, MatchType: repository.MatchTypeExact},
			"+15559876543": {ContactID: &contactB, MatchType: repository.MatchTypeExact},
		},
	}
	matcher := &PeerMatcher{
		identityService:     identityMock,
		externalContactRepo: &mockExternalContactUpserter{},
		discoveryMinMsgs:    3,
	}

	username := "charlie"
	phone := "+15559876543"
	result, err := matcher.MatchPeer(context.Background(), 12345, &username, ptr("Charlie"), nil, &phone)

	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, contactA, *result) // username match wins
	// Should only have tried username (short-circuits on match)
	require.Len(t, identityMock.calls, 1)
}

func TestMatchPeer_Unmatched(t *testing.T) {
	identityMock := &mockIdentityMatcher{
		results: map[string]*service.MatchResult{},
	}
	matcher := &PeerMatcher{
		identityService:     identityMock,
		externalContactRepo: &mockExternalContactUpserter{},
		discoveryMinMsgs:    3,
	}

	username := "nobody"
	result, err := matcher.MatchPeer(context.Background(), 12345, &username, ptr("Nobody"), nil, nil)

	require.NoError(t, err)
	assert.Nil(t, result) // no match
	// Should have tried username
	require.Len(t, identityMock.calls, 1)
}

func TestMatchPeer_NilUsernameAndPhone(t *testing.T) {
	identityMock := &mockIdentityMatcher{
		results: map[string]*service.MatchResult{},
	}
	matcher := &PeerMatcher{
		identityService:     identityMock,
		externalContactRepo: &mockExternalContactUpserter{},
		discoveryMinMsgs:    3,
	}

	result, err := matcher.MatchPeer(context.Background(), 12345, nil, nil, nil, nil)

	require.NoError(t, err)
	assert.Nil(t, result)
	// No calls — nothing to match on
	assert.Empty(t, identityMock.calls)
}

func TestOnPeerLinked_WithUsername(t *testing.T) {
	contactID := uuid.New()
	identityMock := &mockIdentityMatcher{
		results: map[string]*service.MatchResult{},
	}
	matcher := &PeerMatcher{
		identityService:     identityMock,
		externalContactRepo: &mockExternalContactUpserter{},
		discoveryMinMsgs:    3,
	}

	err := matcher.OnPeerLinked(context.Background(), 12345, "alice", contactID)
	require.NoError(t, err)

	// Should call MatchOrCreate with KnownContactID to link identity
	require.Len(t, identityMock.calls, 1)
	assert.Equal(t, "alice", identityMock.calls[0].RawIdentifier)
	assert.NotNil(t, identityMock.calls[0].KnownContactID)
	assert.Equal(t, contactID, *identityMock.calls[0].KnownContactID)
}

func TestOnPeerLinked_WithoutUsername(t *testing.T) {
	contactID := uuid.New()
	identityMock := &mockIdentityMatcher{
		results: map[string]*service.MatchResult{},
	}
	matcher := &PeerMatcher{
		identityService:     identityMock,
		externalContactRepo: &mockExternalContactUpserter{},
		discoveryMinMsgs:    3,
	}

	err := matcher.OnPeerLinked(context.Background(), 12345, "", contactID)
	require.NoError(t, err)

	// No MatchOrCreate call when username is empty
	assert.Empty(t, identityMock.calls)
}

func TestMarkExternalContactMatched_AlreadyMatched(t *testing.T) {
	contactID := uuid.New()
	ecMock := &mockExternalContactUpserter{
		getResult: &repository.ExternalContact{
			ID:          uuid.New(),
			MatchStatus: repository.MatchStatusMatched,
		},
		updateMatchCalls: make([]updateMatchCall, 0),
	}
	matcher := &PeerMatcher{
		externalContactRepo: ecMock,
	}

	// Should not call UpdateMatch — already matched
	matcher.markExternalContactMatched(context.Background(), 12345, contactID)
	assert.Empty(t, ecMock.updateMatchCalls)
}

func TestMarkExternalContactMatched_Unmatched(t *testing.T) {
	contactID := uuid.New()
	ecID := uuid.New()
	ecMock := &mockExternalContactUpserter{
		getResult: &repository.ExternalContact{
			ID:          ecID,
			MatchStatus: repository.MatchStatusUnmatched,
		},
		updateMatchCalls: make([]updateMatchCall, 0),
	}
	matcher := &PeerMatcher{
		externalContactRepo: ecMock,
	}

	matcher.markExternalContactMatched(context.Background(), 12345, contactID)
	require.Len(t, ecMock.updateMatchCalls, 1)
	assert.Equal(t, ecID, ecMock.updateMatchCalls[0].id)
	assert.Equal(t, contactID, *ecMock.updateMatchCalls[0].contactID)
}

// --- Mock message counter ---

type mockMessageCounter struct {
	counts map[int64]*repository.PeerMessageCount
}

func (m *mockMessageCounter) CountMessagesByPeerID(_ context.Context, peerUserID int64) (*repository.PeerMessageCount, error) {
	if count, ok := m.counts[peerUserID]; ok {
		return count, nil
	}
	return &repository.PeerMessageCount{PeerUserID: peerUserID}, nil
}

func TestUpdateDiscoveryCandidatesForPeer_BelowThreshold(t *testing.T) {
	ecMock := &mockExternalContactUpserter{}
	matcher := &PeerMatcher{
		messageCounter:      &mockMessageCounter{counts: map[int64]*repository.PeerMessageCount{12345: {TotalCount: 2}}},
		externalContactRepo: ecMock,
		discoveryMinMsgs:    3,
	}

	matcher.UpdateDiscoveryCandidatesForPeer(context.Background(), 12345, ptr("alice"), ptr("Alice"), nil)

	// Below threshold — no upsert
	assert.Empty(t, ecMock.upsertCalls)
}

func TestUpdateDiscoveryCandidatesForPeer_AtThreshold(t *testing.T) {
	ecMock := &mockExternalContactUpserter{}
	matcher := &PeerMatcher{
		messageCounter: &mockMessageCounter{counts: map[int64]*repository.PeerMessageCount{
			12345: {TotalCount: 3, OutboundCount: 1, InboundCount: 2},
		}},
		externalContactRepo: ecMock,
		discoveryMinMsgs:    3,
	}

	matcher.UpdateDiscoveryCandidatesForPeer(context.Background(), 12345, ptr("alice"), ptr("Alice"), nil)

	// At threshold — should upsert
	require.Len(t, ecMock.upsertCalls, 1)
	assert.Equal(t, "12345", ecMock.upsertCalls[0].SourceID)
	assert.Equal(t, int64(3), ecMock.upsertCalls[0].Metadata["message_count"])
	assert.Equal(t, "@alice", ecMock.upsertCalls[0].Metadata["username"])
	require.NotNil(t, ecMock.upsertCalls[0].SyncedAt, "synced_at should be set on live-path upsert")
}

func ptr(s string) *string { return &s }
