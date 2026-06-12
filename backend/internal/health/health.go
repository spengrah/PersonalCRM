package health

import (
	"context"
	"math"
	"net/http"
	"runtime"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/repository"

	"github.com/gin-gonic/gin"
)

// Component status vocabulary. database keeps only healthy/unhealthy; the
// river/sync/disk components add degraded (threshold breached) and unknown
// (skipped / not configured / stale data). Readiness aggregation treats
// anything != statusHealthy as not-ready.
const (
	statusHealthy   = "healthy"
	statusDegraded  = "degraded"
	statusUnhealthy = "unhealthy"
	statusUnknown   = "unknown"
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

// RiverStatsReader reads the river_job state behind the river + sync
// components. Satisfied by *repository.HealthRepository. A nil reader makes the
// river component report "unknown"/"not configured".
type RiverStatsReader interface {
	CountDiscardedRiverJobs(ctx context.Context) (int64, error)
	OldestDueRiverJobScheduledAt(ctx context.Context, now time.Time) (*time.Time, error)
	LatestCompletedRiverJobByKind(ctx context.Context, kind string) (*time.Time, error)
}

// StalenessReader exposes the currently-open sync-staleness breaches. Satisfied
// by *service.StalenessService (same shape as handlers.StalenessReader). A nil
// reader makes the sync component report "unknown"/"not configured".
type StalenessReader interface {
	ListActiveBreaches(ctx context.Context) ([]repository.StalenessBreach, error)
}

// Thresholds holds the component thresholds the health package evaluates, so it
// never imports internal/config. Populated from config.HealthConfig in main.go.
type Thresholds struct {
	RiverDiscardedMax  int
	RiverOldestDueMax  time.Duration
	SyncWatchdogMaxAge time.Duration
	DiskPath           string
	DiskMinFreePercent int
}

// Deps carries the optional collaborators for the river/sync/disk components.
// A zero-value Deps is safe: nil River/Staleness → that component reports
// "unknown", nil DiskUsage → the default statfs implementation, empty
// SyncWatchdogKind → the watchdog-freshness guard is skipped.
type Deps struct {
	River            RiverStatsReader
	Staleness        StalenessReader
	SyncWatchdogKind string
	DiskUsage        DiskUsageFunc
	Thresholds       Thresholds
}

// HealthChecker handles health check requests
type HealthChecker struct {
	db        DatabaseChecker
	timeout   time.Duration
	startTime time.Time
	deps      Deps
}

// NewHealthChecker creates a new health checker with database reference,
// timeout, and the optional river/sync/disk component deps.
func NewHealthChecker(db DatabaseChecker, timeout time.Duration, deps Deps) *HealthChecker {
	if deps.DiskUsage == nil {
		deps.DiskUsage = defaultDiskUsage
	}
	return &HealthChecker{
		db:        db,
		timeout:   timeout,
		startTime: accelerated.GetCurrentTime(),
		deps:      deps,
	}
}

// ComponentStatus represents the status of a component. Details is appended
// LAST so the database component's serialization stays byte-identical (it
// never sets Details, and omitempty drops the key).
type ComponentStatus struct {
	Status       string  `json:"status"`
	ResponseTime *string `json:"response_time,omitempty"`
	Error        *string `json:"error,omitempty"`
	Details      any     `json:"details,omitempty"`
}

// RiverDetails is the river component's payload. OldestDueAgeSeconds is a
// pointer so "no due jobs" (field absent) is distinguishable from "a job due
// exactly now" (0). Counts and ages only — nothing from job args ever
// serializes (args can reference contact UUIDs), keeping the unauthenticated
// payload PII-free by construction.
type RiverDetails struct {
	DiscardedCount      int64  `json:"discarded_count"`
	OldestDueAgeSeconds *int64 `json:"oldest_due_age_seconds,omitempty"`
}

// SyncDetails is the sync component's payload. Only a count and a max age — the
// breach AccountID (can be a Google account email), Details (can embed provider
// error text), Source, and breach types NEVER serialize into /health (full rows
// stay on the authenticated GET /api/v1/sync/staleness). MaxStalenessSeconds is
// omitted when the count is 0.
type SyncDetails struct {
	ActiveBreachCount   int    `json:"active_breach_count"`
	MaxStalenessSeconds *int64 `json:"max_staleness_seconds,omitempty"`
}

// DiskDetails is the disk component's payload. The path is operator config, not
// PII; free_percent is rounded to one decimal.
type DiskDetails struct {
	Path        string  `json:"path"`
	FreePercent float64 `json:"free_percent"`
	FreeBytes   uint64  `json:"free_bytes"`
	TotalBytes  uint64  `json:"total_bytes"`
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

// HealthResponse is the response for the health endpoint. Probe self-documents
// which contract a body represents ("liveness" on bare /health, "readiness" on
// ?ready=1); omitempty keeps the legacy HealthHandler output byte-identical.
type HealthResponse struct {
	Status     string                     `json:"status"`
	Probe      string                     `json:"probe,omitempty"`
	Timestamp  string                     `json:"timestamp"`
	Version    VersionInfo                `json:"version"`
	Components map[string]ComponentStatus `json:"components"`
	System     *SystemInfo                `json:"system,omitempty"`
}

// Handler handles the health check request.
//
// Liveness/readiness split on one route via ?ready=1 (accept "1" or "true";
// anything else → liveness):
//   - Bare /health is the LIVENESS probe — top-level status + HTTP code are
//     driven by the database component ALONE (healthy/200 when the DB ping
//     succeeds, degraded/503 when it fails), keeping today's contract
//     bit-for-bit for the Quadlet boot gate and deploy health_gate. The
//     river/sync/disk components appear in components with their own statuses
//     but are informational here.
//   - ?ready=1 is the READINESS probe — top-level status aggregates ALL
//     components (healthy only when every component is healthy, else degraded)
//     and the HTTP code is 503 whenever any component is non-healthy.
func (h *HealthChecker) Handler(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), h.timeout)
	defer cancel()

	now := accelerated.GetCurrentTime()
	ready := isReadinessProbe(c.Query("ready"))

	// One shared budget bounds the DB ping + the three river/watchdog queries +
	// the sync read, run sequentially — worst-case endpoint latency is
	// unchanged from today (the DB ping alone could already consume the full
	// timeout).
	dbStatus := h.checkDatabase(ctx)
	dbHealthy := dbStatus.Status == statusHealthy

	components := map[string]ComponentStatus{
		"database": dbStatus,
	}

	// Skip-on-DB-down: piling more queries onto a dead pool would add latency
	// exactly when the boot gate (which polls every 1s while the DB is down)
	// needs the fastest possible answer. River + sync are skipped; disk always
	// runs (local syscall, and most relevant right when the DB just died on a
	// full SD card).
	if dbHealthy {
		components["river"] = h.checkRiver(ctx, now)
		components["sync"] = h.checkSync(ctx, now)
	} else {
		skipped := "skipped: database unhealthy"
		components["river"] = ComponentStatus{Status: statusUnknown, Error: &skipped}
		components["sync"] = ComponentStatus{Status: statusUnknown, Error: &skipped}
	}
	components["disk"] = h.checkDisk()

	// Liveness top-level: database alone. Readiness top-level: aggregate.
	overallStatus := statusHealthy
	httpStatus := http.StatusOK
	if ready {
		for _, comp := range components {
			if comp.Status != statusHealthy {
				overallStatus = statusDegraded
				httpStatus = http.StatusServiceUnavailable
				break
			}
		}
	} else if !dbHealthy {
		overallStatus = statusDegraded
		httpStatus = http.StatusServiceUnavailable
	}

	probe := "liveness"
	if ready {
		probe = "readiness"
	}

	response := HealthResponse{
		Status:    overallStatus,
		Probe:     probe,
		Timestamp: now.UTC().Format(time.RFC3339),
		Version: VersionInfo{
			Version:   Version,
			BuildTime: BuildTime,
			GitCommit: GitCommit,
		},
		Components: components,
	}

	// Include system info by default for observability.
	response.System = h.getSystemInfo()

	c.JSON(httpStatus, response)
}

