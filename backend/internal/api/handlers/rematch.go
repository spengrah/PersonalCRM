package handlers

import (
	"errors"
	"net/http"
	"time"

	"personal-crm/backend/internal/api"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// RematchHandler exposes HTTP endpoints for inspecting and triggering rematch
// jobs produced by service.RematchService.
type RematchHandler struct {
	rematchSvc *service.RematchService
	contactSvc *service.ContactService
	methodRepo *repository.ContactMethodRepository
}

// NewRematchHandler constructs a RematchHandler.
func NewRematchHandler(rematchSvc *service.RematchService, contactSvc *service.ContactService, methodRepo *repository.ContactMethodRepository) *RematchHandler {
	return &RematchHandler{
		rematchSvc: rematchSvc,
		contactSvc: contactSvc,
		methodRepo: methodRepo,
	}
}

// RematchJobMethod mirrors service.Method for HTTP responses.
type RematchJobMethod struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}

// RematchJobResponse is the external view of a rematch job.
type RematchJobResponse struct {
	ID          string             `json:"id"`
	ContactID   string             `json:"contact_id"`
	Status      string             `json:"status"`
	Matched     int                `json:"matched"`
	Methods     []RematchJobMethod `json:"methods"`
	StartedAt   time.Time          `json:"started_at"`
	CompletedAt *time.Time         `json:"completed_at,omitempty"`
	Error       string             `json:"error,omitempty"`
}

// RescanResponse is returned by the manual rescan endpoint.
type RescanResponse struct {
	RematchJobID *string `json:"rematch_job_id"`
}

// GetJob returns the current state of a rematch job.
// @Summary Get rematch job
// @Description Returns the progress of a rematch job by ID.
// @Tags rematch
// @Produce json
// @Param jobID path string true "Rematch job ID" format(uuid)
// @Success 200 {object} api.APIResponse{data=RematchJobResponse}
// @Failure 400 {object} api.APIResponse{error=api.APIError}
// @Failure 404 {object} api.APIResponse{error=api.APIError}
// @Router /rematch/jobs/{jobID} [get]
func (h *RematchHandler) GetJob(c *gin.Context) {
	id, err := uuid.Parse(c.Param("jobID"))
	if err != nil {
		api.SendValidationError(c, "Invalid job ID", "ID must be a valid UUID")
		return
	}

	job, err := h.rematchSvc.GetJob(id)
	if err != nil {
		if errors.Is(err, service.ErrJobNotFound) {
			api.SendNotFound(c, "Rematch job")
			return
		}
		api.SendInternalError(c, "Failed to load rematch job")
		return
	}

	api.SendSuccess(c, http.StatusOK, toRematchJobResponse(job), nil)
}

// Rescan triggers a full rematch across all of the contact's current methods.
// @Summary Trigger rematch for a contact
// @Description Re-runs rematch handlers for every method currently on the contact.
// @Tags rematch
// @Produce json
// @Param id path string true "Contact ID" format(uuid)
// @Success 200 {object} api.APIResponse{data=RescanResponse}
// @Failure 400 {object} api.APIResponse{error=api.APIError}
// @Failure 404 {object} api.APIResponse{error=api.APIError}
// @Router /rematch/contacts/{id}/rescan [post]
func (h *RematchHandler) Rescan(c *gin.Context) {
	ctx := c.Request.Context()

	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		api.SendValidationError(c, "Invalid contact ID", "ID must be a valid UUID")
		return
	}

	// Confirm the contact exists (and is not soft-deleted).
	if _, err := h.contactSvc.GetContact(ctx, id); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			api.SendNotFound(c, "Contact")
			return
		}
		api.SendInternalError(c, "Failed to load contact")
		return
	}

	methods, err := h.methodRepo.ListContactMethodsByContact(ctx, id)
	if err != nil {
		api.SendInternalError(c, "Failed to load contact methods")
		return
	}

	jobID := h.rematchSvc.StartRematchForContact(id, serviceMethodsFromContactMethods(methods))
	api.SendSuccess(c, http.StatusOK, RescanResponse{
		RematchJobID: nilStringPtrFromUUID(jobID),
	}, nil)
}

func toRematchJobResponse(j service.JobProgress) RematchJobResponse {
	methods := make([]RematchJobMethod, len(j.Methods))
	for i, m := range j.Methods {
		methods[i] = RematchJobMethod{Type: m.Type, Value: m.Value}
	}
	return RematchJobResponse{
		ID:          j.ID.String(),
		ContactID:   j.ContactID.String(),
		Status:      string(j.Status),
		Matched:     j.Matched,
		Methods:     methods,
		StartedAt:   j.StartedAt,
		CompletedAt: j.CompletedAt,
		Error:       j.Error,
	}
}

// serviceMethodsFromContactMethods is a handler-local helper that mirrors
// service.toRematchMethods (unexported in the service package).
func serviceMethodsFromContactMethods(methods []repository.ContactMethod) []service.Method {
	out := make([]service.Method, len(methods))
	for i, m := range methods {
		out[i] = service.Method{Type: m.Type, Value: m.ValueNormalized}
	}
	return out
}
