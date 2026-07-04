package config

import (
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
)

// Config holds all application configuration
type Config struct {
	Database  DatabaseConfig
	Server    ServerConfig
	Logger    LoggerConfig
	CORS      CORSConfig
	Features  FeatureFlags
	Runtime   RuntimeConfig
	External  ExternalConfig
	Google    GoogleConfig
	Todoist   TodoistConfig
	Watchdog  WatchdogConfig
	Staleness StalenessConfig
	Telegram  TelegramConfig
	River     RiverConfig
	EventBus  EventBusConfig
	Health    HealthConfig
}

// DatabaseConfig holds database connection settings
type DatabaseConfig struct {
	URL            string        // Required
	MigrationsPath string        // Default: "migrations"
	HealthTimeout  time.Duration // Default: 5s
	// Connection pool settings (Pi-optimized defaults)
	MaxConns          int32         // Default: 5 (limit connections for Pi memory)
	MinConns          int32         // Default: 2 (keep connections warm)
	MaxConnIdleTime   time.Duration // Default: 5m (recycle idle faster)
	MaxConnLifetime   time.Duration // Default: 30m (limit connection age)
	HealthCheckPeriod time.Duration // Default: 30s (frequent health checks)
}

// ServerConfig holds HTTP server settings
type ServerConfig struct {
	Host            string        // Default: "127.0.0.1"
	Port            int           // Default: 8080
	ShutdownTimeout time.Duration // Default: 30s
}

// LoggerConfig holds logging configuration
type LoggerConfig struct {
	Level       string // Default: "info" (trace, debug, info, warn, error, fatal, panic)
	Environment string // production|development|staging|test (affects format)
}

// CORSConfig holds CORS middleware settings
type CORSConfig struct {
	AllowAll    bool   // Default: false
	FrontendURL string // Used when AllowAll=false
}

// FeatureFlags holds experimental feature toggles
type FeatureFlags struct {
	EnableVectorSearch   bool // Default: false
	EnableTelegramSync   bool // Default: false
	EnableCalendarSync   bool // Default: false
	EnableExternalSync   bool // Default: false
	EnableEventBusIngest bool // Default: false (gates POST /api/v1/ingest/events — spec §3.9)
}

// RuntimeConfig holds runtime-only settings (not validated at startup)
type RuntimeConfig struct {
	CRMEnvironment   string // production|staging|test|accelerated (affects cadence)
	TimeAcceleration int    // Default: 1 (no acceleration)
	TimeBase         string // RFC3339 timestamp for acceleration base
}

// ExternalConfig holds external service credentials
type ExternalConfig struct {
	SessionSecret      string // Required in production
	AnthropicAPIKey    string // Optional (future use)
	TelegramAPIID      int    // TELEGRAM_API_ID (from my.telegram.org/apps)
	TelegramAPIHash    string // TELEGRAM_API_HASH
	APIKey             string // Required in production (API authentication)
	BackupPath         string // Optional
	HomeServerHost     string // Optional
	HomeServerUser     string // Optional
	TokenEncryptionKey string // Required for OAuth token encryption (32-byte hex)
}

// GoogleConfig holds Google OAuth2 configuration
type GoogleConfig struct {
	ClientID     string // GOOGLE_CLIENT_ID
	ClientSecret string // GOOGLE_CLIENT_SECRET
	RedirectURL  string // GOOGLE_REDIRECT_URL (default: http://localhost:8080/api/v1/auth/google/callback)
}

// TodoistConfig holds Todoist OAuth2 configuration
type TodoistConfig struct {
	ClientID     string // TODOIST_CLIENT_ID
	ClientSecret string // TODOIST_CLIENT_SECRET
	RedirectURL  string // TODOIST_REDIRECT_URL (default: http://localhost:8080/api/v1/auth/todoist/callback)
}

// WatchdogConfig holds follow-up task watchdog window settings (days until follow-up is due)
type WatchdogConfig struct {
	WeeklyDays    int // Default: 3
	BiweeklyDays  int // Default: 5
	MonthlyDays   int // Default: 7
	QuarterlyDays int // Default: 14
	BiannualDays  int // Default: 21
	AnnualDays    int // Default: 21
}

// StalenessConfig holds sync-staleness watchdog thresholds. Distinct from
// WatchdogConfig (the follow-up-task watchdog); env prefix SYNC_STALENESS_*.
//
// Threshold knobs use Go duration strings. SourceOverrides carries per-source
// freshness thresholds keyed by source name; a hit overrides PullThreshold /
// PushThreshold for that source. A zero duration disables the corresponding
// check (or that source). Built-in override defaults are baked in by Load()
// and TestConfig() before env merges on top, so prod gets sane per-source
// values with zero env.
//
// Note on parse strictness: the duration/int knobs go through getEnvAsDuration
// / getEnvAsInt, which SILENTLY fall back to the default on a malformed value
// (existing repo behavior). Only SYNC_STALENESS_SOURCE_OVERRIDES — which has
// no existing helper — is strictly parsed and fails Load()/Validate() loud on
// a typo, since an operator tuning a single override should know it didn't
// take rather than silently revert.
type StalenessConfig struct {
	HeartbeatThreshold time.Duration            // default 15m; 0 disables heartbeat checks
	PullThreshold      time.Duration            // default 24h; 0 disables sync_stale checks
	PushThreshold      time.Duration            // default 48h; 0 disables push_stale checks
	ErrorMinCount      int                      // default 3; 0 disables sync_error checks
	ErrorThreshold     time.Duration            // default 6h; duration floor for sync_error (0 = count-only)
	SourceOverrides    map[string]time.Duration // per-source freshness threshold; 0 disables that source
}

