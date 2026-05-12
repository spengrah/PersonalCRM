package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// fakeHostRepo is a deterministic MacHostKeyValidator for middleware
// tests. The map is keyed on the UUID; bcrypt hashes for tests use the
// literal plaintext (paired with countingComparator).
type fakeHostRepo struct {
	hosts map[uuid.UUID]*repository.MacHost
}

func (r *fakeHostRepo) GetActiveHostByID(_ context.Context, id uuid.UUID) (*repository.MacHost, error) {
	host, ok := r.hosts[id]
	if !ok {
		return nil, db.ErrNotFound
	}
	return host, nil
}

// countingComparator is a test stub PasswordComparator. Returns nil
// when hashed == password (treats the "hash" as the plaintext for the
// purposes of the test). Counts invocations so tests can assert that
// rate-limited paths do NOT invoke compare.
type countingComparator struct {
	calls atomic.Int64
}

func (c *countingComparator) Compare(hashed []byte, password []byte) error {
	c.calls.Add(1)
	if string(hashed) == string(password) {
		return nil
	}
	return errors.New("mismatch")
}

func newMacAuthTestRouter(t *testing.T, repo MacHostKeyValidator, cmp PasswordComparator, cfg MacHostAuthLimiterConfig) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(MacHostAuthMiddleware(repo, cmp, cfg))
	r.GET("/test", func(c *gin.Context) {
		host, ok := MacHostFromContext(c)
		if !ok {
			c.JSON(http.StatusInternalServerError, gin.H{"err": "no host"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"id": host.ID.String()})
	})
	r.GET("/host/:id/check", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	return r
}

func TestMacHostAuth_HappyPath(t *testing.T) {
	id := uuid.New()
	repo := &fakeHostRepo{hosts: map[uuid.UUID]*repository.MacHost{
		id: {ID: id, APIKeyHash: "shared-secret"},
	}}
	cmp := &countingComparator{}

	r := newMacAuthTestRouter(t, repo, cmp.Compare, DefaultMacHostAuthLimiterConfig())
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("X-Mac-Host-ID", id.String())
	req.Header.Set("Authorization", "Bearer shared-secret")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	require.Equal(t, int64(1), cmp.calls.Load())
}

func TestMacHostAuth_MissingHeader(t *testing.T) {
	id := uuid.New()
	repo := &fakeHostRepo{hosts: map[uuid.UUID]*repository.MacHost{
		id: {ID: id, APIKeyHash: "shared-secret"},
	}}
	cmp := &countingComparator{}
	r := newMacAuthTestRouter(t, repo, cmp.Compare, DefaultMacHostAuthLimiterConfig())

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusUnauthorized, w.Code)
	require.Equal(t, int64(0), cmp.calls.Load(), "bcrypt must not be invoked on missing-header path")
}

func TestMacHostAuth_MalformedUUID(t *testing.T) {
	repo := &fakeHostRepo{}
	r := newMacAuthTestRouter(t, repo, DefaultPasswordComparator, DefaultMacHostAuthLimiterConfig())

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("X-Mac-Host-ID", "not-a-uuid")
	req.Header.Set("Authorization", "Bearer x")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestMacHostAuth_AmbiguousAuth_XAPIKey(t *testing.T) {
	id := uuid.New()
	repo := &fakeHostRepo{hosts: map[uuid.UUID]*repository.MacHost{
		id: {ID: id, APIKeyHash: "shared-secret"},
	}}
	cmp := &countingComparator{}
	r := newMacAuthTestRouter(t, repo, cmp.Compare, DefaultMacHostAuthLimiterConfig())

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("X-Mac-Host-ID", id.String())
	req.Header.Set("X-API-Key", "global-key")
	req.Header.Set("Authorization", "Bearer shared-secret")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusBadRequest, w.Code, "ambiguous-auth must be 400")
	require.Equal(t, int64(0), cmp.calls.Load(), "bcrypt must not run on ambiguous-auth path")
}

func TestMacHostAuth_AmbiguousAuth_ApiKeyScheme(t *testing.T) {
	id := uuid.New()
	repo := &fakeHostRepo{hosts: map[uuid.UUID]*repository.MacHost{
		id: {ID: id, APIKeyHash: "shared-secret"},
	}}
	cmp := &countingComparator{}
	r := newMacAuthTestRouter(t, repo, cmp.Compare, DefaultMacHostAuthLimiterConfig())

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("X-Mac-Host-ID", id.String())
	req.Header.Set("Authorization", "ApiKey global-key")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusBadRequest, w.Code)
}

func TestMacHostAuth_RevokedOrMissingHost_401(t *testing.T) {
	repo := &fakeHostRepo{hosts: map[uuid.UUID]*repository.MacHost{}}
	cmp := &countingComparator{}
	r := newMacAuthTestRouter(t, repo, cmp.Compare, DefaultMacHostAuthLimiterConfig())

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("X-Mac-Host-ID", uuid.New().String())
	req.Header.Set("Authorization", "Bearer something")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusUnauthorized, w.Code)
	require.Equal(t, int64(0), cmp.calls.Load(), "bcrypt must not run when host is unknown")
}

func TestMacHostAuth_InvalidKey_401(t *testing.T) {
	id := uuid.New()
	repo := &fakeHostRepo{hosts: map[uuid.UUID]*repository.MacHost{
		id: {ID: id, APIKeyHash: "expected"},
	}}
	cmp := &countingComparator{}
	r := newMacAuthTestRouter(t, repo, cmp.Compare, DefaultMacHostAuthLimiterConfig())

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("X-Mac-Host-ID", id.String())
	req.Header.Set("Authorization", "Bearer wrong")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusUnauthorized, w.Code)
	require.Equal(t, int64(1), cmp.calls.Load(), "bcrypt runs once for the failing attempt")
}

