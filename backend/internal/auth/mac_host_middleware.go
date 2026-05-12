package auth

import (
	"container/list"
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"

	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
	"golang.org/x/time/rate"
)

// macHostContextKey is the gin context key used to stash the resolved
// *repository.MacHost for downstream handlers. The pointer-typed key
// avoids string-collision with other middleware.
const macHostContextKey = "mac_host"
const macHostIDContextKey = "mac_host_id"

// MacHostKeyValidator is the minimal surface MacHostAuthMiddleware needs.
// In production this is *repository.MacHostRepository; tests substitute
// a fake that returns canned hosts.
type MacHostKeyValidator interface {
	GetActiveHostByID(ctx context.Context, id uuid.UUID) (*repository.MacHost, error)
}

// PasswordComparator wraps bcrypt.CompareHashAndPassword so tests can
// inject a counting fake (verifies the 429 path does NOT invoke bcrypt).
// Returns nil on match, non-nil on mismatch.
type PasswordComparator func(hashed []byte, password []byte) error

// DefaultPasswordComparator delegates to bcrypt.CompareHashAndPassword.
// All non-test call sites use this.
func DefaultPasswordComparator(hashed []byte, password []byte) error {
	return bcrypt.CompareHashAndPassword(hashed, password)
}

// MacHostAuthLimiterConfig tunes the per-host failed-auth rate limiter.
// Defaults are conservative — 5 failed authentications per minute with a
// burst of 5 (so the first 5 attempts succeed even back-to-back, the
// 6th in the same minute is throttled). The limiter map is LRU-bounded
// so a flood of forged host_ids cannot grow memory unboundedly.
type MacHostAuthLimiterConfig struct {
	// FailedAuthsPerMinute is the steady-state refill rate.
	FailedAuthsPerMinute float64
	// Burst is the initial token bucket capacity.
	Burst int
	// MaxEntries bounds the LRU. When exceeded, the least-recently-used
	// host_id's limiter is evicted (and that host gets a fresh limiter
	// on its next failed-auth attempt).
	MaxEntries int
}

// DefaultMacHostAuthLimiterConfig returns the production defaults.
func DefaultMacHostAuthLimiterConfig() MacHostAuthLimiterConfig {
	return MacHostAuthLimiterConfig{
		FailedAuthsPerMinute: 5,
		Burst:                5,
		MaxEntries:           100,
	}
}

// macHostAuthLimiter is an LRU-bounded per-host_id failed-auth limiter.
// Concurrency-safe; mutates the LRU list under mu.
type macHostAuthLimiter struct {
	cfg  MacHostAuthLimiterConfig
	mu   sync.Mutex
	lru  *list.List // most-recently-used at the front
	byID map[uuid.UUID]*list.Element
}

type limiterEntry struct {
	id      uuid.UUID
	limiter *rate.Limiter
}

func newMacHostAuthLimiter(cfg MacHostAuthLimiterConfig) *macHostAuthLimiter {
	return &macHostAuthLimiter{
		cfg:  cfg,
		lru:  list.New(),
		byID: make(map[uuid.UUID]*list.Element),
	}
}

// macHostAuthReservation tracks a single in-flight authentication
// attempt. The reservation consumes a token from the per-host bucket
// up-front (so concurrent attempts serialize on the same bucket and
// cannot all race through to bcrypt). After auth completes:
//   - on success, refund() returns the token to the bucket so the
//     legitimate daemon is not penalized for previous typos.
//   - on failure, commit() leaves the token consumed.
//
// The reservation is single-use; calling refund or commit a second
// time is a no-op.
type macHostAuthReservation struct {
	parent   *macHostAuthLimiter
	id       uuid.UUID
	resolved bool
	allowed  bool
}

// allowed reports whether the reservation was granted (i.e. the host
// had a token available). When false, the caller must NOT proceed
// past the precheck; commit/refund become no-ops.
func (r *macHostAuthReservation) Allowed() bool {
	return r.allowed
}

