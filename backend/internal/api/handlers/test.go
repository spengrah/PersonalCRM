package handlers

import (
	"net/http"

	"personal-crm/backend/internal/api"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/go-playground/validator/v10"
	"github.com/google/uuid"
)

// TestHandler handles test data management endpoints
// These endpoints are only available when CRM_ENV=testing or CRM_ENV=test
//
// Provisioning is DECLARED: /test/seed/declared and the namespaces shape of
// /test/cleanup (both in test_declared.go) drive internal/synthetic/declare
// directly, under a documented layering exception — service cannot import
// synthetic without a cycle, and the toolkit itself writes through the real
// services and repositories, so the layer rule holds one level down. No bespoke
// /test/seed/* endpoint remains.
//
// What is left here is the PREFIX shape of /test/cleanup — which still has real
// work to do, because a test's own rows (contacts it created through the product's
// own API, notes it wrote) carry its prefix and no declared namespace — plus the
// error-boundary trigger. Both go through service.TestSeedService; no handler
// calls sqlc queries.
type TestHandler struct {
	seedSvc *service.TestSeedService
	lockSvc *service.TestLockService
	// database backs the declared path only (see the layering exception above).
	database  *db.Database
	validator *validator.Validate
}

// NewTestHandler creates a new test handler over the test-seed service, the
// named-mutex arbiter, and the database the declared-seeding path drives.
func NewTestHandler(seedSvc *service.TestSeedService, lockSvc *service.TestLockService, database *db.Database) *TestHandler {
	return &TestHandler{
		seedSvc:   seedSvc,
		lockSvc:   lockSvc,
		database:  database,
		validator: sharedValidator,
	}
}

// CleanupRequest represents the request to cleanup test data. It is dual-shape
// during the migration to declared seeding, and the caller must supply EXACTLY
// ONE of the two:
//
//   - prefix  — the bespoke shape: delete rows whose identifying strings carry
//     the test's prefix. Unchanged behavior.
//   - namespaces — the declared shape: remove the worlds those requested
//     namespace tokens resolved to, per-namespace outcomes.
type CleanupRequest struct {
	Prefix string `json:"prefix,omitempty" validate:"omitempty,min=1,max=50"`
	// HostID, when set, also hard-deletes every meeting_note owned by that
	// mac_host (seeded session UUIDs are random, so there is no prefix to
	// match on — cleanup is by host instead). Prefix shape only.
	HostID string `json:"host_id,omitempty" validate:"omitempty,uuid"`
	// Namespaces are the REQUESTED namespace tokens the client seeded under.
	// The server expands each to the effective (possibly re-salted) worlds, so
	// a client that never saw a seed response can still clean up.
	Namespaces []string `json:"namespaces,omitempty" validate:"omitempty,max=32,dive,min=1,max=60"`
	// Seed must match the seed the namespaces were created with; 0 or absent
	// uses the default.
	Seed uint64 `json:"seed,omitempty"`
}

// CleanupResponse represents the response from cleanup by prefix
type CleanupResponse struct {
	DeletedContacts         int64 `json:"deleted_contacts"`
	DeletedExternalContacts int64 `json:"deleted_external_contacts"`
	DeletedCalendarEvents   int64 `json:"deleted_calendar_events"`
}

// CleanupResponseData is the DOCUMENTED shape of this endpoint's 200 payload.
// The endpoint is dual-shape and Swagger 2.0 has no oneOf, so the documented
// schema is the union of the two real payloads — embedded, never re-declared,
// so it cannot drift from what the handler actually returns:
//
//   - a `prefix` request returns exactly CleanupResponse's fields;
//   - a `namespaces` request returns exactly CleanupNamespacesResponse's.
//
// No response ever carries both groups. This type exists only to make both
// schemas — including the per-namespace outcome — reachable in the generated
// spec; the handler serializes the concrete types.
type CleanupResponseData struct {
	CleanupResponse
	CleanupNamespacesResponse
}