func TestMacHostAuth_IDParamMismatch_403(t *testing.T) {
	id := uuid.New()
	other := uuid.New()
	repo := &fakeHostRepo{hosts: map[uuid.UUID]*repository.MacHost{
		id: {ID: id, APIKeyHash: "secret"},
	}}
	cmp := &countingComparator{}
	r := newMacAuthTestRouter(t, repo, cmp.Compare, DefaultMacHostAuthLimiterConfig())

	req := httptest.NewRequest(http.MethodGet, "/host/"+other.String()+"/check", nil)
	req.Header.Set("X-Mac-Host-ID", id.String())
	req.Header.Set("Authorization", "Bearer secret")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusForbidden, w.Code)
}

func TestMacHostAuth_RateLimit_429AfterBurst(t *testing.T) {
	id := uuid.New()
	repo := &fakeHostRepo{hosts: map[uuid.UUID]*repository.MacHost{
		id: {ID: id, APIKeyHash: "expected"},
	}}
	cmp := &countingComparator{}
	cfg := MacHostAuthLimiterConfig{
		FailedAuthsPerMinute: 5,
		Burst:                5,
		MaxEntries:           10,
	}
	r := newMacAuthTestRouter(t, repo, cmp.Compare, cfg)

	makeReq := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		req.Header.Set("X-Mac-Host-ID", id.String())
		req.Header.Set("Authorization", "Bearer wrong")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}

	// Burst=5 → first 5 attempts consume tokens, all 401 from bcrypt mismatch.
	for i := 0; i < 5; i++ {
		w := makeReq()
		require.Equal(t, http.StatusUnauthorized, w.Code, "attempt %d", i+1)
	}
	require.Equal(t, int64(5), cmp.calls.Load(), "bcrypt called 5 times on the burst")

	// 6th attempt should hit the limiter — 429 and bcrypt NOT called.
	w := makeReq()
	require.Equal(t, http.StatusTooManyRequests, w.Code)
	require.Equal(t, int64(5), cmp.calls.Load(), "bcrypt must not run on rate-limited path")
}

func TestMacHostAuth_RateLimit_ResetsOnSuccess(t *testing.T) {
	id := uuid.New()
	repo := &fakeHostRepo{hosts: map[uuid.UUID]*repository.MacHost{
		id: {ID: id, APIKeyHash: "expected"},
	}}
	cmp := &countingComparator{}
	cfg := MacHostAuthLimiterConfig{
		FailedAuthsPerMinute: 5,
		Burst:                5,
		MaxEntries:           10,
	}
	r := newMacAuthTestRouter(t, repo, cmp.Compare, cfg)

	// Burn 3 failures.
	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		req.Header.Set("X-Mac-Host-ID", id.String())
		req.Header.Set("Authorization", "Bearer wrong")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		require.Equal(t, http.StatusUnauthorized, w.Code)
	}

	// Good auth resets the limiter.
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("X-Mac-Host-ID", id.String())
	req.Header.Set("Authorization", "Bearer expected")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	// Now we should be able to fail 5 more times before 429.
	for i := 0; i < 5; i++ {
		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		req.Header.Set("X-Mac-Host-ID", id.String())
		req.Header.Set("Authorization", "Bearer wrong")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		require.Equal(t, http.StatusUnauthorized, w.Code, "post-reset attempt %d", i+1)
	}
}

func TestMacHostAuth_LRU_EvictsLeastRecent(t *testing.T) {
	repo := &fakeHostRepo{hosts: map[uuid.UUID]*repository.MacHost{}}
	cmp := &countingComparator{}
	cfg := MacHostAuthLimiterConfig{
		FailedAuthsPerMinute: 1,
		Burst:                1,
		MaxEntries:           2,
	}
	r := newMacAuthTestRouter(t, repo, cmp.Compare, cfg)

	// Burn id1's burst.
	id1, id2, id3 := uuid.New(), uuid.New(), uuid.New()
	burn := func(id uuid.UUID) {
		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		req.Header.Set("X-Mac-Host-ID", id.String())
		req.Header.Set("Authorization", "Bearer x")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
	}
	// id1 burst consumed, second attempt would be 429.
	burn(id1)
	// id2 enters.
	burn(id2)
	// id3 enters — id1 evicted (LRU).
	burn(id3)
	// id1 should now get a fresh limiter — 401 not 429.
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("X-Mac-Host-ID", id1.String())
	req.Header.Set("Authorization", "Bearer x")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusUnauthorized, w.Code, "id1 should have fresh limiter after eviction")
}

func TestPairingIPRateLimiter(t *testing.T) {
	lim := NewPairingIPRateLimiter()
	// burst=10
	for i := 0; i < 10; i++ {
		require.True(t, lim.Allow("1.2.3.4"), "attempt %d should pass", i+1)
	}
	require.False(t, lim.Allow("1.2.3.4"), "11th attempt should be limited")
	// Different IP gets its own bucket.
	require.True(t, lim.Allow("5.6.7.8"))
}

// errBodyCode probes the JSON error body returned by abortAuth.
func TestMacHostAuth_ErrorBodyShape(t *testing.T) {
	repo := &fakeHostRepo{}
	r := newMacAuthTestRouter(t, repo, DefaultPasswordComparator, DefaultMacHostAuthLimiterConfig())

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusUnauthorized, w.Code)
	var body map[string]any
	require.NoError(t, json.NewDecoder(strings.NewReader(w.Body.String())).Decode(&body))
	errObj, ok := body["error"].(map[string]any)
	require.True(t, ok)
	require.Contains(t, errObj, "code")
	require.Contains(t, errObj, "message")
}