// HealthConfig holds /health component thresholds. Same parse-strictness
// caveat as StalenessConfig: getEnvAsInt/getEnvAsDuration silently fall back to
// defaults on malformed values.
type HealthConfig struct {
	RiverDiscardedMax  int           // HEALTH_RIVER_DISCARDED_MAX, default 0: degraded when discarded count exceeds this
	RiverOldestDueMax  time.Duration // HEALTH_RIVER_OLDEST_DUE_MAX, default 30m; 0 disables the oldest-due age check
	SyncWatchdogMaxAge time.Duration // HEALTH_SYNC_WATCHDOG_MAX_AGE, default 30m; 0 disables the watchdog-freshness guard
	DiskPath           string        // HEALTH_DISK_PATH, default "/"
	DiskMinFreePercent int           // HEALTH_DISK_MIN_FREE_PERCENT, default 10; 0 disables the floor
}

// TelegramConfig holds Telegram integration tuning parameters
type TelegramConfig struct {
	BurstWindowHours     int    // Default: 2
	ReplyBridgeHours     int    // Default: 48
	BackfillSince        string // Default: "2026-01-01" (YYYY-MM-DD)
	DiscoveryMinMessages int    // Default: 3
	GroupMaxMembers      int    // Default: 10
}

// RiverConfig holds River worker-queue settings. See .ai/spec/event-bus-foundation.md §3.9.
type RiverConfig struct {
	WorkerConcurrency int // Default: 10 (Pi-optimized; river's own default is much higher)
	// JobTimeout is the per-job context deadline. River will cancel a
	// worker's context after this duration. Default 6m covers the existing
	// handler-layer 5m + 1m headroom (handlers/sync.go). Must be in
	// [1s, 1h] — lower is unsafe (full provider syncs exceed a second);
	// higher indicates a misbehaving provider that should be investigated.
	JobTimeout time.Duration
	// JobSampleRetentionDays is how many days of job_exec_sample rows the
	// job_sample_trim periodic worker keeps. Default 14 ("let it run a couple
	// of weeks"); validated 1–365.
	JobSampleRetentionDays int
}

// EventBusConfig holds the event-bus consumer mode flags. See
// .ai/spec/event-bus-foundation.md §3.9.
//
// InteractionMode controls the InteractionRecorder consumer:
//   - "off":     emergency override that disables publisher-driven
//     interaction recording entirely (telegram / calendar /
//     manual UI will NOT write interactions). The HTTP ingest
//     path remains functional. Unlike cadence / follow-up
//     modes, there is no UnsafeAllowOff gate on interaction
//     mode — publisher paths are not sole-writer-gated the
//     same way, so "off" is safe to flip without the gate.
//     Does NOT restore any pre-cutover direct path.
//   - "cutover": default. Consumer is the sole writer; publishers only
//     Publish events. This is the intended production mode.
//
// CadenceMode controls the CadenceUpdater consumer (spec §3.9).
//
//   - "off":     emergency override that disables the sole writer of
//     the four cadence columns entirely. Startup REFUSES to boot
//     on "off" unless UnsafeAllowOffMode is explicitly set (via
//     EVENT_BUS_CADENCE_UNSAFE_ALLOW_OFF=true). Does NOT restore any
//     pre-cutover direct path. Real rollback is `git revert`, not a
//     flag flip. See backend/internal/eventbus/README.md.
//   - "cutover": default. CadenceUpdater is the sole writer of the
//     four cadence columns; InteractionRecorder inline-applies on
//     fresh writes; queued deliveries are durable no-ops via the
//     event_consumer_claim table.
//
// UnsafeAllowOffMode is the explicit opt-in for running with
// CadenceMode="off" in non-test code. It corresponds to the env var
// EVENT_BUS_CADENCE_UNSAFE_ALLOW_OFF. config.TestConfig() sets it to
// true so unit/integration harnesses can exercise the off-mode branch
// without tripping the startup gate. When honored in a non-test config
// load path the caller is expected to log a WARN — see
// EventBusConfig.MaybeWarnUnsafeOff.
type EventBusConfig struct {
	InteractionMode        string // off | cutover. Default: cutover.
	CadenceMode            string // off | cutover. Default: cutover.
	FollowUpMode           string // off | cutover. Default: cutover.
	UnsafeAllowOffMode     bool   // EVENT_BUS_CADENCE_UNSAFE_ALLOW_OFF=true opt-in. Never true in normal prod.
	FollowUpUnsafeAllowOff bool   // EVENT_BUS_FOLLOWUP_UNSAFE_ALLOW_OFF=true opt-in. Never true in normal prod.
}

// EventBus interaction-mode constants. Mirrors (and must stay in sync
// with) consumer.InteractionMode*. Duplicated here to avoid a
// config-package import from the consumer package in validation / defaults.
const (
	EventBusInteractionModeOff     = "off"
	EventBusInteractionModeCutover = "cutover"
)

// EventBus cadence-mode constants. Mirrors (and must stay in sync with)
// consumer.CadenceMode*.
const (
	EventBusCadenceModeOff     = "off"
	EventBusCadenceModeCutover = "cutover"
)

// EventBus follow-up-mode constants. Mirrors (and must stay in sync
// with) consumer.FollowUpMode*. Post-cutover the direct path is gone,
// so FollowUpMode=off is a startup-time kill switch: turning the
// consumer off leaves nothing to create or complete follow-ups.
// Startup REFUSES to boot on "off" unless FollowUpUnsafeAllowOff is
// explicitly set (via EVENT_BUS_FOLLOWUP_UNSAFE_ALLOW_OFF=true).
// Real rollback is `git revert`, not a flag flip.
const (
	EventBusFollowUpModeOff     = "off"
	EventBusFollowUpModeCutover = "cutover"
)

