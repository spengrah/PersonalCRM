package telegram

import (
	"context"
	"errors"
	"testing"

	"personal-crm/backend/internal/repository"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func ptrInt32(v int32) *int32 { return &v }
func ptrInt64(v int64) *int64 { return &v }
func ptrStr(v string) *string { return &v }

// mockPeerEntityFetcher records call counts and returns canned responses.
type mockPeerEntityFetcher struct {
	CallCount int
	entity    *repository.PeerEntity
	err       error
}

func (m *mockPeerEntityFetcher) GetPeerEntityByUserID(_ context.Context, _ int64) (*repository.PeerEntity, error) {
	m.CallCount++
	return m.entity, m.err
}

func TestEnrichSparseEntity_FillsWhenUnresolvedAndCachedDataAvailable(t *testing.T) {
	mock := &mockPeerEntityFetcher{
		entity: &repository.PeerEntity{
			PeerUserID:    222,
			PeerUsername:  ptrStr("connormcmk"),
			PeerFirstName: ptrStr("Connor"),
			PeerLastName:  ptrStr("McK"),
			PeerPhone:     ptrStr("15551234567"),
		},
	}
	h := &MessageHandler{peerEntityFetcher: mock}
	parsed := &ParsedMessage{
		PeerUserID:         ptrInt64(222),
		PeerEntityResolved: false,
	}

	h.enrichSparseEntity(context.Background(), parsed)

	assert.Equal(t, 1, mock.CallCount)
	require.NotNil(t, parsed.PeerUsername)
	assert.Equal(t, "connormcmk", *parsed.PeerUsername)
	require.NotNil(t, parsed.PeerFirstName)
	assert.Equal(t, "Connor", *parsed.PeerFirstName)
	require.NotNil(t, parsed.PeerLastName)
	assert.Equal(t, "McK", *parsed.PeerLastName)
	require.NotNil(t, parsed.PeerPhone)
	assert.Equal(t, "15551234567", *parsed.PeerPhone)
}

func TestEnrichSparseEntity_ShortCircuitsWhenResolved(t *testing.T) {
	mock := &mockPeerEntityFetcher{
		entity: &repository.PeerEntity{PeerUserID: 222, PeerUsername: ptrStr("anything")},
	}
	h := &MessageHandler{peerEntityFetcher: mock}
	parsed := &ParsedMessage{
		PeerUserID:         ptrInt64(222),
		PeerUsername:       ptrStr("dan13ram"),
		PeerFirstName:      ptrStr("Dan"),
		PeerEntityResolved: true,
	}

	h.enrichSparseEntity(context.Background(), parsed)

	assert.Equal(t, 0, mock.CallCount, "fetcher must not be called when entity was resolved")
	assert.Equal(t, "dan13ram", *parsed.PeerUsername)
	assert.Equal(t, "Dan", *parsed.PeerFirstName)
}

func TestEnrichSparseEntity_ResolvedWithEmptyFieldsRespectsRemoval(t *testing.T) {
	// User has removed their username; ParseMessage marked PeerEntityResolved=true
	// with PeerUsername=nil. enrichSparseEntity must NOT resurrect a stored handle.
	mock := &mockPeerEntityFetcher{
		entity: &repository.PeerEntity{PeerUserID: 222, PeerUsername: ptrStr("oldhandle")},
	}
	h := &MessageHandler{peerEntityFetcher: mock}
	parsed := &ParsedMessage{
		PeerUserID:         ptrInt64(222),
		PeerUsername:       nil,
		PeerFirstName:      ptrStr("Connor"),
		PeerEntityResolved: true,
	}

	h.enrichSparseEntity(context.Background(), parsed)

	assert.Equal(t, 0, mock.CallCount)
	assert.Nil(t, parsed.PeerUsername, "removed username must remain nil")
	assert.Equal(t, "Connor", *parsed.PeerFirstName)
}

func TestEnrichSparseEntity_NoCachedData(t *testing.T) {
	mock := &mockPeerEntityFetcher{entity: nil, err: nil}
	h := &MessageHandler{peerEntityFetcher: mock}
	parsed := &ParsedMessage{
		PeerUserID:         ptrInt64(222),
		PeerEntityResolved: false,
	}

	h.enrichSparseEntity(context.Background(), parsed)

	assert.Equal(t, 1, mock.CallCount)
	assert.Nil(t, parsed.PeerUsername)
	assert.Nil(t, parsed.PeerFirstName)
}

func TestEnrichSparseEntity_LookupErrorIsBestEffort(t *testing.T) {
	mock := &mockPeerEntityFetcher{err: errors.New("boom")}
	h := &MessageHandler{peerEntityFetcher: mock}
	parsed := &ParsedMessage{
		PeerUserID:         ptrInt64(222),
		PeerFirstName:      ptrStr("preserved"),
		PeerEntityResolved: false,
	}

	require.NotPanics(t, func() {
		h.enrichSparseEntity(context.Background(), parsed)
	})
	assert.Equal(t, 1, mock.CallCount)
	assert.Equal(t, "preserved", *parsed.PeerFirstName, "parsed must remain unchanged on error")
}

func TestEnrichSparseEntity_NilParsed(t *testing.T) {
	mock := &mockPeerEntityFetcher{}
	h := &MessageHandler{peerEntityFetcher: mock}
	require.NotPanics(t, func() {
		h.enrichSparseEntity(context.Background(), nil)
	})
	assert.Equal(t, 0, mock.CallCount)
}

func TestEnrichSparseEntity_NilPeerUserID(t *testing.T) {
	mock := &mockPeerEntityFetcher{}
	h := &MessageHandler{peerEntityFetcher: mock}
	parsed := &ParsedMessage{PeerUserID: nil, PeerEntityResolved: false}
	h.enrichSparseEntity(context.Background(), parsed)
	assert.Equal(t, 0, mock.CallCount)
}

func TestEffectiveTracked_TrackedOverridesLarge(t *testing.T) {
	assert.True(t, EffectiveTracked("tracked", ptrInt32(200), 10))
}

func TestEffectiveTracked_IgnoredOverridesSmall(t *testing.T) {
	assert.False(t, EffectiveTracked("ignored", ptrInt32(3), 10))
}

func TestEffectiveTracked_AutoSmallGroup(t *testing.T) {
	assert.True(t, EffectiveTracked("auto", ptrInt32(5), 10))
}

func TestEffectiveTracked_AutoLargeGroup(t *testing.T) {
	assert.False(t, EffectiveTracked("auto", ptrInt32(50), 10))
}

func TestEffectiveTracked_AutoExactThreshold(t *testing.T) {
	assert.True(t, EffectiveTracked("auto", ptrInt32(10), 10))
}

func TestEffectiveTracked_AutoNilMemberCount(t *testing.T) {
	assert.True(t, EffectiveTracked("auto", nil, 10))
}
