package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds all application configuration
type Config struct {
	Database DatabaseConfig
	Server   ServerConfig
	Logger   LoggerConfig
	CORS     CORSConfig
	Features FeatureFlags
	Runtime  RuntimeConfig
	External ExternalConfig
	Google   GoogleConfig
	Todoist  TodoistConfig
	Watchdog WatchdogConfig
	Telegram TelegramConfig
	River    RiverConfig
	EventBus EventBusConfig
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
}

// EventBusConfig holds event-bus rollout phase flags. See
// .ai/spec/event-bus-foundation.md §3.9.
//
// InteractionMode gates the PR 5-8 phase lifecycle for the
// InteractionRecorder consumer:
//   - "off":     default. No shadow writes, no sibling publishes. The
//     InteractionRecorder river worker is still registered (so
//     river rejects no kinds at startup) but receives zero
//     events — pubBus is nil at publish sites.
//   - "shadow":  both paths run. Direct path is authoritative; consumer
//     observes and writes to event_shadow_observation. Post-
//     bake divergence query verifies 1:1 parity (PR 5).
//   - "cutover": consumer is sole writer. Only valid from PR 6
//     onwards — PR 5 rejects it as misconfiguration per
//     Decision 12 ("undefined behavior in PR 5").
type EventBusConfig struct {
	InteractionMode string // off | shadow | cutover. Default: off.
}

// EventBus interaction-mode constants. Mirrors (and must stay in sync
// with) consumer.InteractionMode*. Duplicated here to avoid a
// config-package import from the consumer package in validation / defaults.
const (
	EventBusInteractionModeOff     = "off"
	EventBusInteractionModeShadow  = "shadow"
	EventBusInteractionModeCutover = "cutover"
)

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
)

// Load reads configuration from environment variables
func Load() (*Config, error) {
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
		Telegram: TelegramConfig{
			BurstWindowHours:     getEnvAsInt("TELEGRAM_BURST_WINDOW_HOURS", 2),
			ReplyBridgeHours:     getEnvAsInt("TELEGRAM_REPLY_BRIDGE_HOURS", 48),
			BackfillSince:        getEnv("TELEGRAM_BACKFILL_SINCE", "2026-01-01"),
			DiscoveryMinMessages: getEnvAsInt("TELEGRAM_DISCOVERY_MIN_MESSAGES", 3),
			GroupMaxMembers:      getEnvAsInt("TELEGRAM_GROUP_MAX_MEMBERS", 10),
		},
		River: RiverConfig{
			WorkerConcurrency: getEnvAsInt("RIVER_WORKER_CONCURRENCY", DefaultRiverWorkerConcurrency),
		},
		EventBus: EventBusConfig{
			InteractionMode: getEnv("EVENT_BUS_INTERACTION_MODE", EventBusInteractionModeOff),
		},
	}

	// Validate configuration
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

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

	// EventBus interaction-mode validation. Must be one of the three phase
	// values (spec §3.9). Default "off" is applied in Load, so the empty
	// string should never reach Validate — but guard regardless for
	// test-constructed configs.
	switch c.EventBus.InteractionMode {
	case EventBusInteractionModeOff, EventBusInteractionModeShadow, EventBusInteractionModeCutover:
		// ok
	default:
		errors = append(errors, ValidationError{
			Field:   "EVENT_BUS_INTERACTION_MODE",
			Message: fmt.Sprintf("invalid mode %q; must be one of: off, shadow, cutover", c.EventBus.InteractionMode),
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
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}

// TestConfig creates a test configuration with sensible defaults for testing
func TestConfig() *Config {
	return &Config{
		Database: DatabaseConfig{
			URL:               "postgres://test:test@localhost:5432/test?sslmode=disable",
			MigrationsPath:    "../../migrations",
			HealthTimeout:     DefaultHealthCheckTimeout,
			MaxConns:          DefaultDBMaxConns,
			MinConns:          DefaultDBMinConns,
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
		Telegram: TelegramConfig{
			BurstWindowHours:     2,
			ReplyBridgeHours:     48,
			BackfillSince:        "2026-01-01",
			DiscoveryMinMessages: 3,
			GroupMaxMembers:      10,
		},
		River: RiverConfig{
			WorkerConcurrency: DefaultRiverWorkerConcurrency,
		},
		EventBus: EventBusConfig{
			InteractionMode: EventBusInteractionModeOff,
		},
	}
}