// MaybeWarnUnsafeOff emits the mandated WARN log when the unsafe
// override is active AND cadence mode is actually "off". Safe to call
// from non-test config load paths (Load, reload hooks). No-op when the
// config is in a normal state. The WARN fields are a stable contract
// for operator-facing alerting; do not rename them without updating
// backend/internal/eventbus/README.md. Uses the zerolog package-level
// logger directly (not internal/logger) to avoid a circular import —
// internal/logger already depends on internal/config.
func (c *EventBusConfig) MaybeWarnUnsafeOff() {
	if c.UnsafeAllowOffMode && c.CadenceMode == EventBusCadenceModeOff {
		log.Warn().
			Str("event", "cadence_mode_off_unsafe_override_active").
			Str("cadence_mode", EventBusCadenceModeOff).
			Bool("unsafe_allow_off", true).
			Str("recovery", "unset EVENT_BUS_CADENCE_UNSAFE_ALLOW_OFF to restart in safe mode; git revert the cutover if you need the direct path back").
			Msg("EVENT_BUS_CADENCE_UNSAFE_ALLOW_OFF is active; CadenceUpdater is disabled and cadence columns WILL NOT be written")
	}
	if c.FollowUpUnsafeAllowOff && c.FollowUpMode == EventBusFollowUpModeOff {
		log.Warn().
			Str("event", "followup_mode_off_unsafe_override_active").
			Str("followup_mode", EventBusFollowUpModeOff).
			Bool("unsafe_allow_off", true).
			Str("recovery", "unset EVENT_BUS_FOLLOWUP_UNSAFE_ALLOW_OFF to restart in safe mode; git revert the FollowUpManager cutover to restore direct path").
			Msg("EVENT_BUS_FOLLOWUP_UNSAFE_ALLOW_OFF is active; FollowUpManager is disabled and follow-up tasks WILL NOT be created or completed")
	}
}

// ValidationError represents a configuration validation error
type ValidationError struct {
	Field   string
	Message string
}

func (e ValidationError) Error() string {
	return fmt.Sprintf("config validation failed for %s: %s", e.Field, e.Message)
}

// ValidationErrors represents multiple validation errors
type ValidationErrors []ValidationError

func (e ValidationErrors) Error() string {
	if len(e) == 0 {
		return "no validation errors"
	}
	var sb strings.Builder
	sb.WriteString("configuration validation failed:\n")
	for _, err := range e {
		fmt.Fprintf(&sb, "  - %s: %s\n", err.Field, err.Message)
	}
	return sb.String()
}

// Constants for default values
const (
	DefaultMigrationsPath     = "migrations"
	DefaultServerHost         = "127.0.0.1"
	DefaultServerPort         = 8080
	DefaultShutdownTimeout    = 30 * time.Second
	DefaultHealthCheckTimeout = 5 * time.Second
	DefaultLogLevel           = "info"
	DefaultEnvironment        = "development"
	DefaultCRMEnvironment     = "production"
	// Pi-optimized connection pool defaults. MaxConns must comfortably
	// cover (a) the river worker concurrency, (b) river's internal
	// leader/notifier/completer connections (~3), and (c) HTTP request
	// traffic. With RIVER_WORKER_CONCURRENCY=10 as the default, a pool
	// smaller than ~15 will starve either request handling or job
	// processing under load.
	DefaultDBMaxConns          = 15
	DefaultDBMinConns          = 2
	DefaultDBMaxConnIdleTime   = 5 * time.Minute
	DefaultDBMaxConnLifetime   = 30 * time.Minute
	DefaultDBHealthCheckPeriod = 30 * time.Second
	// River defaults (Pi-optimized; river's own default is much higher)
	DefaultRiverWorkerConcurrency = 10
	// DefaultRiverJobTimeout matches the pre-river handler's 5m sync
	// budget + 1m headroom. River cancels the worker's context when the
	// budget is exceeded.
	DefaultRiverJobTimeout = 6 * time.Minute
	// DefaultRiverJobSampleRetentionDays is the default retention window for
	// job_exec_sample rows (the River job-execution sampling table).
	DefaultRiverJobSampleRetentionDays = 14
	// Sync-staleness watchdog defaults. See the StalenessConfig doc
	// comment for the full rationale: 15m heartbeat (~15 missed ~60s
	// beats), 24h pull (5m–15m intervals for everything but gcontacts),
	// 48h push (daily-traffic messages for a single user), and a two-term
	// sync_error predicate (≥3 consecutive errors AND no success for ≥6h).
	DefaultStalenessHeartbeatThreshold = 15 * time.Minute
	DefaultStalenessPullThreshold      = 24 * time.Hour
	DefaultStalenessPushThreshold      = 48 * time.Hour
	DefaultStalenessErrorMinCount      = 3
	DefaultStalenessErrorThreshold     = 6 * time.Hour
	// /health component thresholds. See the HealthConfig doc comment:
	// any discarded job is immediately actionable (crm-admin --list-jobs /
	// --retry-job is the remediation tooling), 30m oldest-due /
	// watchdog-max-age = six missed 5m
	// periodic ticks (comfortably under River's 24h completed-job retention),
	// and 10% free is standard SD-card headroom.
	DefaultHealthRiverDiscardedMax  = 0
	DefaultHealthRiverOldestDueMax  = 30 * time.Minute
	DefaultHealthSyncWatchdogMaxAge = 30 * time.Minute
	DefaultHealthDiskPath           = "/"
	DefaultHealthDiskMinFreePercent = 10
)

// builtinStalenessSourceOverrides returns a fresh copy of the per-source
// freshness thresholds baked in before env overrides merge on top. These
// cover the full named push/pull surface so prod gets sane values with zero
// env. gcontacts is pulled (24h interval) — its threshold must comfortably
// exceed the interval; the rest are quiet-by-design push sources for a
// single user. A fresh map each call keeps callers from mutating a shared
// package-level value.
func builtinStalenessSourceOverrides() map[string]time.Duration {
	return map[string]time.Duration{
		"gcontacts":        72 * time.Hour,  // 24h sync interval — threshold must exceed it
		"phone_calls":      168 * time.Hour, // 7d — calls are legitimately rare; still beats the worst observed discovery lag
		"icloud_contacts":  336 * time.Hour, // 14d — contact edits are the rarest push trigger
		"anarlog_sessions": 168 * time.Hour, // 7d — meeting notes appear only when meetings happen
		"anarlog_humans":   336 * time.Hour, // 14d — human-file edits are contact-edit-rare
	}
}