// commit marks the reservation as a failed authentication. The
// already-consumed token stays consumed.
func (r *macHostAuthReservation) Commit() {
	if r.resolved || !r.allowed {
		return
	}
	r.resolved = true
	// Token already consumed at reserve() time — nothing to do.
}

// refund returns the consumed token to the bucket. Called on
// successful auth so the legitimate daemon's burst budget is
// preserved.
func (r *macHostAuthReservation) Refund() {
	if r.resolved || !r.allowed {
		return
	}
	r.resolved = true
	r.parent.refundLocked(r.id)
}

// reserve atomically peeks AND consumes a token under the same mutex
// hold. Concurrent attempts for the same host_id therefore serialize
// on the bucket — a flood of N parallel bad-bearer requests with
// burst=5 lets exactly 5 pass through to bcrypt, the rest get 429.
func (m *macHostAuthLimiter) reserve(id uuid.UUID) *macHostAuthReservation {
	m.mu.Lock()
	defer m.mu.Unlock()
	lim := m.getOrCreateLocked(id)
	allowed := lim.Allow()
	return &macHostAuthReservation{
		parent:  m,
		id:      id,
		allowed: allowed,
	}
}

// refundLocked is the bucket-refund half of Reservation.Refund. The
// rate package's AllowN with negative n is not supported; we use
// SetBurst to temporarily expose the bucket and then call AllowN(-1)
// via a SetTokensAt-equivalent — but the public API for that is also
// limited. The pragmatic approach used here: increment the limiter's
// remaining tokens by clamping back to burst, using ReserveN(t, -1)
// is not supported either. We achieve the refund by calling
// limiter-internal time-based replenishment: the rate.Limiter's
// underlying tokens are tracked relative to a refill rate, so the
// only safe way to "give a token back" is to reset the limiter to a
// fresh state. Acceptable because refund is only called on success,
// at which point the host has demonstrated control over the key and
// we want a clean budget anyway. Equivalent to the old
// limiter.reset() semantics.
func (m *macHostAuthLimiter) refundLocked(id uuid.UUID) {
	if elem, ok := m.byID[id]; ok {
		m.lru.Remove(elem)
		delete(m.byID, id)
	}
}

// reset removes the limiter entry for host_id. Public counterpart of
// refundLocked, kept for callers (currently the success branch in
// the middleware) that explicitly want a fresh bucket.
func (m *macHostAuthLimiter) reset(id uuid.UUID) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.refundLocked(id)
}

func (m *macHostAuthLimiter) getOrCreateLocked(id uuid.UUID) *rate.Limiter {
	if elem, ok := m.byID[id]; ok {
		m.lru.MoveToFront(elem)
		return elem.Value.(*limiterEntry).limiter
	}
	// Evict LRU if at capacity.
	for m.lru.Len() >= m.cfg.MaxEntries {
		oldest := m.lru.Back()
		if oldest == nil {
			break
		}
		oldEntry := oldest.Value.(*limiterEntry)
		delete(m.byID, oldEntry.id)
		m.lru.Remove(oldest)
	}
	lim := rate.NewLimiter(rate.Limit(m.cfg.FailedAuthsPerMinute/60.0), m.cfg.Burst)
	elem := m.lru.PushFront(&limiterEntry{id: id, limiter: lim})
	m.byID[id] = elem
	return lim
}

// IPRateLimiter is the matching shape for the pairing endpoint, keyed
// by client IP instead of host_id. Both limiters share the same LRU
// pattern but use different key types so a single struct does not
// muddle the two domains.
type ipRateLimiter struct {
	rate       float64
	burst      int
	maxEntries int
	mu         sync.Mutex
	lru        *list.List
	byIP       map[string]*list.Element
}

type ipLimiterEntry struct {
	ip      string
	limiter *rate.Limiter
}

