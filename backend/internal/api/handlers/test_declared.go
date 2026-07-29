package handlers

import (
	"errors"
	"net/http"
	"time"

	"personal-crm/backend/internal/api"
	"personal-crm/backend/internal/synthetic/declare"

	"github.com/gin-gonic/gin"
)

// Declared-seeding endpoints. These are the ONE provisioning path the E2E suite
// is migrating to: a test names a spec behavior, the server executes that
// behavior's declared fixture in an isolated namespace, and a later request
// removes it.
//
// LAYERING EXCEPTION, deliberate and documented at the wiring site too
// (cmd/crm-api/routes.go): this handler drives internal/synthetic/declare
// DIRECTLY, with no service in between, because service cannot import synthetic
// (service → synthetic → replay → service would cycle). The layer rule still
// holds one level down — the toolkit itself writes exclusively through the real
// services and repositories, which is what makes a declared fixture reachable
// by the product. Do not "fix" this by minting a service.
//
// RIVER ISOLATION, running the toolkit inside a live server: a seed builds a
// replay harness with its OWN River client, and River clients do not partition
// river_job by owner — a client fetches whatever is available in the queues it
// is configured for. Sharing `default` with the application would let a seed
// fetch the application's jobs (finalizing no-op kinds without their work,
// running replay-mode workers on production jobs, failing unregistered kinds as
// unknown) and let the application work the seed's. Both directions are closed
// structurally rather than per worker: the harness fetches ONLY a private
// per-namespace queue and an insert hook routes everything it enqueues there
// (replay.SyntheticQueueName). The application's client fetches only `default`,
// so neither side can see the other's work.

// SeedDeclaredRequest asks for one behavior's declared fixture.
type SeedDeclaredRequest struct {
	// BehaviorID is a spec behavior id (e.g. "CAD-026").
	BehaviorID string `json:"behavior_id" validate:"required,min=1,max=64"`
	// Namespace isolates this fixture's rows. It may not end in the reserved
	// -sN re-salt suffix, and it stops short of the 60-character token limit so
	// that a re-salted EFFECTIVE namespace still fits — see
	// declare.maxRequestedNamespaceLen, which this bound mirrors and
	// TestSeedDeclaredEndpoint_MaxLengthNamespaceResalts holds it to.
	Namespace string `json:"namespace" validate:"required,min=1,max=57"`
	// Seed overrides the generator seed; 0 or absent uses the default.
	Seed uint64 `json:"seed,omitempty"`
}

// SeededEntityResponse is one created row, keyed in the manifest by the
// declaration-local handle.
type SeededEntityResponse struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
	Name string `json:"name"`
}

// SeedDeclaredResponse is the manifest of an executed declaration.
type SeedDeclaredResponse struct {
	// Namespace is the EFFECTIVE namespace, which may carry a -sN suffix the
	// caller never asked for. Assertions must key off this, never the request.
	Namespace string `json:"namespace"`
	// Anchor is the generator anchor the world was built against.
	Anchor time.Time `json:"anchor"`
	// Entities maps declaration handle → created row.
	Entities map[string]SeededEntityResponse `json:"entities"`
}

// SeedDeclaredFailure is the recovery metadata a failed seed returns in the
// envelope's data field: which namespace was involved, and whether the failure
// left rows behind. Cleaned reports reality, so a client knows whether to retry
// directly or clean up first.
type SeedDeclaredFailure struct {
	Namespace string `json:"namespace"`
	Cleaned   bool   `json:"cleaned"`
}

// SeedDeclared executes a spec behavior's declared fixture
// @Summary Seed a spec behavior's declared fixture
// @Description Execute the fixture declared for a spec behavior id in an isolated namespace and return a manifest of what was created. Test-only (CRM_ENV=testing).
// @Tags test
// @Accept json
// @Produce json
// @Param body body SeedDeclaredRequest true "Declared seed request"
// @Success 201 {object} api.APIResponse{data=SeedDeclaredResponse}
// @Failure 400 {object} api.APIResponse{error=api.APIError}
// @Failure 409 {object} api.APIResponse{error=api.APIError}
// @Failure 500 {object} api.APIResponse{data=SeedDeclaredFailure,error=api.APIError}
// @Router /test/seed/declared [post]
func (h *TestHandler) SeedDeclared(c *gin.Context) {
	var req SeedDeclaredRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		api.SendError(c, http.StatusBadRequest, api.ErrCodeValidation, "Invalid request body", err.Error())
		return
	}
	if err := h.validator.Struct(req); err != nil {
		api.SendError(c, http.StatusBadRequest, api.ErrCodeValidation, "Validation failed", err.Error())
		return
	}
	// Resolve the behavior and check the namespace grammar FIRST: a malformed
	// request must be rejected identically whether or not this instance has the
	// declared path wired.
	if _, err := declare.ValidateSeedRequest(req.BehaviorID, req.Namespace); err != nil {
		h.sendDeclaredSeedError(c, req.Namespace, err)
		return
	}
	if h.database == nil {
		api.SendInternalError(c, "declared seeding is not wired on this instance")
		return
	}

	// declare.Run detaches and deadline-bounds internally, so a client
	// disconnect cannot cancel River's fetch loop mid-settle.
	res, err := declare.Run(c.Request.Context(), h.database, req.BehaviorID, req.Namespace, req.Seed)
	if err != nil {
		h.sendDeclaredSeedError(c, req.Namespace, err)
		return
	}

	entities := make(map[string]SeededEntityResponse, len(res.Entities))
	for handle, seeded := range res.Entities {
		entities[handle] = SeededEntityResponse{Kind: seeded.Kind, ID: seeded.ID, Name: seeded.Name}
	}
	api.SendSuccess(c, http.StatusCreated, SeedDeclaredResponse{
		Namespace: res.Namespace,
		Anchor:    res.Anchor,
		Entities:  entities,
	}, nil)
}