// parseStalenessSourceOverrides parses a comma-separated source=duration
// string (e.g. "gcontacts=96h,phone_calls=240h") into a map, returning a
// descriptive error on the first malformed entry. Unlike getEnvAsDuration,
// this is strict: a typo fails Load() loud rather than silently reverting to
// the built-in default. An empty string yields a nil map (no overrides).
// Negative/zero durations are accepted here (0 disables a source); Validate()
// rejects negatives.
func parseStalenessSourceOverrides(raw string) (map[string]time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	out := make(map[string]time.Duration)
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		key, valStr, found := strings.Cut(entry, "=")
		key = strings.TrimSpace(key)
		valStr = strings.TrimSpace(valStr)
		if !found || key == "" || valStr == "" {
			return nil, fmt.Errorf("invalid SYNC_STALENESS_SOURCE_OVERRIDES entry %q; expected source=duration", entry)
		}
		d, err := time.ParseDuration(valStr)
		if err != nil {
			return nil, fmt.Errorf("invalid duration %q for source %q in SYNC_STALENESS_SOURCE_OVERRIDES: %w", valStr, key, err)
		}
		out[key] = d
	}
	return out, nil
}

// Load reads configuration from environment variables
func Load() (*Config, error) {
	// Parse the strict source-overrides string first so a typo fails loud
	// before the rest of config is assembled. Built-in defaults seed the
	// map; env entries merge on top (an env key with the same name wins).
	stalenessOverrides := builtinStalenessSourceOverrides()
	envOverrides, err := parseStalenessSourceOverrides(getEnv("SYNC_STALENESS_SOURCE_OVERRIDES", ""))
	if err != nil {
		return nil, err
	}
	for k, v := range envOverrides {
		stalenessOverrides[k] = v
	}

	cfg := &Config{
		Database: DatabaseConfig{
			URL:               getEnv("DATABASE_URL", ""),
			MigrationsPath:    getEnv("MIGRATIONS_PATH", DefaultMigrationsPath),
			HealthTimeout:     DefaultHealthCheckTimeout,
			MaxConns:          int32(getEnvAsInt("DB_MAX_CONNS", DefaultDBMaxConns)),
			MinConns:          int32(getEnvAsInt("DB_MIN_CONNS", DefaultDBMinConns)),
			MaxConnIdleTime:   getEnvAsDuration("DB_MAX_CONN_IDLE_TIME", DefaultDBMaxConnIdleTime),
			MaxConnLifetime:   getEnvAsDuration("DB_MAX_CONN_LIFETIME", DefaultDBMaxConnLifetime),
			HealthCheckPeriod: getEnvAsDuration("DB_HEALTH_CHECK_PERIOD", DefaultDBHealthCheckPeriod),
		},
		Server: ServerConfig{
			Host:            getEnv("HOST", DefaultServerHost),
			Port:            getEnvAsInt("PORT", DefaultServerPort),
			ShutdownTimeout: DefaultShutdownTimeout,
		},
		Logger: LoggerConfig{
			Level:       getEnv("LOG_LEVEL", DefaultLogLevel),
			Environment: getEnv("NODE_ENV", DefaultEnvironment),
		},
		CORS: CORSConfig{
			AllowAll:    getEnvAsBool("CORS_ALLOW_ALL", false),
			FrontendURL: getEnv("FRONTEND_URL", "http://localhost:3000"),
		},
		Features: FeatureFlags{
			EnableVectorSearch:   getEnvAsBool("ENABLE_VECTOR_SEARCH", false),
			EnableTelegramSync:   getEnvAsBool("ENABLE_TELEGRAM_SYNC", false),
			EnableCalendarSync:   getEnvAsBool("ENABLE_CALENDAR_SYNC", false),
			EnableExternalSync:   getEnvAsBool("ENABLE_EXTERNAL_SYNC", false),
			EnableEventBusIngest: getEnvAsBool("EVENT_BUS_INGEST_ENABLED", false),
		},
		Runtime: RuntimeConfig{
			CRMEnvironment:   getEnv("CRM_ENV", DefaultCRMEnvironment),
			TimeAcceleration: getEnvAsInt("TIME_ACCELERATION", 1),
			TimeBase:         getEnv("TIME_BASE", ""),
		},
		External: ExternalConfig{
			SessionSecret:      getEnv("SESSION_SECRET", ""),
			AnthropicAPIKey:    getEnv("ANTHROPIC_API_KEY", ""),
			TelegramAPIID:      getEnvAsInt("TELEGRAM_API_ID", 0),
			TelegramAPIHash:    getEnv("TELEGRAM_API_HASH", ""),
			APIKey:             getEnv("API_KEY", ""),
			BackupPath:         getEnv("BACKUP_PATH", ""),
			HomeServerHost:     getEnv("HOME_SERVER_HOST", ""),
			HomeServerUser:     getEnv("HOME_SERVER_USER", ""),
			TokenEncryptionKey: getEnv("TOKEN_ENCRYPTION_KEY", ""),
		},
		Google: GoogleConfig{
			ClientID:     getEnv("GOOGLE_CLIENT_ID", ""),
			ClientSecret: getEnv("GOOGLE_CLIENT_SECRET", ""),
			RedirectURL:  getEnv("GOOGLE_REDIRECT_URL", "http://localhost:8080/api/v1/auth/google/callback"),
		},
		Todoist: TodoistConfig{
			ClientID:     getEnv("TODOIST_CLIENT_ID", ""),
			ClientSecret: getEnv("TODOIST_CLIENT_SECRET", ""),
			RedirectURL:  getEnv("TODOIST_REDIRECT_URL", "http://localhost:8080/api/v1/auth/todoist/callback"),
		},
		Watchdog: WatchdogConfig{
			WeeklyDays:    getEnvAsInt("WATCHDOG_WEEKLY_DAYS", 3),
			BiweeklyDays:  getEnvAsInt("WATCHDOG_BIWEEKLY_DAYS", 5),
			MonthlyDays:   getEnvAsInt("WATCHDOG_MONTHLY_DAYS", 7),
			QuarterlyDays: getEnvAsInt("WATCHDOG_QUARTERLY_DAYS", 14),
			BiannualDays:  getEnvAsInt("WATCHDOG_BIANNUAL_DAYS", 21),
			AnnualDays:    getEnvAsInt("WATCHDOG_ANNUAL_DAYS", 21),
		},
		Staleness: StalenessConfig{
			HeartbeatThreshold: getEnvAsDuration("SYNC_STALENESS_HEARTBEAT_THRESHOLD", DefaultStalenessHeartbeatThreshold),
			PullThreshold:      getEnvAsDuration("SYNC_STALENESS_PULL_THRESHOLD", DefaultStalenessPullThreshold),
			PushThreshold:      getEnvAsDuration("SYNC_STALENESS_PUSH_THRESHOLD", DefaultStalenessPushThreshold),
			ErrorMinCount:      getEnvAsInt("SYNC_STALENESS_ERROR_MIN_COUNT", DefaultStalenessErrorMinCount),
			ErrorThreshold:     getEnvAsDuration("SYNC_STALENESS_ERROR_THRESHOLD", DefaultStalenessErrorThreshold),
			SourceOverrides:    stalenessOverrides,
		},
		Health: HealthConfig{
			RiverDiscardedMax:  getEnvAsInt("HEALTH_RIVER_DISCARDED_MAX", DefaultHealthRiverDiscardedMax),
			RiverOldestDueMax:  getEnvAsDuration("HEALTH_RIVER_OLDEST_DUE_MAX", DefaultHealthRiverOldestDueMax),
			SyncWatchdogMaxAge: getEnvAsDuration("HEALTH_SYNC_WATCHDOG_MAX_AGE", DefaultHealthSyncWatchdogMaxAge),
			DiskPath:           getEnv("HEALTH_DISK_PATH", DefaultHealthDiskPath),
			DiskMinFreePercent: getEnvAsInt("HEALTH_DISK_MIN_FREE_PERCENT", DefaultHealthDiskMinFreePercent),
		},
		Telegram: TelegramConfig{
			BurstWindowHours:     getEnvAsInt("TELEGRAM_BURST_WINDOW_HOURS", 2),
			ReplyBridgeHours:     getEnvAsInt("TELEGRAM_REPLY_BRIDGE_HOURS", 48),
			BackfillSince:        getEnv("TELEGRAM_BACKFILL_SINCE", "2026-01-01"),
			DiscoveryMinMessages: getEnvAsInt("TELEGRAM_DISCOVERY_MIN_MESSAGES", 3),
			GroupMaxMembers:      getEnvAsInt("TELEGRAM_GROUP_MAX_MEMBERS", 10),
		},
		River: RiverConfig{
			WorkerConcurrency:      getEnvAsInt("RIVER_WORKER_CONCURRENCY", DefaultRiverWorkerConcurrency),
			JobTimeout:             getEnvAsDuration("RIVER_JOB_TIMEOUT", DefaultRiverJobTimeout),
			JobSampleRetentionDays: getEnvAsInt("RIVER_JOB_SAMPLE_RETENTION_DAYS", DefaultRiverJobSampleRetentionDays),
		},
		EventBus: EventBusConfig{
			InteractionMode:        getEnv("EVENT_BUS_INTERACTION_MODE", EventBusInteractionModeCutover),
			CadenceMode:            getEnv("EVENT_BUS_CADENCE_MODE", EventBusCadenceModeCutover),
			FollowUpMode:           getEnv("EVENT_BUS_FOLLOWUP_MODE", EventBusFollowUpModeCutover),
			UnsafeAllowOffMode:     getEnvAsBool("EVENT_BUS_CADENCE_UNSAFE_ALLOW_OFF", false),
			FollowUpUnsafeAllowOff: getEnvAsBool("EVENT_BUS_FOLLOWUP_UNSAFE_ALLOW_OFF", false),
		},
	}

	// Validate configuration
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	// Non-test config load path: if the operator set the unsafe override
	// AND CadenceMode=off was honored, emit a loud WARN so repeated
	// config reloads keep surfacing the degraded state. The startup gate
	// in Validate already refuses boot without the override; this WARN
	// covers the "override honored" case. Pre-cutover TestConfig callers
	// don't land here because they don't call Load().
	cfg.EventBus.MaybeWarnUnsafeOff()

	return cfg, nil
}

