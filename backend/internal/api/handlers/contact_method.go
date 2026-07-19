package handlers

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"personal-crm/backend/internal/api"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/go-playground/validator/v10"
	"github.com/google/uuid"
)

// ContactMethodApplier is the narrow contract the handler depends on.
//
// Declared as an interface rather than the concrete service so the handler's
// error-to-HTTP mapping is testable with a stub. That matters specifically for
// ErrMethodValueConflict: a correct fold rejects trigger collisions before any
// statement runs, so the database backstop cannot be reached from a real
// request, and its HTTP translation would otherwise have no test at all.
type ContactMethodApplier interface {
	ApplyOperations(ctx context.Context, contactID uuid.UUID, ops []service.ContactMethodOperation) (*service.ApplyContactMethodsResult, error)
}

// ContactMethodHandler serves POST /contacts/:id/methods.
type ContactMethodHandler struct {
	service   ContactMethodApplier
	validator *validator.Validate
}

func NewContactMethodHandler(svc ContactMethodApplier) *ContactMethodHandler {
	return &ContactMethodHandler{service: svc, validator: sharedValidator}
}

// ApplyOperations applies contact-method operations transactionally
// @Summary Apply contact-method operations
// @Description Apply a batch of add/update/remove/set_primary/clear_primary operations to a contact's methods in one transaction. Absence expresses nothing: a method no operation names is never removed or altered.
// @Tags contacts
// @Accept json
// @Produce json
// @Param id path string true "Contact ID"
// @Param operations body ContactMethodOperationsRequest true "Operations to apply"
// @Success 200 {object} api.APIResponse{data=ContactMethodOperationsResponse}
// @Failure 400 {object} api.APIResponse{error=api.APIError} "Malformed, conflicting, or unsatisfiable operations"
// @Failure 404 {object} api.APIResponse{error=api.APIError} "Contact not found, or an operation names a method owned by another contact"
// @Failure 500 {object} api.APIResponse{error=api.APIError} "Internal server error"
// @Router /contacts/{id}/methods [post]
func (h *ContactMethodHandler) ApplyOperations(c *gin.Context) {
	contactID, ok := api.ParseUUIDParam(c, "id", "contact")
	if !ok {
		return
	}

	var req ContactMethodOperationsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		api.SendValidationError(c, "Invalid request body", err.Error())
		return
	}
	if err := h.validator.Struct(&req); err != nil {
		api.SendValidationError(c, "Invalid operations", err.Error())
		return
	}

	ops, err := buildContactMethodOperations(h.validator, req.Operations)
	if err != nil {
		api.SendValidationError(c, "Invalid operations", err.Error())
		return
	}

	result, err := h.service.ApplyOperations(c.Request.Context(), contactID, ops)
	if err != nil {
		switch {
		case errors.Is(err, service.ErrInvalidOperations),
			errors.Is(err, repository.ErrMethodValueConflict):
			// The conflict case is the database backstop surfacing as the same
			// deterministic 400 the fold would have produced, so a client
			// cannot tell which layer rejected it.
			api.SendValidationError(c, "Invalid operations", err.Error())
		case errors.Is(err, service.ErrMethodNotOwned):
			api.SendNotFound(c, "Contact method")
		case errors.Is(err, db.ErrNotFound):
			api.SendNotFound(c, "Contact")
		default:
			api.RespondInternal(c, err)
		}
		return
	}

	api.SendSuccess(c, http.StatusOK, contactMethodOperationsToResponse(result), nil)
}

// buildContactMethodOperations converts wire operations to service operations,
// enforcing the value FORMAT rules the create path uses.
//
// Blank handling deliberately differs from the create path and is enforced in
// the service: create drops a blank value, while an explicit add or update with
// a blank value is rejected. Dropping it here would turn an unsatisfiable
// intent into a successful-looking no-op.
func buildContactMethodOperations(validate *validator.Validate, ops []ContactMethodOperation) ([]service.ContactMethodOperation, error) {
	out := make([]service.ContactMethodOperation, len(ops))
	for i, op := range ops {
		converted := service.ContactMethodOperation{
			Op:        op.Op,
			Type:      op.Type,
			Value:     op.Value,
			IsPrimary: op.IsPrimary,
		}
		if op.MethodID != "" {
			id, err := uuid.Parse(op.MethodID)
			if err != nil {
				return nil, err
			}
			converted.MethodID = &id
		}
		// Format rules apply only where a value is actually being asserted.
		// A blank value is the service's call to reject, not a format failure.
		if (op.Op == service.MethodOpAdd || op.Op == service.MethodOpUpdate) && op.Value != "" {
			if err := validateContactMethodValueFormat(validate, op.Type, op.Value); err != nil {
				return nil, err
			}
		}
		out[i] = converted
	}
	return out, nil
}

func contactMethodOperationsToResponse(result *service.ApplyContactMethodsResult) ContactMethodOperationsResponse {
	resp := ContactMethodOperationsResponse{
		Methods: make([]ContactMethodResponse, len(result.Methods)),
		Results: make([]ContactMethodOperationResult, len(result.Results)),
	}
	for i, m := range result.Methods {
		resp.Methods[i] = contactMethodToResponse(m)
	}
	if result.RematchJobID != uuid.Nil {
		resp.RematchJobID = result.RematchJobID.String()
	}
	for i, r := range result.Results {
		entry := ContactMethodOperationResult{
			Index:    r.Index,
			Outcome:  r.Outcome,
			MethodID: r.MethodID.String(),
		}
		if r.Method != nil {
			snapshot := contactMethodToResponse(*r.Method)
			entry.Method = &snapshot
		}
		resp.Results[i] = entry
	}
	return resp
}

// validateContactMethodValueFormat applies the same per-type format rules the
// create path uses (handlers.validateContactMethods). Shared deliberately: only
// the blank-value handling differs between the two paths, and that difference
// lives in the service.
func validateContactMethodValueFormat(validate *validator.Validate, methodType, value string) error {
	switch methodType {
	case string(repository.ContactMethodEmail),
		string(repository.ContactMethodGChat):
		if err := validate.Var(value, "email"); err != nil {
			return fmt.Errorf("invalid email for contact method %s", methodType)
		}
	case string(repository.ContactMethodPhone),
		string(repository.ContactMethodSignal),
		string(repository.ContactMethodWhatsApp):
		if len(value) > 50 {
			return fmt.Errorf("contact method %s must be less than 50 characters", methodType)
		}
	}
	return nil
}
