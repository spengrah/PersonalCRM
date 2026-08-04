//go:build integration_testdb

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"personal-crm/backend/internal/api/handlers"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	wapkg "personal-crm/backend/internal/whatsapp"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeWhatsAppChatSettings drives the chat endpoints without a database. The
// service's own logic (the effective-tracking predicate) is covered by
// chatsettings_test.go; what these tests pin is the HTTP contract over it.
type fakeWhatsAppChatSettings struct {
	mu sync.Mutex

	chats     []wapkg.ChatWithTracking
	listErr   error
	setResult *wapkg.ChatWithTracking
	setErr    error

	setCalls []struct{ jid, status string }
}

func (f *fakeWhatsAppChatSettings) ListChats(_ context.Context) ([]wapkg.ChatWithTracking, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.chats, f.listErr
}

func (f *fakeWhatsAppChatSettings) SetChatStatus(_ context.Context, chatJID, status string) (*wapkg.ChatWithTracking, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setCalls = append(f.setCalls, struct{ jid, status string }{chatJID, status})
	if f.setErr != nil {
		return nil, f.setErr
	}
	return f.setResult, nil
}

func (f *fakeWhatsAppChatSettings) calls() []struct{ jid, status string } {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]struct{ jid, status string }(nil), f.setCalls...)
}

var _ handlers.WhatsAppChatSettings = (*fakeWhatsAppChatSettings)(nil)

func chatWith(jid, title string, memberCount *int32, status string, tracked bool) wapkg.ChatWithTracking {
	cfg := repository.WhatsAppChatConfig{
		ChatJID:     jid,
		ChatType:    "group",
		MemberCount: memberCount,
		Status:      status,
	}
	if title != "" {
		cfg.ChatTitle = &title
	}
	return wapkg.ChatWithTracking{WhatsAppChatConfig: cfg, EffectiveTracked: tracked}
}

func int32ptr(v int32) *int32 { return &v }

// decodeWhatsAppChats pulls the chat list out of the API envelope.
func decodeWhatsAppChats(t *testing.T, rec *httptest.ResponseRecorder) []handlers.WhatsAppChatResponse {
	t.Helper()
	var envelope struct {
		Data []handlers.WhatsAppChatResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &envelope))
	return envelope.Data
}

func decodeWhatsAppChat(t *testing.T, rec *httptest.ResponseRecorder) handlers.WhatsAppChatResponse {
	t.Helper()
	var envelope struct {
		Data handlers.WhatsAppChatResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &envelope))
	return envelope.Data
}

// --- WHA-078: group tracking endpoints ---------------------------------------

func TestWhatsAppChatsAPI_ListReturnsObservedChats(t *testing.T) {
	// spec: WHA-078.list-returns-observed-chats-with-effective-decision
	t.Parallel()
	chats := &fakeWhatsAppChatSettings{chats: []wapkg.ChatWithTracking{
		chatWith("111-222@g.us", "Book Club", int32ptr(4), "auto", true),
		chatWith("333-444@g.us", "Big Group", int32ptr(400), "auto", false),
		chatWith("555-666@g.us", "", nil, "tracked", true),
	}}
	router := setupWhatsAppRouterWithChats(t, &fakeWhatsAppManager{}, chats)

	rec := doWhatsAppRequest(t, router, http.MethodGet, "/api/v1/whatsapp/chats", nil)

	require.Equal(t, http.StatusOK, rec.Code)
	got := decodeWhatsAppChats(t, rec)
	require.Len(t, got, 3)

	assert.Equal(t, "111-222@g.us", got[0].ChatJID)
	require.NotNil(t, got[0].ChatTitle)
	assert.Equal(t, "Book Club", *got[0].ChatTitle)
	assert.Equal(t, "group", got[0].ChatType)
	require.NotNil(t, got[0].MemberCount)
	assert.EqualValues(t, 4, *got[0].MemberCount)
	assert.Equal(t, "auto", got[0].Status)
	assert.True(t, got[0].EffectiveTracked)

	// A large auto group carries the same fields with the opposite decision.
	assert.False(t, got[1].EffectiveTracked)

	// A chat whose title and size were never resolved keeps both absent rather
	// than collapsing to an empty string or a zero that would read as a size.
	assert.Nil(t, got[2].ChatTitle)
	assert.Nil(t, got[2].MemberCount)
	assert.True(t, got[2].EffectiveTracked, "an explicit override needs no size")
}