// Validate checks configuration for errors
func (c *Config) Validate() error {
	var errors ValidationErrors

	// Required: DATABASE_URL
	if c.Database.URL == "" {
		errors = append(errors, ValidationError{
			Field:   "DATABASE_URL",
			Message: "database URL is required",
		})
	}

	// Server port range
	if c.Server.Port < 0 || c.Server.Port > 65535 {
		errors = append(errors, ValidationError{
			Field:   "PORT",
			Message: fmt.Sprintf("port must be between 0 and 65535, got %d", c.Server.Port),
		})
	}

	// Log level validation
	validLogLevels := []string{"trace", "debug", "info", "warn", "warning", "error", "fatal", "panic"}
	if !contains(validLogLevels, strings.ToLower(c.Logger.Level)) {
		errors = append(errors, ValidationError{
			Field:   "LOG_LEVEL",
			Message: fmt.Sprintf("invalid log level %q, must be one of: %v", c.Logger.Level, validLogLevels),
		})
	}

	// Environment validation
	validEnvs := []string{"production", "development", "staging", "test"}
	if !contains(validEnvs, c.Logger.Environment) {
		errors = append(errors, ValidationError{
			Field:   "NODE_ENV",
			Message: fmt.Sprintf("invalid environment %q, must be one of: %v", c.Logger.Environment, validEnvs),
		})
	}

	// CRM environment validation
	validCRMEnvs := []string{"production", "prod", "staging", "accelerated", "test", "testing"}
	if c.Runtime.CRMEnvironment != "" && !contains(validCRMEnvs, c.Runtime.CRMEnvironment) {
		errors = append(errors, ValidationError{
			Field:   "CRM_ENV",
			Message: fmt.Sprintf("invalid CRM environment %q, must be one of: %v", c.Runtime.CRMEnvironment, validCRMEnvs),
		})
	}

	// Dependency validation: SESSION_SECRET required in production
	if c.IsProduction() && c.External.SessionSecret == "" {
		errors = append(errors, ValidationError{
			Field:   "SESSION_SECRET",
			Message: "session secret is required in production",
		})
	}

	// Dependency validation: API_KEY required in production
	if c.IsProduction() && c.External.APIKey == "" {
		errors = append(errors, ValidationError{
			Field:   "API_KEY",
			Message: "API key is required in production for API authentication",
		})
	}

	// Dependency validation: Telegram API credentials required if feature enabled
	if c.Features.EnableTelegramSync {
		if c.External.TelegramAPIID == 0 {
			errors = append(errors, ValidationError{
				Field:   "TELEGRAM_API_ID",
				Message: "Telegram API ID is required when ENABLE_TELEGRAM_SYNC is true",
			})
		}
		if c.External.TelegramAPIHash == "" {
			errors = append(errors, ValidationError{
				Field:   "TELEGRAM_API_HASH",
				Message: "Telegram API hash is required when ENABLE_TELEGRAM_SYNC is true",
			})
		}
	}

	// CORS validation: FrontendURL should be set if not allowing all
	if !c.CORS.AllowAll && c.CORS.FrontendURL == "" {
		errors = append(errors, ValidationError{
			Field:   "FRONTEND_URL",
			Message: "frontend URL should be set when CORS_ALLOW_ALL is false",
		})
	}

	// River worker concurrency range
	if c.River.WorkerConcurrency <= 0 || c.River.WorkerConcurrency > 1000 {
		errors = append(errors, ValidationError{
			Field:   "RIVER_WORKER_CONCURRENCY",
			Message: fmt.Sprintf("must be between 1 and 1000, got %d", c.River.WorkerConcurrency),
		})
	}

	// River job timeout range. Zero means "use river's own default" but
	// we fill a sensible default in Load() so zero here always indicates
	// an operator misconfiguration.
	if c.River.JobTimeout < time.Second || c.River.JobTimeout > time.Hour {
		errors = append(errors, ValidationError{
			Field:   "RIVER_JOB_TIMEOUT",
			Message: fmt.Sprintf("must be between 1s and 1h, got %s", c.River.JobTimeout),
		})
	}

	// River job-sample retention range.
	if c.River.JobSampleRetentionDays < 1 || c.River.JobSampleRetentionDays > 365 {
		errors = append(errors, ValidationError{
			Field:   "RIVER_JOB_SAMPLE_RETENTION_DAYS",
			Message: fmt.Sprintf("must be between 1 and 365, got %d", c.River.JobSampleRetentionDays),
		})
	}

	// Sync-staleness watchdog validation. Negative durations are nonsense
	// (a zero value is the documented "disable this check" sentinel and is
	// allowed). ErrorMinCount must be >= 0. Override keys must be non-empty
	// and their durations non-negative.
	if c.Staleness.HeartbeatThreshold < 0 {
		errors = append(errors, ValidationError{
			Field:   "SYNC_STALENESS_HEARTBEAT_THRESHOLD",
			Message: fmt.Sprintf("must not be negative, got %s", c.Staleness.HeartbeatThreshold),
		})
	}
	if c.Staleness.PullThreshold < 0 {
		errors = append(errors, ValidationError{
			Field:   "SYNC_STALENESS_PULL_THRESHOLD",
			Message: fmt.Sprintf("must not be negative, got %s", c.Staleness.PullThreshold),
		})
	}
	if c.Staleness.PushThreshold < 0 {
		errors = append(errors, ValidationError{
			Field:   "SYNC_STALENESS_PUSH_THRESHOLD",
			Message: fmt.Sprintf("must not be negative, got %s", c.Staleness.PushThreshold),
		})
	}
	if c.Staleness.ErrorThreshold < 0 {
		errors = append(errors, ValidationError{
			Field:   "SYNC_STALENESS_ERROR_THRESHOLD",
			Message: fmt.Sprintf("must not be negative, got %s", c.Staleness.ErrorThreshold),
		})
	}
	if c.Staleness.ErrorMinCount < 0 {
		errors = append(errors, ValidationError{
			Field:   "SYNC_STALENESS_ERROR_MIN_COUNT",
			Message: fmt.Sprintf("must not be negative, got %d", c.Staleness.ErrorMinCount),
		})
	}
	for key, dur := range c.Staleness.SourceOverrides {
		if key == "" {
			errors = append(errors, ValidationError{
				Field:   "SYNC_STALENESS_SOURCE_OVERRIDES",
				Message: "override source keys must be non-empty",
			})
		}
		if dur < 0 {
			errors = append(errors, ValidationError{
				Field:   "SYNC_STALENESS_SOURCE_OVERRIDES",
				Message: fmt.Sprintf("override for source %q must not be negative, got %s", key, dur),
			})
		}
	}

	// /health component threshold validation. Zero values are the documented
	// "disable this check" sentinels and are allowed; negatives are nonsense.
	if c.Health.RiverDiscardedMax < 0 {
		errors = append(errors, ValidationError{
			Field:   "HEALTH_RIVER_DISCARDED_MAX",
			Message: fmt.Sprintf("must not be negative, got %d", c.Health.RiverDiscardedMax),
		})
	}
	if c.Health.RiverOldestDueMax < 0 {
		errors = append(errors, ValidationError{
			Field:   "HEALTH_RIVER_OLDEST_DUE_MAX",
			Message: fmt.Sprintf("must not be negative, got %s", c.Health.RiverOldestDueMax),
		})
	}
	if c.Health.SyncWatchdogMaxAge < 0 {
		errors = append(errors, ValidationError{
			Field:   "HEALTH_SYNC_WATCHDOG_MAX_AGE",
			Message: fmt.Sprintf("must not be negative, got %s", c.Health.SyncWatchdogMaxAge),
		})
	}
	if c.Health.DiskMinFreePercent < 0 || c.Health.DiskMinFreePercent > 100 {
		errors = append(errors, ValidationError{
			Field:   "HEALTH_DISK_MIN_FREE_PERCENT",
			Message: fmt.Sprintf("must be in [0,100], got %d", c.Health.DiskMinFreePercent),
		})
	}
	if c.Health.DiskPath == "" {
		errors = append(errors, ValidationError{
			Field:   "HEALTH_DISK_PATH",
			Message: "must not be empty",
		})
	}

	// EventBus interaction-mode validation. Default "cutover" is applied
	// in Load, so the empty string should never reach Validate — but
	// guard regardless for test-constructed configs.
	switch c.EventBus.InteractionMode {
	case EventBusInteractionModeOff, EventBusInteractionModeCutover:
		// ok
	default:
		errors = append(errors, ValidationError{
			Field:   "EVENT_BUS_INTERACTION_MODE",
			Message: fmt.Sprintf("invalid mode %q; must be one of: off, cutover", c.EventBus.InteractionMode),
		})
	}

	// EventBus cadence-mode validation. Default is "cutover". The "off"
	// value is a startup-time kill switch post-cutover — the direct path
	// is gone, so "off" disables the sole writer of cadence columns.
	// Require explicit opt-in via UnsafeAllowOffMode
	// (EVENT_BUS_CADENCE_UNSAFE_ALLOW_OFF=true) before accepting "off"
	// in non-test config. TestConfig() sets UnsafeAllowOffMode=true so
	// unit/integration harnesses can exercise the off branch without
	// tripping this gate.
	switch c.EventBus.CadenceMode {
	case EventBusCadenceModeCutover:
		// ok
	case EventBusCadenceModeOff:
		if !c.EventBus.UnsafeAllowOffMode {
			errors = append(errors, ValidationError{
				Field: "EVENT_BUS_CADENCE_MODE",
				Message: "cadence cutover has no runtime rollback: mode=off disables the " +
					"sole writer of the four cadence columns. Use `git revert` to roll " +
					"back the cutover, or set EVENT_BUS_CADENCE_UNSAFE_ALLOW_OFF=true " +
					"ONLY for emergency ops. This override leaves the system with " +
					"cadence writes disabled.",
			})
		}
	default:
		errors = append(errors, ValidationError{
			Field:   "EVENT_BUS_CADENCE_MODE",
			Message: fmt.Sprintf("invalid mode %q; must be one of: off, cutover", c.EventBus.CadenceMode),
		})
	}

	// EventBus follow-up-mode validation. Default is "cutover". Post-
	// cutover the direct path is gone, so "off" disables the sole
	// writer of follow-up tasks entirely. Require explicit opt-in via
	// FollowUpUnsafeAllowOff (EVENT_BUS_FOLLOWUP_UNSAFE_ALLOW_OFF=true)
	// before accepting "off" in non-test config.
	switch c.EventBus.FollowUpMode {
	case EventBusFollowUpModeCutover:
		// ok
	case EventBusFollowUpModeOff:
		if !c.EventBus.FollowUpUnsafeAllowOff {
			errors = append(errors, ValidationError{
				Field: "EVENT_BUS_FOLLOWUP_MODE",
				Message: "follow-up cutover has no runtime rollback: mode=off disables " +
					"the sole writer of follow-up tasks. Use `git revert` to roll back " +
					"the cutover, or set EVENT_BUS_FOLLOWUP_UNSAFE_ALLOW_OFF=true ONLY " +
					"for emergency ops. This override leaves the system with follow-up " +
					"creation and completion disabled.",
			})
		}
	default:
		errors = append(errors, ValidationError{
			Field:   "EVENT_BUS_FOLLOWUP_MODE",
			Message: fmt.Sprintf("invalid mode %q; must be one of: off, cutover", c.EventBus.FollowUpMode),
		})
	}

	// Cross-field sanity: River workers share the application pgxpool with
	// HTTP request handlers, and river.Client itself uses extra connections
	// for its leader/notifier/completer loops. Require that DB_MAX_CONNS
	// exceed RIVER_WORKER_CONCURRENCY by at least 3 — ~3 for river's
	// internals plus headroom for HTTP traffic. This catches the common
	// misconfiguration where a user raises concurrency without raising
	// the pool. Operators who know what they're doing can still set
	// DB_MAX_CONNS high enough to satisfy the check for any concurrency.
	const riverPoolHeadroom = 3
	if c.Database.MaxConns > 0 && c.River.WorkerConcurrency+riverPoolHeadroom > int(c.Database.MaxConns) {
		errors = append(errors, ValidationError{
			Field: "RIVER_WORKER_CONCURRENCY",
			Message: fmt.Sprintf(
				"river concurrency (%d) plus river/HTTP headroom (%d) must not exceed DB_MAX_CONNS (%d)",
				c.River.WorkerConcurrency, riverPoolHeadroom, c.Database.MaxConns,
			),
		})
	}

	if len(errors) > 0 {
		return errors
	}

	return nil
}