func (h *TestHandler) sendDeclaredSeedError(c *gin.Context, requested string, err error) {
	switch {
	case errors.Is(err, declare.ErrUnknownBehavior),
		errors.Is(err, declare.ErrNoFixtureBehavior),
		errors.Is(err, declare.ErrInvalidNamespace):
		api.SendError(c, http.StatusBadRequest, api.ErrCodeValidation, "Declared seed request rejected", err.Error())
	case errors.Is(err, declare.ErrNamespaceOccupied),
		errors.Is(err, declare.ErrNamespaceNested),
		errors.Is(err, declare.ErrNamespaceBusy):
		api.SendError(c, http.StatusConflict, api.ErrCodeConflict, "Namespace unavailable", err.Error())
	default:
		// Execution failure. The body carries the recovery metadata in `data`
		// alongside the error, so a client that lost track of what happened can
		// still decide between retrying and cleaning up first. Recovery does NOT
		// depend on this body arriving: the client pre-registers the requested
		// namespace before issuing the request, and cleanup expands that token
		// to whatever the server actually created.
		failure := SeedDeclaredFailure{Namespace: requested, Cleaned: false}
		var runErr *declare.RunError
		if errors.As(err, &runErr) {
			failure.Namespace = runErr.Namespace
			failure.Cleaned = runErr.Cleaned
		}
		api.LogServerError(c, err)
		c.JSON(http.StatusInternalServerError, api.APIResponse{
			Success: false,
			Data:    failure,
			Error: &api.APIError{
				Code:    api.ErrCodeInternal,
				Message: "Declared seed failed",
				Details: err.Error(),
			},
		})
	}
}

// NamespaceCleanupResponse is one effective namespace's cleanup outcome.
// Status is one of cleaned | busy | pending | error; busy and pending are
// retriable and mean NOTHING was deleted.
type NamespaceCleanupResponse struct {
	Status      string           `json:"status"`
	Deleted     map[string]int64 `json:"deleted,omitempty"`
	Descendants []string         `json:"descendants,omitempty"`
	Error       string           `json:"error,omitempty"`
}

// CleanupNamespacesResponse reports what each REQUESTED token expanded to and
// what happened to each EFFECTIVE namespace.
type CleanupNamespacesResponse struct {
	Expansions map[string][]string                 `json:"expansions"`
	Results    map[string]NamespaceCleanupResponse `json:"results"`
}

// cleanupNamespaces handles the declared shape of POST /test/cleanup.
func (h *TestHandler) cleanupNamespaces(c *gin.Context, namespaces []string, seed uint64) {
	if err := declare.ValidateCleanupNamespaces(namespaces); err != nil {
		api.SendError(c, http.StatusBadRequest, api.ErrCodeValidation, "Invalid namespaces", err.Error())
		return
	}
	if h.database == nil {
		api.SendInternalError(c, "declared cleanup is not wired on this instance")
		return
	}

	res, err := declare.CleanupNamespaces(c.Request.Context(), h.database, namespaces, seed)
	if err != nil {
		if errors.Is(err, declare.ErrInvalidNamespace) {
			api.SendError(c, http.StatusBadRequest, api.ErrCodeValidation, "Invalid namespaces", err.Error())
			return
		}
		api.RespondInternal(c, err)
		return
	}

	results := make(map[string]NamespaceCleanupResponse, len(res.Results))
	for ns, outcome := range res.Results {
		results[ns] = NamespaceCleanupResponse{
			Status:      outcome.Status,
			Deleted:     outcome.Deleted,
			Descendants: outcome.Descendants,
			Error:       outcome.Err,
		}
	}
	api.SendSuccess(c, http.StatusOK, CleanupNamespacesResponse{
		Expansions: res.Expansions,
		Results:    results,
	}, nil)
}