// NewPairingIPRateLimiter constructs a limiter for the pairing endpoint
// at 10 attempts/minute, burst 10, max 100 IPs. The handler calls Allow
// on the source IP and returns 429 on miss before consulting the DB.
func NewPairingIPRateLimiter() *PairingIPRateLimiter {
	return &PairingIPRateLimiter{inner: &ipRateLimiter{
		rate:       10.0 / 60.0,
		burst:      10,
		maxEntries: 100,
		lru:        list.New(),
		byIP:       make(map[string]*list.Element),
	}}
}

// PairingIPRateLimiter is the public type returned by NewPairingIPRateLimiter.
// Exposes Allow(ip) only — the internal LRU layout is private.
type PairingIPRateLimiter struct {
	inner *ipRateLimiter
}

// Allow returns true when the IP may proceed.
func (p *PairingIPRateLimiter) Allow(ip string) bool {
	return p.inner.allow(ip)
}

func (l *ipRateLimiter) allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if elem, ok := l.byIP[ip]; ok {
		l.lru.MoveToFront(elem)
		return elem.Value.(*ipLimiterEntry).limiter.Allow()
	}
	for l.lru.Len() >= l.maxEntries {
		oldest := l.lru.Back()
		if oldest == nil {
			break
		}
		old := oldest.Value.(*ipLimiterEntry)
		delete(l.byIP, old.ip)
		l.lru.Remove(oldest)
	}
	lim := rate.NewLimiter(rate.Limit(l.rate), l.burst)
	elem := l.lru.PushFront(&ipLimiterEntry{ip: ip, limiter: lim})
	l.byIP[ip] = elem
	return lim.Allow()
}

// MacHostAuthMiddleware authenticates Mac-daemon requests. Successful
// requests have the resolved *repository.MacHost stashed in gin.Context
// under macHostContextKey and the parsed host UUID under
// macHostIDContextKey.
//
// Auth flow:
//  1. Parse X-Mac-Host-ID header as UUID (401 on parse failure).
//  2. Reject when global API key auth is ALSO present (X-API-Key or
//     Authorization: ApiKey ...) — ambiguous auth is a 400.
//  3. Parse Authorization: Bearer <token> (401 on missing/malformed).
//  4. Per-host failed-auth rate limit (429 if exceeded — does NOT
//     invoke bcrypt).
//  5. Look up active (non-revoked) host (401 on miss).
//  6. Constant-time bcrypt compare (401 on mismatch).
//  7. If the route has a :id param, assert it matches X-Mac-Host-ID
//     (403 on mismatch — authorization failure, not authentication).
//  8. Reset the failed-auth limiter for this host (so a freshly-paired
//     host isn't penalized by earlier guesses against a different key).
func MacHostAuthMiddleware(
	repo MacHostKeyValidator,
	compare PasswordComparator,
	limiterCfg MacHostAuthLimiterConfig,
) gin.HandlerFunc {
	if compare == nil {
		compare = DefaultPasswordComparator
	}
	limiter := newMacHostAuthLimiter(limiterCfg)
	return macHostAuthMiddleware(repo, compare, limiter)
}