// IsProduction returns true if the environment is production
func (c *Config) IsProduction() bool {
	return c.Logger.Environment == "production"
}

// IsDevelopment returns true if the environment is development
func (c *Config) IsDevelopment() bool {
	return c.Logger.Environment == "development"
}

// IsProductionCRMEnv reports whether a CRM_ENV value is a production alias,
// matching the convention used by cadence.GetCadenceConfig and
// synthetic.SeedAllowed. Empty maps to production because config defaults an
// unset CRM_ENV to "production" at load; treating "" as prod keeps this
// consistent with cadence's switch and avoids a non-prod refusal on a
// misconfigured-empty value that the rest of the system already treats as prod.
//
// This is distinct from Config.IsProduction(), which keys off NODE_ENV
// (Logger.Environment), not CRM_ENV. Use this predicate for CRM_ENV-gated
// behavior such as the Todoist outbound-write guard.
func IsProductionCRMEnv(env string) bool {
	switch env {
	case "production", "prod", "":
		return true
	default:
		return false
	}
}

// GetBindAddress returns the server bind address in format "host:port"
func (c *Config) GetBindAddress() string {
	return fmt.Sprintf("%s:%d", c.Server.Host, c.Server.Port)
}

// Helper functions for parsing environment variables

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func getEnvAsInt(key string, defaultValue int) int {
	valueStr := os.Getenv(key)
	if valueStr == "" {
		return defaultValue
	}
	value, err := strconv.Atoi(valueStr)
	if err != nil {
		return defaultValue
	}
	return value
}