// Cleanup deletes test data by prefix or by declared namespace
// @Summary Cleanup test data
// @Description Delete test data. Supply EXACTLY ONE of `prefix` (bespoke shape — returns the CleanupResponse fields: contacts, external contacts and calendar events deleted by prefix) or `namespaces` (declared shape — returns the CleanupNamespacesResponse fields: per-requested-token expansions plus a per-effective-namespace outcome). `host_id` belongs to the prefix shape and is rejected alongside `namespaces`. The documented 200 schema is the union of the two; a given response carries one group, never both.
// @Tags test
// @Accept json
// @Produce json
// @Param body body CleanupRequest true "Cleanup request"
// @Success 200 {object} api.APIResponse{data=CleanupResponseData}
// @Failure 400 {object} api.APIResponse{error=api.APIError}
// @Failure 500 {object} api.APIResponse{error=api.APIError}
// @Router /test/cleanup [post]
func (h *TestHandler) Cleanup(c *gin.Context) {
	ctx := c.Request.Context()

	var req CleanupRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		api.SendError(c, http.StatusBadRequest, api.ErrCodeValidation, "Invalid request body", err.Error())
		return
	}

	if err := h.validator.Struct(req); err != nil {
		api.SendError(c, http.StatusBadRequest, api.ErrCodeValidation, "Validation failed", err.Error())
		return
	}

	// Exactly one shape. Accepting both would leave the response ambiguous
	// (the two shapes report different things); accepting neither would be a
	// silent no-op that a caller could mistake for a successful cleanup.
	switch {
	case len(req.Namespaces) > 0 && req.Prefix != "":
		api.SendError(c, http.StatusBadRequest, api.ErrCodeValidation,
			"Supply exactly one of prefix or namespaces", "both were provided")
		return
	case len(req.Namespaces) == 0 && req.Prefix == "":
		api.SendError(c, http.StatusBadRequest, api.ErrCodeValidation,
			"Supply exactly one of prefix or namespaces", "neither was provided")
		return
	case len(req.Namespaces) > 0 && req.HostID != "":
		// host_id is prefix-shape only: the declared branch below has no host to
		// scope meeting notes to and would return 200 without ever looking at it.
		// A caller that sent one asked for cleanup work that silently would not
		// happen, so refuse rather than under-deliver behind a success.
		api.SendError(c, http.StatusBadRequest, api.ErrCodeValidation,
			"host_id belongs to the prefix shape", "host_id cannot be combined with namespaces")
		return
	case len(req.Namespaces) > 0:
		h.cleanupNamespaces(c, req.Namespaces, req.Seed)
		return
	}

	// Meeting notes are seeded with random session UUIDs scoped to a host, so
	// cleanup is by host id rather than by prefix. Parse it at the boundary.
	var hostID *uuid.UUID
	if req.HostID != "" {
		parsed, parseErr := uuid.Parse(req.HostID)
		if parseErr != nil {
			api.SendError(c, http.StatusBadRequest, api.ErrCodeValidation, "invalid host_id", parseErr.Error())
			return
		}
		hostID = &parsed
	}

	res, err := h.seedSvc.Cleanup(ctx, req.Prefix, hostID)
	if err != nil {
		api.RespondInternal(c, err)
		return
	}

	api.SendSuccess(c, http.StatusOK, CleanupResponse{
		DeletedContacts:         res.DeletedContacts,
		DeletedExternalContacts: res.DeletedExternalContacts,
		DeletedCalendarEvents:   res.DeletedCalendarEvents,
	}, nil)
}

// TriggerErrorRequest represents the request to trigger an error
type TriggerErrorRequest struct {
	ErrorType string `json:"error_type" validate:"required,oneof=500 panic"`
	Message   string `json:"message,omitempty"`
}

// TriggerError triggers an error for error boundary testing
// @Summary Trigger error for testing
// @Description Trigger a server error for error boundary testing
// @Tags test
// @Accept json
// @Produce json
// @Param body body TriggerErrorRequest true "Error request"
// @Failure 500 {object} api.APIResponse{error=api.APIError}
// @Router /test/trigger-error [post]
func (h *TestHandler) TriggerError(c *gin.Context) {
	var req TriggerErrorRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		api.SendError(c, http.StatusBadRequest, api.ErrCodeValidation, "Invalid request body", err.Error())
		return
	}

	message := req.Message
	if message == "" {
		message = "Test error triggered"
	}

	switch req.ErrorType {
	case "panic":
		panic(message)
	default:
		api.SendError(c, http.StatusInternalServerError, api.ErrCodeInternal, "Test error triggered", message)
	}
}