// isReadinessProbe reports whether the ?ready query value selects the readiness
// contract. Only "1" and "true" qualify; anything else (including empty) is
// liveness.
func isReadinessProbe(raw string) bool {
	return raw == "1" || raw == "true"
}

// checkDatabase checks database connectivity and returns status
func (h *HealthChecker) checkDatabase(ctx context.Context) ComponentStatus {
	if h.db == nil {
		errMsg := "database not configured"
		return ComponentStatus{
			Status: statusUnhealthy,
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
			Status:       statusUnhealthy,
			ResponseTime: &responseTime,
			Error:        &errMsg,
		}
	}

	return ComponentStatus{
		Status:       statusHealthy,
		ResponseTime: &responseTime,
	}
}

// checkRiver reports on the River worker queue: degraded when discarded jobs
// exceed the tolerance OR the oldest due-but-unpicked job is older than the
// age threshold. Catches permanently-failed jobs (discarded; remediation is
// crm-admin --retry-job) and a stalled/starved worker pool.
//
// Accelerated-time caveat (dev only): the age compares accelerated time against
// the wall-clock river_job.scheduled_at, so under TIME_ACCELERATION the age
// inflates and ready-mode may show river degraded. Prod never sets
// acceleration, and the bare liveness route (gates/Playwright) is unaffected.
func (h *HealthChecker) checkRiver(ctx context.Context, now time.Time) ComponentStatus {
	if h.deps.River == nil {
		msg := "river stats not configured"
		return ComponentStatus{Status: statusUnknown, Error: &msg}
	}

	discarded, err := h.deps.River.CountDiscardedRiverJobs(ctx)
	if err != nil {
		return unhealthyComponent(err)
	}
	oldestDue, err := h.deps.River.OldestDueRiverJobScheduledAt(ctx, now)
	if err != nil {
		return unhealthyComponent(err)
	}

	details := RiverDetails{DiscardedCount: discarded}
	status := statusHealthy
	if discarded > int64(h.deps.Thresholds.RiverDiscardedMax) {
		status = statusDegraded
	}
	if oldestDue != nil {
		age := now.Sub(*oldestDue)
		if age < 0 {
			age = 0
		}
		// Compare at whole-second granularity (the same unit the payload
		// reports) so the strictly-greater boundary is deterministic — a raw
		// time.Duration comparison would degrade at threshold+1ns.
		ageSeconds := int64(age.Seconds())
		details.OldestDueAgeSeconds = &ageSeconds
		maxSeconds := int64(h.deps.Thresholds.RiverOldestDueMax.Seconds())
		if maxSeconds > 0 && ageSeconds > maxSeconds {
			status = statusDegraded
		}
	}

	return ComponentStatus{Status: status, Details: details}
}