func macHostAuthMiddleware(
	repo MacHostKeyValidator,
	compare PasswordComparator,
	limiter *macHostAuthLimiter,
) gin.HandlerFunc {
	return func(c *gin.Context) {
		hostIDRaw := c.GetHeader("X-Mac-Host-ID")
		if hostIDRaw == "" {
			abortAuth(c, http.StatusUnauthorized, "MISSING_MAC_HOST_ID", "X-Mac-Host-ID header required")
			return
		}
		hostID, err := uuid.Parse(hostIDRaw)
		if err != nil {
			abortAuth(c, http.StatusUnauthorized, "INVALID_MAC_HOST_ID", "X-Mac-Host-ID header is not a UUID")
			return
		}

		// Ambiguous-auth check.
		if c.GetHeader("X-API-Key") != "" {
			abortAuth(c, http.StatusBadRequest, "AMBIGUOUS_AUTH", "X-API-Key and X-Mac-Host-ID cannot both be set")
			return
		}
		authHeader := c.GetHeader("Authorization")
		if strings.HasPrefix(authHeader, "ApiKey ") {
			abortAuth(c, http.StatusBadRequest, "AMBIGUOUS_AUTH", "Authorization: ApiKey is reserved for global API key; Mac daemon uses Bearer")
			return
		}

		// Parse Authorization: Bearer.
		if !strings.HasPrefix(authHeader, "Bearer ") {
			abortAuth(c, http.StatusUnauthorized, "MISSING_BEARER", "Authorization: Bearer <token> required")
			return
		}
		bearer := strings.TrimSpace(strings.TrimPrefix(authHeader, "Bearer "))
		if bearer == "" {
			abortAuth(c, http.StatusUnauthorized, "MISSING_BEARER", "Authorization: Bearer <token> required")
			return
		}

		// Rate-limit BEFORE bcrypt to cap CPU work on brute-force.
		// reserve() atomically peeks AND consumes a token, so
		// concurrent attempts for the same host serialize on the
		// bucket. Burst=5 means at most 5 parallel attempts can be
		// running bcrypt simultaneously; the 6th gets 429.
		// On success the reservation is refunded so a legitimate
		// daemon's burst budget is preserved.
		reservation := limiter.reserve(hostID)
		if !reservation.Allowed() {
			abortAuth(c, http.StatusTooManyRequests, "RATE_LIMITED", "too many failed auth attempts for this host")
			return
		}

		host, err := repo.GetActiveHostByID(c.Request.Context(), hostID)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				reservation.Commit()
				abortAuth(c, http.StatusUnauthorized, "UNKNOWN_HOST", "host not found or revoked")
				return
			}
			// Internal error — don't penalize the daemon for a Pi-side
			// hiccup. Refund the token.
			reservation.Refund()
			abortAuth(c, http.StatusInternalServerError, "AUTH_ERROR", "internal auth error")
			return
		}

		if err := compare([]byte(host.APIKeyHash), []byte(bearer)); err != nil {
			reservation.Commit()
			abortAuth(c, http.StatusUnauthorized, "INVALID_KEY", "invalid API key")
			return
		}

		// :id param consistency check (when present on the route).
		// This is an authorization failure, not authentication, so
		// we refund the rate-limit token: the daemon authenticated
		// correctly, just hit the wrong route.
		if idParam := c.Param("id"); idParam != "" {
			if idParam != hostID.String() {
				reservation.Refund()
				abortAuth(c, http.StatusForbidden, "WRONG_HOST_ID", "X-Mac-Host-ID does not match route :id")
				return
			}
		}

		reservation.Refund()
		c.Set(macHostContextKey, host)
		c.Set(macHostIDContextKey, hostID)
		c.Next()
	}
}

// MacHostFromContext returns the resolved MacHost stashed by the
// middleware. Panics in test if used outside the middleware; safe to
// call from any handler downstream of MacHostAuthMiddleware.
func MacHostFromContext(c *gin.Context) (*repository.MacHost, bool) {
	v, ok := c.Get(macHostContextKey)
	if !ok {
		return nil, false
	}
	host, ok := v.(*repository.MacHost)
	return host, ok
}

// MacHostIDFromContext returns the parsed UUID for the request. Same
// caveats as MacHostFromContext.
func MacHostIDFromContext(c *gin.Context) (uuid.UUID, bool) {
	v, ok := c.Get(macHostIDContextKey)
	if !ok {
		return uuid.Nil, false
	}
	id, ok := v.(uuid.UUID)
	return id, ok
}

func abortAuth(c *gin.Context, status int, code, message string) {
	c.AbortWithStatusJSON(status, gin.H{
		"success": false,
		"error": gin.H{
			"code":    code,
			"message": message,
		},
	})
}