// TestWhatsAppChatsAPI_ListWithNoChatsReturnsEmptyArray guards the client: a
// null would make its length check throw rather than render the empty state.
func TestWhatsAppChatsAPI_ListWithNoChatsReturnsEmptyArray(t *testing.T) {
	t.Parallel()
	router := setupWhatsAppRouterWithChats(t, &fakeWhatsAppManager{}, &fakeWhatsAppChatSettings{})

	rec := doWhatsAppRequest(t, router, http.MethodGet, "/api/v1/whatsapp/chats", nil)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"data":[]`, "an empty list must serialise as [], never null")
	assert.Empty(t, decodeWhatsAppChats(t, rec))
}

func TestWhatsAppChatsAPI_ListReportsAStoreFailure(t *testing.T) {
	t.Parallel()
	chats := &fakeWhatsAppChatSettings{listErr: errors.New("connection refused")}
	router := setupWhatsAppRouterWithChats(t, &fakeWhatsAppManager{}, chats)

	rec := doWhatsAppRequest(t, router, http.MethodGet, "/api/v1/whatsapp/chats", nil)

	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestWhatsAppChatsAPI_UpdateStatusPersistsAndEchoes(t *testing.T) {
	// spec: WHA-078.override-persists-and-is-echoed-back
	t.Parallel()
	cases := []struct {
		name        string
		status      string
		memberCount *int32
		tracked     bool
	}{
		{"tracked with a known size", "tracked", int32ptr(400), true},
		{"tracked with an unknown size", "tracked", nil, true},
		{"ignored with a known size", "ignored", int32ptr(3), false},
		{"ignored with an unknown size", "ignored", nil, false},
		{"auto with a small known size", "auto", int32ptr(3), true},
		{"auto with an unknown size fails closed", "auto", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			updated := chatWith("111-222@g.us", "Book Club", tc.memberCount, tc.status, tc.tracked)
			chats := &fakeWhatsAppChatSettings{setResult: &updated}
			router := setupWhatsAppRouterWithChats(t, &fakeWhatsAppManager{}, chats)

			rec := doWhatsAppRequest(t, router, http.MethodPatch,
				"/api/v1/whatsapp/chats/111-222%40g.us",
				handlers.WhatsAppChatStatusRequest{Status: tc.status})

			require.Equal(t, http.StatusOK, rec.Code)
			got := decodeWhatsAppChat(t, rec)
			assert.Equal(t, tc.status, got.Status, "the echoed row carries the new override")
			assert.Equal(t, tc.tracked, got.EffectiveTracked,
				"and the decision recomputed from it, so the client needs no second read")

			calls := chats.calls()
			require.Len(t, calls, 1)
			assert.Equal(t, tc.status, calls[0].status)
		})
	}
}

func TestWhatsAppChatsAPI_UpdateStatusRejectsUnknownStatus(t *testing.T) {
	// spec: WHA-078.override-rejects-an-unknown-status
	t.Parallel()
	for _, body := range []string{`{"status":"sometimes"}`, `{}`} {
		t.Run(body, func(t *testing.T) {
			t.Parallel()
			chats := &fakeWhatsAppChatSettings{}
			router := setupWhatsAppRouterWithChats(t, &fakeWhatsAppManager{}, chats)

			rec := doWhatsAppRawRequest(t, router, http.MethodPatch,
				"/api/v1/whatsapp/chats/111-222%40g.us", body)

			require.Equal(t, http.StatusBadRequest, rec.Code)
			assert.Equal(t, "VALIDATION_ERROR", decodeWhatsAppError(t, rec).Code)
			assert.Empty(t, chats.calls(), "an invalid status never reaches the store")
		})
	}
}

func TestWhatsAppChatsAPI_UpdateStatusOnUnobservedChatIsNotFound(t *testing.T) {
	// spec: WHA-078.override-of-an-unobserved-chat-is-not-found
	t.Parallel()
	chats := &fakeWhatsAppChatSettings{setErr: db.ErrNotFound}
	router := setupWhatsAppRouterWithChats(t, &fakeWhatsAppManager{}, chats)

	rec := doWhatsAppRequest(t, router, http.MethodPatch,
		"/api/v1/whatsapp/chats/never-observed%40g.us",
		handlers.WhatsAppChatStatusRequest{Status: "tracked"})

	require.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, "NOT_FOUND", decodeWhatsAppError(t, rec).Code)
}

// TestWhatsAppChatsAPI_JIDWithAtSignRoundTrips proves the path-param transport
// rather than assuming RFC 3986 behaves: a percent-encoded JID must reach the
// handler decoded, or every override would address the wrong chat.
func TestWhatsAppChatsAPI_JIDWithAtSignRoundTrips(t *testing.T) {
	t.Parallel()
	updated := chatWith("123-456@g.us", "Round Trip", int32ptr(5), "ignored", false)
	chats := &fakeWhatsAppChatSettings{setResult: &updated}
	router := setupWhatsAppRouterWithChats(t, &fakeWhatsAppManager{}, chats)

	rec := doWhatsAppRequest(t, router, http.MethodPatch,
		"/api/v1/whatsapp/chats/123-456%40g.us",
		handlers.WhatsAppChatStatusRequest{Status: "ignored"})

	require.Equal(t, http.StatusOK, rec.Code)
	calls := chats.calls()
	require.Len(t, calls, 1)
	assert.Equal(t, "123-456@g.us", calls[0].jid, "the handler must see the decoded JID")
}

// TestWhatsAppChatsAPI_NilSeamReportsUnavailable covers the production shape a
// failed device-store open produces: the routes exist, the seam does not.
func TestWhatsAppChatsAPI_NilSeamReportsUnavailable(t *testing.T) {
	t.Parallel()
	router := setupWhatsAppRouterWithChats(t, &fakeWhatsAppManager{}, nil)

	list := doWhatsAppRequest(t, router, http.MethodGet, "/api/v1/whatsapp/chats", nil)
	assert.Equal(t, http.StatusServiceUnavailable, list.Code)

	patch := doWhatsAppRequest(t, router, http.MethodPatch,
		"/api/v1/whatsapp/chats/111-222%40g.us",
		handlers.WhatsAppChatStatusRequest{Status: "tracked"})
	assert.Equal(t, http.StatusServiceUnavailable, patch.Code)
}

// doWhatsAppRawRequest sends a body verbatim, so a payload that cannot be built
// from the typed request struct (an unknown status) still reaches binding.
func doWhatsAppRawRequest(t *testing.T, router *gin.Engine, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}
