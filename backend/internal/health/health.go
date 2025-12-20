package health

import (
	"context"
	"net/http"
	"runtime"
	"time"

	"personal-crm/backend/internal/accelerated"

	"github.com/gin-gonic/gin"
)

// Version info - set at build time or defaults
var (
	Version   = "dev"
	BuildTime = "unknown"
	GitCommit = "unknown"
)

// DatabaseChecker interface for checking database connectivity
type DatabaseChecker interface {
	HealthCheck(ctx context.Context) error
}

// HealthChecker handles health check requests
type HealthChecker struct {
	db        DatabaseChecker
	startTime time.Time
}

// NewHealthChecker creates a new health checker with database reference
func NewHealthChecker(db DatabaseChecker) *HealthChecker {
	return &HealthChecker{
		db:        db,
		startTime: accelerated.GetCurrentTime(),
	}
}

// ComponentStatus represents the status of a component
type ComponentStatus struct {
	Status       string  `json:"status"`
	ResponseTime *string `json:"response_time,omitempty"`
	Error        *string `json:"error,omitempty"`
}

// VersionInfo contains version information
type VersionInfo struct {
	Version   string `json:"version"`
	BuildTime string `json:"build_time"`
	GitCommit string `json:"git_commit"`
}

// SystemInfo contains system runtime information
type SystemInfo struct {
	Uptime       string `json:"uptime"`
	GoVersion    string `json:"go_version"`
	NumGoroutine int    `json:"num_goroutine"`
	MemoryAlloc  string `json:"memory_alloc_mb"`
}

// HealthResponse is the response for the health endpoint
type HealthResponse struct {
	Status     string                     `json:"status"`
	Timestamp  string                     `json:"timestamp"`
	Version    VersionInfo                `json:"version"`
	Components map[string]ComponentStatus `json:"components"`
	System     *SystemInfo                `json:"system,omitempty"`
}

// Handler handles the health check request
func (h *HealthChecker) Handler(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()

	now := accelerated.GetCurrentTime()
	overallStatus := "healthy"
	httpStatus := http.StatusOK

	// Check database
	dbStatus := h.checkDatabase(ctx)
	if dbStatus.Status != "healthy" {
		overallStatus = "degraded"
		httpStatus = http.StatusServiceUnavailable
	}

	// Build response
	response := HealthResponse{
		Status:    overallStatus,
		Timestamp: now.UTC().Format(time.RFC3339),
		Version: VersionInfo{
			Version:   Version,
			BuildTime: BuildTime,
			GitCommit: GitCommit,
		},
		Components: map[string]ComponentStatus{
			"database": dbStatus,
		},
	}

	// Include system info if requested or always (optional per requirements)
	// Including by default for observability
	response.System = h.getSystemInfo()

	c.JSON(httpStatus, response)
}

// checkDatabase checks database connectivity and returns status
func (h *HealthChecker) checkDatabase(ctx context.Context) ComponentStatus {
	if h.db == nil {
		errMsg := "database not configured"
		return ComponentStatus{
			Status: "unhealthy",
			Error:  &errMsg,
		}
	}

	start := accelerated.GetCurrentTime()
	err := h.db.HealthCheck(ctx)
	elapsed := accelerated.GetCurrentTime().Sub(start)
	responseTime := elapsed.String()

	if err != nil {
		errMsg := err.Error()
		return ComponentStatus{
			Status:       "unhealthy",
			ResponseTime: &responseTime,
			Error:        &errMsg,
		}
	}

	return ComponentStatus{
		Status:       "healthy",
		ResponseTime: &responseTime,
	}
}

// getSystemInfo returns system runtime information
func (h *HealthChecker) getSystemInfo() *SystemInfo {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	uptime := accelerated.GetCurrentTime().Sub(h.startTime)

	return &SystemInfo{
		Uptime:       uptime.Round(time.Second).String(),
		GoVersion:    runtime.Version(),
		NumGoroutine: runtime.NumGoroutine(),
		MemoryAlloc:  formatMemoryMB(m.Alloc),
	}
}

// formatMemoryMB formats bytes to megabytes string
func formatMemoryMB(bytes uint64) string {
	mb := float64(bytes) / 1024 / 1024
	return formatFloat(mb, 2)
}

// formatFloat formats a float with specified precision
func formatFloat(f float64, precision int) string {
	format := "%." + string(rune('0'+precision)) + "f"
	return sprintf(format, f)
}

// sprintf is a simple fmt.Sprintf without importing fmt
func sprintf(format string, a float64) string {
	// Simple implementation for our specific use case
	intPart := int(a)
	fracPart := int((a - float64(intPart)) * 100)
	if fracPart < 0 {
		fracPart = -fracPart
	}
	return itoa(intPart) + "." + padLeft(itoa(fracPart), 2, '0')
}

// itoa converts int to string without fmt package
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	negative := i < 0
	if negative {
		i = -i
	}
	var digits []byte
	for i > 0 {
		digits = append([]byte{byte('0' + i%10)}, digits...)
		i /= 10
	}
	if negative {
		digits = append([]byte{'-'}, digits...)
	}
	return string(digits)
}

// padLeft pads string with char on left to reach length
func padLeft(s string, length int, char byte) string {
	for len(s) < length {
		s = string(char) + s
	}
	return s
}

// HealthHandler is a legacy handler for backward compatibility (without DB check)
// Deprecated: Use NewHealthChecker().Handler instead
func HealthHandler(c *gin.Context) {
	response := HealthResponse{
		Status:    "ok",
		Timestamp: accelerated.GetCurrentTime().UTC().Format(time.RFC3339),
		Version: VersionInfo{
			Version:   Version,
			BuildTime: BuildTime,
			GitCommit: GitCommit,
		},
		Components: map[string]ComponentStatus{},
	}
	c.JSON(http.StatusOK, response)
}
