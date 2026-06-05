package google

import (
	"context"
	"errors"
	"testing"

	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	chat "google.golang.org/api/chat/v1"
)

// fakeStateLister satisfies GChatSyncStateLister with a canned state list.
type fakeStateLister struct {
	states []repository.SyncState
	err    error
}

func (f *fakeStateLister) ListEnabledSyncStates(context.Context) ([]repository.SyncState, error) {
	return f.states, f.err
}

// noopFetcherProvider builds a provider whose fetcher factory FAILS the test if
// it is ever called — proving the no-op gate does zero fetcher work.
func noopFetcherProvider(t *testing.T) *GChatSyncProvider {
	t.Helper()
	p := &GChatSyncProvider{}
	p.SetFetcherFactoryForTest(func(context.Context, string) (chatFetcher, error) {
		t.Fatal("fetcher must not be built when no gchat state is enabled")
		return nil, nil
	})
	p.SetMeSetForTest(map[string]struct{}{})
	return p
}

func TestGChatHandleRematch_NoOpWhenNoEnabledState(t *testing.T) {
	ctx := context.Background()
	p := noopFetcherProvider(t)

	// (a) zero states at all.
	h := NewGChatHandleRematchHandler(p, &fakeStateLister{states: nil}, nil, nil)
	n, err := h.Rematch(ctx, uuid.New(), "alice@example.test")
	require.NoError(t, err)
	assert.Equal(t, 0, n)

	// (b) an ENABLED state, but for a DIFFERENT source (email) — the gchat gate
	// still finds zero gchat states.
	acct := "me@example.test"
	h2 := NewGChatHandleRematchHandler(p, &fakeStateLister{states: []repository.SyncState{
		{Source: GmailSourceName, AccountID: &acct, Enabled: true},
	}}, nil, nil)
	n, err = h2.Rematch(ctx, uuid.New(), "alice@example.test")
	require.NoError(t, err)
	assert.Equal(t, 0, n)
}

func TestGChatEmailRematch_NoOpWhenNoEnabledState(t *testing.T) {
	ctx := context.Background()
	p := noopFetcherProvider(t)

	h := NewGChatEmailRematchHandler(p, &fakeStateLister{states: nil}, nil, nil)
	n, err := h.Rematch(ctx, uuid.New(), "alice@example.test")
	require.NoError(t, err)
	assert.Equal(t, 0, n)
}

func TestGChatRematch_IdentifierTypes(t *testing.T) {
	handle := NewGChatHandleRematchHandler(nil, nil, nil, nil)
	email := NewGChatEmailRematchHandler(nil, nil, nil, nil)
	assert.Equal(t, "gchat", handle.IdentifierType())
	assert.Equal(t, "email", email.IdentifierType())
}

func TestGChatRematch_EmptyAddressNoOps(t *testing.T) {
	ctx := context.Background()
	p := noopFetcherProvider(t)

	// An empty/whitespace address normalizes to "" and short-circuits BEFORE the
	// state gate, so a lister that would ERROR is never consulted (err stays nil).
	h := NewGChatHandleRematchHandler(p, &fakeStateLister{err: errors.New("lister must not be called for an empty address")}, nil, nil)
	n, err := h.Rematch(ctx, uuid.New(), "   ")
	require.NoError(t, err)
	assert.Equal(t, 0, n)
}

// TestRestrictKnownMembers covers the rematch fan-out narrowing helper.
func TestRestrictKnownMembers(t *testing.T) {
	a := uuid.New()
	b := uuid.New()
	known := map[string][]uuid.UUID{
		"alice@example.test": {a},
		"bob@example.test":   {b},
	}
	got := restrictKnownMembers(known, "alice@example.test")
	assert.Equal(t, map[string][]uuid.UUID{"alice@example.test": {a}}, got)

	// Address not present → empty (non-nil) map.
	got = restrictKnownMembers(known, "carol@example.test")
	assert.Empty(t, got)
	assert.NotNil(t, got)
}

// sanity: the fake fetcher funcs type is usable here (compile guard so the
// cross-package seam stays exercised by this package's tests too).
var _ = FakeChatFetcherFuncs{ListSpaces: func(context.Context, string) ([]*chat.Space, string, error) { return nil, "", nil }}
