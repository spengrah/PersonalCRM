package auth

import (
	"github.com/gin-gonic/gin"
)

// IngestAuthMiddleware composes the global API-key middleware and the
// Mac-host bearer middleware behind the single /api/v1/ingest/events
// route. The dispatch is per-request, keyed on the presence of the
// X-Mac-Host-ID header:
//
//   - X-Mac-Host-ID present → host-auth path (MacHostAuthMiddleware).
//     Daemons authenticate with their host UUID + bearer token. The
//     parsed host is stashed in gin context under macHostIDContextKey
//     so downstream handlers can read it.
//   - X-Mac-Host-ID absent → global-API-key path (APIKeyMiddleware).
//     Preserves the existing publisher contract — internal Pi
//     publishers and ops scripts continue to authenticate via the
//     global key without code changes.
//
// Both branches use the existing per-path middleware verbatim. This
// composite is purely a dispatcher; it does not run any auth logic
// itself, so the per-path 401/429/etc semantics are unchanged.
//
// Rationale for in-handler dispatch (vs. separate route registration):
// gin route trees reject a duplicate registration on the same prefix
// (the pattern used by MacHostAuthMiddleware for the daemon endpoints),
// so we cannot register /api/v1/ingest/events twice under different
// middleware groups. A composite dispatcher is the minimal seam.
func IngestAuthMiddleware(
	apiKeyMW gin.HandlerFunc,
	macHostMW gin.HandlerFunc,
) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.GetHeader("X-Mac-Host-ID") != "" {
			macHostMW(c)
			return
		}
		apiKeyMW(c)
	}
}