func getEnvAsBool(key string, defaultValue bool) bool {
	valueStr := os.Getenv(key)
	if valueStr == "" {
		return defaultValue
	}
	value, err := strconv.ParseBool(valueStr)
	if err != nil {
		return defaultValue
	}
	return value
}

func getEnvAsDuration(key string, defaultValue time.Duration) time.Duration {
	valueStr := os.Getenv(key)
	if valueStr == "" {
		return defaultValue
	}
	value, err := time.ParseDuration(valueStr)
	if err != nil {
		return defaultValue
	}
	return value
}

func contains(slice []string, item string) bool {
	return slices.Contains(slice, item)
}

// TestConfig creates a test configuration with sensible defaults for testing
func TestConfig() *Config {
	return &Config{
		Database: DatabaseConfig{
			URL:            "postgres://test:test@localhost:5432/test?sslmode=disable",
			MigrationsPath: "../../migrations",
			HealthTimeout:  DefaultHealthCheckTimeout,
			// Lowered for parallel integration tests: concurrent per-test pools
			// must not exhaust Postgres max_connections. MaxConns=8 is the floor
			// that still satisfies Validate()'s MaxConns >= WorkerConcurrency+3
			// after WorkerConcurrency drops to 4 below. Production Load() keeps
			// the DefaultDB* values.
			MaxConns:          8,
			MinConns:          1,
			MaxConnIdleTime:   DefaultDBMaxConnIdleTime,
			MaxConnLifetime:   DefaultDBMaxConnLifetime,
			HealthCheckPeriod: DefaultDBHealthCheckPeriod,
		},
		Server: ServerConfig{
			Host:            DefaultServerHost,
			Port:            0, // Random port for tests
			ShutdownTimeout: 5 * time.Second,
		},
		Logger: LoggerConfig{
			Level:       "debug",
			Environment: "test",
		},
		CORS: CORSConfig{
			AllowAll:    true,
			FrontendURL: "http://localhost:3000",
		},
		Features: FeatureFlags{
			EnableVectorSearch:   false,
			EnableTelegramSync:   false,
			EnableCalendarSync:   false,
			EnableExternalSync:   false,
			EnableEventBusIngest: false,
		},
		Runtime: RuntimeConfig{
			CRMEnvironment:   "test",
			TimeAcceleration: 1,
			TimeBase:         "",
		},
		External: ExternalConfig{
			SessionSecret:      "test-secret",
			TokenEncryptionKey: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		},
		Google: GoogleConfig{
			ClientID:     "test-client-id",
			ClientSecret: "test-client-secret",
			RedirectURL:  "http://localhost:8080/api/v1/auth/google/callback",
		},
		Todoist: TodoistConfig{
			ClientID:     "test-todoist-client-id",
			ClientSecret: "test-todoist-client-secret",
			RedirectURL:  "http://localhost:8080/api/v1/auth/todoist/callback",
		},
		Watchdog: WatchdogConfig{
			WeeklyDays:    3,
			BiweeklyDays:  5,
			MonthlyDays:   7,
			QuarterlyDays: 14,
			BiannualDays:  21,
			AnnualDays:    21,
		},
		Staleness: StalenessConfig{
			HeartbeatThreshold: DefaultStalenessHeartbeatThreshold,
			PullThreshold:      DefaultStalenessPullThreshold,
			PushThreshold:      DefaultStalenessPushThreshold,
			ErrorMinCount:      DefaultStalenessErrorMinCount,
			ErrorThreshold:     DefaultStalenessErrorThreshold,
			SourceOverrides:    builtinStalenessSourceOverrides(),
		},
		Health: HealthConfig{
			RiverDiscardedMax:  DefaultHealthRiverDiscardedMax,
			RiverOldestDueMax:  DefaultHealthRiverOldestDueMax,
			SyncWatchdogMaxAge: DefaultHealthSyncWatchdogMaxAge,
			DiskPath:           DefaultHealthDiskPath,
			DiskMinFreePercent: DefaultHealthDiskMinFreePercent,
		},
		Telegram: TelegramConfig{
			BurstWindowHours:     2,
			ReplyBridgeHours:     48,
			BackfillSince:        "2026-01-01",
			DiscoveryMinMessages: 3,
			GroupMaxMembers:      10,
		},
		River: RiverConfig{
			// Lowered alongside Database.MaxConns above so Validate()'s
			// MaxConns >= WorkerConcurrency+3 holds (4+3 <= 8). The River test
			// harnesses cap MaxWorkers at this value; 4 is plenty for tests.
			WorkerConcurrency:      4,
			JobTimeout:             DefaultRiverJobTimeout,
			JobSampleRetentionDays: DefaultRiverJobSampleRetentionDays,
		},
		EventBus: EventBusConfig{
			InteractionMode: EventBusInteractionModeCutover,
			// CadenceMode and FollowUpMode default to "off" in TestConfig so
			// unit tests don't inadvertently exercise the cutover writer
			// path; tests that need cutover explicitly override this.
			// UnsafeAllowOffMode + FollowUpUnsafeAllowOff are set so
			// Validate() doesn't reject "off" via the post-cutover startup
			// gates — those gates exist to catch prod misconfigs, not test
			// harnesses that deliberately opt into "off".
			CadenceMode:            EventBusCadenceModeOff,
			FollowUpMode:           EventBusFollowUpModeOff,
			UnsafeAllowOffMode:     true,
			FollowUpUnsafeAllowOff: true,
		},
	}
}