// checkSync reports on per-provider sync staleness by consuming the staleness
// watchdog's breach table (one indexed read, ≤5m detection lag). Before
// trusting that table it verifies the watchdog's own completion trail: the
// breach data is only fresh while the watchdog (itself a River periodic job)
// runs, so a stale/absent completion trail makes this component "unknown"
// regardless of the breach count. Readiness treats unknown as not-ready, so
// total River-client death AND a persistently-failing watchdog worker both flip
// ?ready=1 to 503 within SyncWatchdogMaxAge.
//
// Accelerated-time caveat (dev only): the watchdog-freshness guard compares
// accelerated time against the wall-clock finalized_at, so under
// TIME_ACCELERATION the trail can read stale and the component may show
// unknown. Prod never sets acceleration; the bare liveness route is unaffected.
func (h *HealthChecker) checkSync(ctx context.Context, now time.Time) ComponentStatus {
	if h.deps.Staleness == nil {
		msg := "staleness reader not configured"
		return ComponentStatus{Status: statusUnknown, Error: &msg}
	}

	breaches, err := h.deps.Staleness.ListActiveBreaches(ctx)
	if err != nil {
		return unhealthyComponent(err)
	}

	details := SyncDetails{ActiveBreachCount: len(breaches)}
	if len(breaches) > 0 {
		var maxStale int64
		for _, b := range breaches {
			age := int64(now.Sub(b.StaleSince).Seconds())
			if age < 0 {
				age = 0
			}
			if age > maxStale {
				maxStale = age
			}
		}
		details.MaxStalenessSeconds = &maxStale
	}

	// Watchdog-freshness guard. Stale/absent trail takes precedence over the
	// breach status — stale data must not assert health. Skipped when disabled
	// (zero max-age or empty kind), matching the nil-guard effect.
	if h.deps.River != nil && h.deps.SyncWatchdogKind != "" && h.deps.Thresholds.SyncWatchdogMaxAge > 0 {
		latest, lerr := h.deps.River.LatestCompletedRiverJobByKind(ctx, h.deps.SyncWatchdogKind)
		if lerr != nil {
			return unhealthyComponent(lerr)
		}
		stale := latest == nil || now.Sub(*latest) > h.deps.Thresholds.SyncWatchdogMaxAge
		if stale {
			msg := "staleness watchdog not running"
			return ComponentStatus{Status: statusUnknown, Error: &msg, Details: details}
		}
	}

	status := statusHealthy
	if len(breaches) > 0 {
		status = statusDegraded
	}
	return ComponentStatus{Status: status, Details: details}
}

// checkDisk statfs's the configured path and reports degraded when free space
// drops below the floor. Local syscall — no ctx/timeout needed; always runs
// even when the DB is down (an SD card at 100% is exactly when the DB dies).
func (h *HealthChecker) checkDisk() ComponentStatus {
	total, avail, err := h.deps.DiskUsage(h.deps.Thresholds.DiskPath)
	if err != nil {
		return unhealthyComponent(err)
	}

	freePercent := 0.0
	if total > 0 {
		freePercent = math.Round(float64(avail)/float64(total)*1000) / 10
	}
	details := DiskDetails{
		Path:        h.deps.Thresholds.DiskPath,
		FreePercent: freePercent,
		FreeBytes:   avail,
		TotalBytes:  total,
	}

	status := statusHealthy
	if h.deps.Thresholds.DiskMinFreePercent > 0 && freePercent < float64(h.deps.Thresholds.DiskMinFreePercent) {
		status = statusDegraded
	}
	return ComponentStatus{Status: status, Details: details}
}

// unhealthyComponent wraps a check error as an unhealthy component. The error
// string can carry infrastructure detail (pg connection errors) — the same
// exposure class the database component has always had; breach/account/job-args
// content is excluded by construction (the detail payloads carry counts + ages
// only), so this never leaks PII.
func unhealthyComponent(err error) ComponentStatus {
	msg := err.Error()
	return ComponentStatus{Status: statusUnhealthy, Error: &msg}
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
