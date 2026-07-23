package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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

// spec: MAC-007[0]
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

// spec: MAC-007[0]
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

// spec: MAC-007[1]
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

// spec: MAC-007[1]
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

// spec: MAC-007[3]
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

// spec: ING-001[1]
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

// spec: MAC-007[5]
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

func TestMacHostAuth_ConcurrentBadBearersDoNotAllPassRateLimit(t *testing.T) {
	// A flood of parallel bad-bearer requests for the SAME host must
	// not all reach bcrypt. With burst=3, at most 3 requests should
	// invoke the comparator; the remaining 7 should get 429.
	id := uuid.New()
	repo := &fakeHostRepo{hosts: map[uuid.UUID]*repository.MacHost{
		id: {ID: id, APIKeyHash: "expected"},
	}}
	cmp := &countingComparator{}
	cfg := MacHostAuthLimiterConfig{
		FailedAuthsPerMinute: 3,
		Burst:                3,
		MaxEntries:           10,
	}
	r := newMacAuthTestRouter(t, repo, cmp.Compare, cfg)

	const N = 10
	var wg sync.WaitGroup
	codes := make([]int, N)
	wg.Add(N)
	for i := 0; i < N; i++ {
		i := i
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodGet, "/test", nil)
			req.Header.Set("X-Mac-Host-ID", id.String())
			req.Header.Set("Authorization", "Bearer wrong")
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			codes[i] = w.Code
		}()
	}
	wg.Wait()

	var unauth, rateLimited int
	for _, code := range codes {
		switch code {
		case http.StatusUnauthorized:
			unauth++
		case http.StatusTooManyRequests:
			rateLimited++
		default:
			t.Fatalf("unexpected status %d", code)
		}
	}
	require.LessOrEqual(t, unauth, cfg.Burst,
		"at most burst=%d bcrypt-running attempts; got %d unauth + %d 429",
		cfg.Burst, unauth, rateLimited)
	require.Equal(t, N-unauth, rateLimited, "the remainder must be rate-limited")
	require.LessOrEqual(t, int(cmp.calls.Load()), cfg.Burst,
		"bcrypt must be called at most burst times under concurrent flood")
}

func TestMacHostAuth_SuccessDoesNotConsumeToken(t *testing.T) {
	// A valid daemon presenting the correct key after burst-1 of
	// failures must STILL be allowed through. This regression test
	// guards the "successful auth does not consume a rate-limit
	// token" property required for legitimate operators who typo
	// their key once or twice before pasting the right one.
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

	// Burst-1 (4) failures with the wrong key. Each consumes a token.
	for i := 0; i < 4; i++ {
		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		req.Header.Set("X-Mac-Host-ID", id.String())
		req.Header.Set("Authorization", "Bearer wrong")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		require.Equal(t, http.StatusUnauthorized, w.Code, "failure attempt %d", i+1)
	}

	// One token left — a correct bearer must succeed AND not consume
	// it. After this, the daemon should still have at least one
	// failure budget before 429.
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("X-Mac-Host-ID", id.String())
	req.Header.Set("Authorization", "Bearer expected")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, "valid auth after burst-1 failures must succeed")

	// Limiter is reset on success — burst should be replenished.
	for i := 0; i < 5; i++ {
		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		req.Header.Set("X-Mac-Host-ID", id.String())
		req.Header.Set("Authorization", "Bearer wrong")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		require.Equal(t, http.StatusUnauthorized, w.Code, "post-success attempt %d", i+1)
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
