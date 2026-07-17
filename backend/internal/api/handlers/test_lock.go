package handlers

import (
	"errors"
	"net/http"
	"time"

	"personal-crm/backend/internal/api"
	"personal-crm/backend/internal/service"

	"github.com/gin-gonic/gin"
)

// Test-only named-mutex endpoints (CRM_ENV=testing, wired by
// RegisterTestRoutes). The E2E suite uses them to serialize spec files that
// mutate DB-level singletons (e.g. mac_host): Playwright workers are
// separate OS processes, and this backend is the one arbiter they all
// already share. Leases expire unless renewed so a killed worker cannot
// deadlock the suite.

// AcquireLockInput is the POST /test/lock request body.
type AcquireLockInput struct {
	Name string `json:"name" validate:"required,max=128"`
	// How long to wait for the lock before giving up (bounded server-side).
	WaitMs int `json:"wait_ms" validate:"omitempty,min=0,max=600000"`
	// Lease TTL; the holder renews to keep it. Defaults to 30s.
	TTLMs int `json:"ttl_ms" validate:"omitempty,min=1000,max=600000"`
}

// AcquireLockResponse carries the lease the holder renews/releases with.
type AcquireLockResponse struct {
	LeaseID string `json:"lease_id"`
}

// RenewLockInput is the POST /test/lock/:lease/renew request body.
type RenewLockInput struct {
	TTLMs int `json:"ttl_ms" validate:"omitempty,min=1000,max=600000"`
}

const defaultLockTTL = 30 * time.Second

// AcquireLock blocks until the named lock is acquired or wait_ms elapses.
func (h *TestHandler) AcquireLock(c *gin.Context) {
	var input AcquireLockInput
	if err := c.ShouldBindJSON(&input); err != nil {
		api.SendError(c, http.StatusBadRequest, api.ErrCodeValidation, "Invalid request body", err.Error())
		return
	}
	if err := h.validator.Struct(input); err != nil {
		api.SendError(c, http.StatusBadRequest, api.ErrCodeValidation, "Validation failed", err.Error())
		return
	}

	ttl := defaultLockTTL
	if input.TTLMs > 0 {
		ttl = time.Duration(input.TTLMs) * time.Millisecond
	}
	lease, err := h.lockSvc.Acquire(c.Request.Context(), input.Name, ttl, time.Duration(input.WaitMs)*time.Millisecond)
	if err != nil {
		if errors.Is(err, service.ErrLockWaitTimeout) {
			api.SendError(c, http.StatusConflict, api.ErrCodeConflict, "Lock wait timeout", err.Error())
			return
		}
		api.RespondInternal(c, err)
		return
	}
	api.SendSuccess(c, http.StatusOK, AcquireLockResponse{LeaseID: lease}, nil)
}

// RenewLock extends a live lease. A lapsed/unknown lease is a 404: the
// holder must treat it as having lost the lock.
func (h *TestHandler) RenewLock(c *gin.Context) {
	var input RenewLockInput
	if err := c.ShouldBindJSON(&input); err != nil {
		api.SendError(c, http.StatusBadRequest, api.ErrCodeValidation, "Invalid request body", err.Error())
		return
	}
	if err := h.validator.Struct(input); err != nil {
		api.SendError(c, http.StatusBadRequest, api.ErrCodeValidation, "Validation failed", err.Error())
		return
	}
	ttl := defaultLockTTL
	if input.TTLMs > 0 {
		ttl = time.Duration(input.TTLMs) * time.Millisecond
	}
	if err := h.lockSvc.Renew(c.Param("lease"), ttl); err != nil {
		api.SendNotFound(c, "Lease")
		return
	}
	api.SendSuccess(c, http.StatusOK, gin.H{"renewed": true}, nil)
}

// ReleaseLock frees the lease's lock. Idempotent: releasing a lapsed or
// unknown lease succeeds without touching a takeover holder's lock.
func (h *TestHandler) ReleaseLock(c *gin.Context) {
	h.lockSvc.Release(c.Param("lease"))
	api.SendSuccess(c, http.StatusOK, gin.H{"released": true}, nil)
}
