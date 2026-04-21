package config

import (
	"os"
	"strings"
	"testing"
	"time"
)

// WithEnv is a test helper that sets environment variables for the duration of a test
func WithEnv(t *testing.T, key, value string) {
	t.Helper()
	original := os.Getenv(key)
	if err := os.Setenv(key, value); err != nil {
		t.Fatalf("Failed to set environment variable %s: %v", key, err)
	}
	t.Cleanup(func() {
		if original == "" {
			if err := os.Unsetenv(key); err != nil {
				t.Errorf("Failed to unset environment variable %s: %v", key, err)
			}
		} else {
			if err := os.Setenv(key, original); err != nil {
				t.Errorf("Failed to restore environment variable %s: %v", key, err)
			}
		}
	})
}

func TestConfig_Load_ValidConfig(t *testing.T) {
	// Set all required env vars
	WithEnv(t, "DATABASE_URL", "postgres://localhost/test")
	WithEnv(t, "NODE_ENV", "development")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}

	if cfg.Database.URL != "postgres://localhost/test" {
		t.Errorf("Expected DATABASE_URL=postgres://localhost/test, got %s", cfg.Database.URL)
	}

	if cfg.Logger.Environment != "development" {
		t.Errorf("Expected NODE_ENV=development, got %s", cfg.Logger.Environment)
	}
}

func TestConfig_Load_Defaults(t *testing.T) {
	// Only set required field
	WithEnv(t, "DATABASE_URL", "postgres://localhost/test")
	WithEnv(t, "NODE_ENV", "development")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}

	// Check defaults
	if cfg.Database.MigrationsPath != DefaultMigrationsPath {
		t.Errorf("Expected default migrations path %q, got %q", DefaultMigrationsPath, cfg.Database.MigrationsPath)
	}

	if cfg.Server.Host != DefaultServerHost {
		t.Errorf("Expected default server host %q, got %q", DefaultServerHost, cfg.Server.Host)
	}

	if cfg.Server.Port != DefaultServerPort {
		t.Errorf("Expected default server port %d, got %d", DefaultServerPort, cfg.Server.Port)
	}

	if cfg.Logger.Level != DefaultLogLevel {
		t.Errorf("Expected default log level %q, got %q", DefaultLogLevel, cfg.Logger.Level)
	}

	if cfg.Runtime.CRMEnvironment != DefaultCRMEnvironment {
		t.Errorf("Expected default CRM environment %q, got %q", DefaultCRMEnvironment, cfg.Runtime.CRMEnvironment)
	}
}

func TestConfig_Validate_MissingDatabaseURL(t *testing.T) {
	WithEnv(t, "NODE_ENV", "development")
	// Don't set DATABASE_URL

	_, err := Load()
	if err == nil {
		t.Fatal("Expected error when DATABASE_URL is missing")
	}

	if verr, ok := err.(ValidationErrors); ok {
		found := false
		for _, e := range verr {
			if e.Field == "DATABASE_URL" {
				found = true
				break
			}
		}
		if !found {
			t.Error("Expected validation error for DATABASE_URL")
		}
	} else {
		t.Errorf("Expected ValidationErrors, got %T", err)
	}
}

func TestConfig_Validate_InvalidPort(t *testing.T) {
	WithEnv(t, "DATABASE_URL", "postgres://localhost/test")
	WithEnv(t, "PORT", "99999")
	WithEnv(t, "NODE_ENV", "development")

	_, err := Load()
	if err == nil {
		t.Fatal("Expected error for invalid port")
	}

	if verr, ok := err.(ValidationErrors); ok {
		found := false
		for _, e := range verr {
			if e.Field == "PORT" {
				found = true
				break
			}
		}
		if !found {
			t.Error("Expected validation error for PORT")
		}
	}
}

func TestConfig_Validate_InvalidLogLevel(t *testing.T) {
	WithEnv(t, "DATABASE_URL", "postgres://localhost/test")
	WithEnv(t, "LOG_LEVEL", "invalid")
	WithEnv(t, "NODE_ENV", "development")

	_, err := Load()
	if err == nil {
		t.Fatal("Expected error for invalid log level")
	}

	if verr, ok := err.(ValidationErrors); ok {
		found := false
		for _, e := range verr {
			if e.Field == "LOG_LEVEL" {
				found = true
				break
			}
		}
		if !found {
			t.Error("Expected validation error for LOG_LEVEL")
		}
	}
}

func TestConfig_Validate_ProductionRequiresSessionSecret(t *testing.T) {
	WithEnv(t, "DATABASE_URL", "postgres://localhost/test")
	WithEnv(t, "NODE_ENV", "production")
	// Don't set SESSION_SECRET

	_, err := Load()
	if err == nil {
		t.Fatal("Expected error when SESSION_SECRET is missing in production")
	}

	if verr, ok := err.(ValidationErrors); ok {
		found := false
		for _, e := range verr {
			if e.Field == "SESSION_SECRET" {
				found = true
				break
			}
		}
		if !found {
			t.Error("Expected validation error for SESSION_SECRET")
		}
	}
}

func TestConfig_Validate_TelegramDependency(t *testing.T) {
	WithEnv(t, "DATABASE_URL", "postgres://localhost/test")
	WithEnv(t, "NODE_ENV", "development")
	WithEnv(t, "ENABLE_TELEGRAM_SYNC", "true")
	// Don't set TELEGRAM_API_ID or TELEGRAM_API_HASH

	_, err := Load()
	if err == nil {
		t.Fatal("Expected error when TELEGRAM_API_ID/TELEGRAM_API_HASH are missing but ENABLE_TELEGRAM_SYNC is true")
	}

	if verr, ok := err.(ValidationErrors); ok {
		foundID := false
		foundHash := false
		for _, e := range verr {
			if e.Field == "TELEGRAM_API_ID" {
				foundID = true
			}
			if e.Field == "TELEGRAM_API_HASH" {
				foundHash = true
			}
		}
		if !foundID {
			t.Error("Expected validation error for TELEGRAM_API_ID")
		}
		if !foundHash {
			t.Error("Expected validation error for TELEGRAM_API_HASH")
		}
	}
}

func TestConfig_TypeConversions(t *testing.T) {
	WithEnv(t, "DATABASE_URL", "postgres://localhost/test")
	WithEnv(t, "NODE_ENV", "development")
	WithEnv(t, "PORT", "3000")
	WithEnv(t, "CORS_ALLOW_ALL", "true")
	WithEnv(t, "ENABLE_VECTOR_SEARCH", "true")
	WithEnv(t, "TIME_ACCELERATION", "10")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}

	// Test int conversion
	if cfg.Server.Port != 3000 {
		t.Errorf("Expected PORT=3000 (int), got %d", cfg.Server.Port)
	}

	// Test bool conversions
	if !cfg.CORS.AllowAll {
		t.Error("Expected CORS_ALLOW_ALL=true (bool), got false")
	}

	if !cfg.Features.EnableVectorSearch {
		t.Error("Expected ENABLE_VECTOR_SEARCH=true (bool), got false")
	}

	if cfg.Runtime.TimeAcceleration != 10 {
		t.Errorf("Expected TIME_ACCELERATION=10 (int), got %d", cfg.Runtime.TimeAcceleration)
	}
}

// TestConfig_EventBusIngestFlag ensures EVENT_BUS_INGEST_ENABLED wires into
// Features.EnableEventBusIngest (default false, env override true). The
// ingest route registration in main.go reads this flag — spec §3.9.
func TestConfig_EventBusIngestFlag(t *testing.T) {
	t.Run("DefaultFalse", func(t *testing.T) {
		WithEnv(t, "DATABASE_URL", "postgres://localhost/test")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load() failed: %v", err)
		}
		if cfg.Features.EnableEventBusIngest {
			t.Error("Expected EnableEventBusIngest=false by default")
		}
	})

	t.Run("EnvOverrideTrue", func(t *testing.T) {
		WithEnv(t, "DATABASE_URL", "postgres://localhost/test")
		WithEnv(t, "EVENT_BUS_INGEST_ENABLED", "true")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load() failed: %v", err)
		}
		if !cfg.Features.EnableEventBusIngest {
			t.Error("Expected EnableEventBusIngest=true when EVENT_BUS_INGEST_ENABLED=true")
		}
	})
}

func TestConfig_IsProduction(t *testing.T) {
	tests := []struct {
		env  string
		want bool
	}{
		{"production", true},
		{"development", false},
		{"staging", false},
		{"test", false},
	}

	for _, tt := range tests {
		t.Run(tt.env, func(t *testing.T) {
			cfg := &Config{
				Logger: LoggerConfig{
					Environment: tt.env,
				},
			}
			if got := cfg.IsProduction(); got != tt.want {
				t.Errorf("IsProduction() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestConfig_IsDevelopment(t *testing.T) {
	tests := []struct {
		env  string
		want bool
	}{
		{"production", false},
		{"development", true},
		{"staging", false},
		{"test", false},
	}

	for _, tt := range tests {
		t.Run(tt.env, func(t *testing.T) {
			cfg := &Config{
				Logger: LoggerConfig{
					Environment: tt.env,
				},
			}
			if got := cfg.IsDevelopment(); got != tt.want {
				t.Errorf("IsDevelopment() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestConfig_GetBindAddress(t *testing.T) {
	tests := []struct {
		host string
		port int
		want string
	}{
		{"127.0.0.1", 8080, "127.0.0.1:8080"},
		{"0.0.0.0", 3000, "0.0.0.0:3000"},
		{"localhost", 9000, "localhost:9000"},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			cfg := &Config{
				Server: ServerConfig{
					Host: tt.host,
					Port: tt.port,
				},
			}
			if got := cfg.GetBindAddress(); got != tt.want {
				t.Errorf("GetBindAddress() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestConfig_ValidationErrorFormat(t *testing.T) {
	WithEnv(t, "DATABASE_URL", "")
	WithEnv(t, "NODE_ENV", "invalid")
	WithEnv(t, "LOG_LEVEL", "invalid")

	_, err := Load()
	if err == nil {
		t.Fatal("Expected validation errors")
	}

	errStr := err.Error()
	if !strings.Contains(errStr, "configuration validation failed:") {
		t.Error("Expected error message to start with 'configuration validation failed:'")
	}

	// Should contain all three errors
	if !strings.Contains(errStr, "DATABASE_URL") {
		t.Error("Expected error message to contain DATABASE_URL")
	}
	if !strings.Contains(errStr, "NODE_ENV") {
		t.Error("Expected error message to contain NODE_ENV")
	}
	if !strings.Contains(errStr, "LOG_LEVEL") {
		t.Error("Expected error message to contain LOG_LEVEL")
	}
}

func TestConfig_Validate_MissingAPIKeyInProduction(t *testing.T) {
	WithEnv(t, "DATABASE_URL", "postgres://localhost/test")
	WithEnv(t, "NODE_ENV", "production")
	WithEnv(t, "SESSION_SECRET", "test-secret")
	// Don't set API_KEY

	_, err := Load()
	if err == nil {
		t.Fatal("Expected error when API_KEY is missing in production")
	}

	if verr, ok := err.(ValidationErrors); ok {
		found := false
		for _, e := range verr {
			if e.Field == "API_KEY" {
				found = true
				break
			}
		}
		if !found {
			t.Error("Expected validation error for API_KEY")
		}
	}
}

func TestConfig_Validate_APIKeyOptionalInDevelopment(t *testing.T) {
	WithEnv(t, "DATABASE_URL", "postgres://localhost/test")
	WithEnv(t, "NODE_ENV", "development")
	// Don't set API_KEY

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Should allow empty API_KEY in development: %v", err)
	}

	if cfg.External.APIKey != "" {
		t.Error("Expected empty API_KEY in development")
	}
}

func TestConfig_DatabasePoolDefaults(t *testing.T) {
	WithEnv(t, "DATABASE_URL", "postgres://localhost/test")
	WithEnv(t, "NODE_ENV", "development")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}

	// Verify Pi-optimized defaults
	if cfg.Database.MaxConns != DefaultDBMaxConns {
		t.Errorf("Expected MaxConns=%d, got %d", DefaultDBMaxConns, cfg.Database.MaxConns)
	}
	if cfg.Database.MinConns != DefaultDBMinConns {
		t.Errorf("Expected MinConns=%d, got %d", DefaultDBMinConns, cfg.Database.MinConns)
	}
	if cfg.Database.MaxConnIdleTime != DefaultDBMaxConnIdleTime {
		t.Errorf("Expected MaxConnIdleTime=%v, got %v", DefaultDBMaxConnIdleTime, cfg.Database.MaxConnIdleTime)
	}
	if cfg.Database.MaxConnLifetime != DefaultDBMaxConnLifetime {
		t.Errorf("Expected MaxConnLifetime=%v, got %v", DefaultDBMaxConnLifetime, cfg.Database.MaxConnLifetime)
	}
	if cfg.Database.HealthCheckPeriod != DefaultDBHealthCheckPeriod {
		t.Errorf("Expected HealthCheckPeriod=%v, got %v", DefaultDBHealthCheckPeriod, cfg.Database.HealthCheckPeriod)
	}
}

func TestConfig_DatabasePoolFromEnv(t *testing.T) {
	WithEnv(t, "DATABASE_URL", "postgres://localhost/test")
	WithEnv(t, "NODE_ENV", "development")
	WithEnv(t, "DB_MAX_CONNS", "20")
	WithEnv(t, "DB_MIN_CONNS", "3")
	WithEnv(t, "DB_MAX_CONN_IDLE_TIME", "10m")
	WithEnv(t, "DB_MAX_CONN_LIFETIME", "1h")
	WithEnv(t, "DB_HEALTH_CHECK_PERIOD", "1m")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}

	if cfg.Database.MaxConns != 20 {
		t.Errorf("Expected MaxConns=20, got %d", cfg.Database.MaxConns)
	}
	if cfg.Database.MinConns != 3 {
		t.Errorf("Expected MinConns=3, got %d", cfg.Database.MinConns)
	}
	if cfg.Database.MaxConnIdleTime != 10*time.Minute {
		t.Errorf("Expected MaxConnIdleTime=10m, got %v", cfg.Database.MaxConnIdleTime)
	}
	if cfg.Database.MaxConnLifetime != 1*time.Hour {
		t.Errorf("Expected MaxConnLifetime=1h, got %v", cfg.Database.MaxConnLifetime)
	}
	if cfg.Database.HealthCheckPeriod != 1*time.Minute {
		t.Errorf("Expected HealthCheckPeriod=1m, got %v", cfg.Database.HealthCheckPeriod)
	}
}

func TestConfig_River_Default(t *testing.T) {
	WithEnv(t, "DATABASE_URL", "postgres://localhost/test")
	WithEnv(t, "NODE_ENV", "development")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}

	if cfg.River.WorkerConcurrency != DefaultRiverWorkerConcurrency {
		t.Errorf("Expected default RiverWorkerConcurrency=%d, got %d",
			DefaultRiverWorkerConcurrency, cfg.River.WorkerConcurrency)
	}
}

func TestConfig_River_FromEnv(t *testing.T) {
	WithEnv(t, "DATABASE_URL", "postgres://localhost/test")
	WithEnv(t, "NODE_ENV", "development")
	WithEnv(t, "RIVER_WORKER_CONCURRENCY", "25")
	// Must raise DB_MAX_CONNS in lockstep or cross-field validation
	// (see TestConfig_Validate_RiverConcurrencyExceedsPool) will reject.
	WithEnv(t, "DB_MAX_CONNS", "30")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}

	if cfg.River.WorkerConcurrency != 25 {
		t.Errorf("Expected RiverWorkerConcurrency=25, got %d", cfg.River.WorkerConcurrency)
	}
}

func TestConfig_Validate_RiverConcurrency(t *testing.T) {
	tests := []struct {
		name         string
		concurrency  int
		wantErrField bool
	}{
		{"zero", 0, true},
		{"negative", -1, true},
		{"one", 1, false},
		{"default", DefaultRiverWorkerConcurrency, false},
		{"max", 1000, false},
		{"over_max", 1001, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := TestConfig()
			// Give the pool enough headroom that the cross-field check
			// (Validate_RiverConcurrencyExceedsPool) never fires here;
			// we're testing only the range bounds in this table.
			cfg.Database.MaxConns = 2000
			cfg.River.WorkerConcurrency = tt.concurrency

			err := cfg.Validate()
			if tt.wantErrField {
				if err == nil {
					t.Fatalf("Expected validation error for concurrency=%d", tt.concurrency)
				}
				verr, ok := err.(ValidationErrors)
				if !ok {
					t.Fatalf("Expected ValidationErrors, got %T", err)
				}
				found := false
				for _, e := range verr {
					if e.Field == "RIVER_WORKER_CONCURRENCY" {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("Expected RIVER_WORKER_CONCURRENCY validation error, got: %v", err)
				}
			} else {
				if err != nil {
					t.Errorf("Expected no error for concurrency=%d, got: %v", tt.concurrency, err)
				}
			}
		})
	}
}

// TestConfig_Validate_RiverConcurrencyExceedsPool asserts the cross-field
// sanity check: DB_MAX_CONNS must exceed RIVER_WORKER_CONCURRENCY by at
// least 3 (river's internal leader/notifier/completer overhead + HTTP).
func TestConfig_Validate_RiverConcurrencyExceedsPool(t *testing.T) {
	tests := []struct {
		name        string
		maxConns    int32
		concurrency int
		wantErr     bool
	}{
		{"concurrency_equals_pool", 10, 10, true},
		{"concurrency_exceeds_pool", 5, 10, true},
		{"headroom_of_one", 11, 10, true},    // 10+3 > 11 → fail
		{"headroom_of_two", 12, 10, true},    // 10+3 > 12 → fail
		{"headroom_of_three", 13, 10, false}, // 10+3 == 13 → ok
		{"headroom_of_five", 15, 10, false},  // default
		{"default_pool_default_concurrency", DefaultDBMaxConns, DefaultRiverWorkerConcurrency, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := TestConfig()
			cfg.Database.MaxConns = tt.maxConns
			cfg.River.WorkerConcurrency = tt.concurrency
			err := cfg.Validate()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Expected validation error for MaxConns=%d, concurrency=%d",
						tt.maxConns, tt.concurrency)
				}
			} else if err != nil {
				t.Errorf("Expected no error for MaxConns=%d, concurrency=%d; got: %v",
					tt.maxConns, tt.concurrency, err)
			}
		})
	}
}

func TestConfig_TestConfig_ValidatesCleanly(t *testing.T) {
	cfg := TestConfig()
	if err := cfg.Validate(); err != nil {
		t.Errorf("TestConfig() should Validate cleanly, got: %v", err)
	}
}

// TestConfig_River_JobTimeout_Default asserts the default from Load()
// matches DefaultRiverJobTimeout (6 minutes).
func TestConfig_River_JobTimeout_Default(t *testing.T) {
	WithEnv(t, "DATABASE_URL", "postgres://localhost/test")
	WithEnv(t, "NODE_ENV", "development")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}

	if cfg.River.JobTimeout != DefaultRiverJobTimeout {
		t.Errorf("Expected default JobTimeout=%s, got %s",
			DefaultRiverJobTimeout, cfg.River.JobTimeout)
	}
}

// TestConfig_River_JobTimeout_FromEnv asserts RIVER_JOB_TIMEOUT is
// parsed as a Go duration (e.g., "45s", "2m30s").
func TestConfig_River_JobTimeout_FromEnv(t *testing.T) {
	WithEnv(t, "DATABASE_URL", "postgres://localhost/test")
	WithEnv(t, "NODE_ENV", "development")
	WithEnv(t, "RIVER_JOB_TIMEOUT", "45s")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}

	if cfg.River.JobTimeout != 45*time.Second {
		t.Errorf("Expected JobTimeout=45s, got %s", cfg.River.JobTimeout)
	}
}

// TestConfig_Validate_RiverJobTimeout exercises the [1s, 1h] range.
func TestConfig_Validate_RiverJobTimeout(t *testing.T) {
	tests := []struct {
		name       string
		timeout    time.Duration
		wantErr    bool
		wantErrMsg string
	}{
		{"zero", 0, true, "RIVER_JOB_TIMEOUT"},
		{"below_min", 500 * time.Millisecond, true, "RIVER_JOB_TIMEOUT"},
		{"at_min", 1 * time.Second, false, ""},
		{"default", DefaultRiverJobTimeout, false, ""},
		{"at_max", 1 * time.Hour, false, ""},
		{"above_max", 61 * time.Minute, true, "RIVER_JOB_TIMEOUT"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := TestConfig()
			cfg.River.JobTimeout = tt.timeout

			err := cfg.Validate()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Expected validation error for timeout=%s", tt.timeout)
				}
				verr, ok := err.(ValidationErrors)
				if !ok {
					t.Fatalf("Expected ValidationErrors, got %T", err)
				}
				found := false
				for _, e := range verr {
					if e.Field == tt.wantErrMsg {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("Expected %s validation error, got: %v", tt.wantErrMsg, err)
				}
			} else if err != nil {
				t.Errorf("Expected no error for timeout=%s, got: %v", tt.timeout, err)
			}
		})
	}
}

func TestConfig_DatabasePoolInvalidDuration(t *testing.T) {
	WithEnv(t, "DATABASE_URL", "postgres://localhost/test")
	WithEnv(t, "NODE_ENV", "development")
	WithEnv(t, "DB_MAX_CONN_IDLE_TIME", "invalid")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}

	// Invalid duration should fall back to default
	if cfg.Database.MaxConnIdleTime != DefaultDBMaxConnIdleTime {
		t.Errorf("Expected fallback to default MaxConnIdleTime=%v, got %v", DefaultDBMaxConnIdleTime, cfg.Database.MaxConnIdleTime)
	}
}

// TestConfig_EventBus_DefaultCutover asserts EVENT_BUS_INTERACTION_MODE
// defaults to "cutover" after PR 6. The PR 5 default ("off") is retained
// as a valid config value for rollback flexibility, but it is effectively
// a no-op post-cutover (the direct path is gone).
func TestConfig_EventBus_DefaultCutover(t *testing.T) {
	WithEnv(t, "DATABASE_URL", "postgres://localhost/test")
	WithEnv(t, "NODE_ENV", "development")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}
	if cfg.EventBus.InteractionMode != EventBusInteractionModeCutover {
		t.Errorf("Expected default EventBus.InteractionMode=%q, got %q",
			EventBusInteractionModeCutover, cfg.EventBus.InteractionMode)
	}
}

func TestConfig_EventBus_ShadowFromEnv(t *testing.T) {
	WithEnv(t, "DATABASE_URL", "postgres://localhost/test")
	WithEnv(t, "NODE_ENV", "development")
	WithEnv(t, "EVENT_BUS_INTERACTION_MODE", "shadow")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}
	if cfg.EventBus.InteractionMode != EventBusInteractionModeShadow {
		t.Errorf("Expected EventBus.InteractionMode=shadow, got %q", cfg.EventBus.InteractionMode)
	}
}

// TestConfig_EventBus_OffFromEnv asserts "off" still parses — rollback
// flexibility per plan Decision 6. Post-cutover "off" disables publisher-
// driven writes entirely (see EventBusConfig doc comment).
func TestConfig_EventBus_OffFromEnv(t *testing.T) {
	WithEnv(t, "DATABASE_URL", "postgres://localhost/test")
	WithEnv(t, "NODE_ENV", "development")
	WithEnv(t, "EVENT_BUS_INTERACTION_MODE", "off")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}
	if cfg.EventBus.InteractionMode != EventBusInteractionModeOff {
		t.Errorf("Expected EventBus.InteractionMode=off, got %q", cfg.EventBus.InteractionMode)
	}
}

func TestConfig_EventBus_CutoverFromEnv(t *testing.T) {
	WithEnv(t, "DATABASE_URL", "postgres://localhost/test")
	WithEnv(t, "NODE_ENV", "development")
	WithEnv(t, "EVENT_BUS_INTERACTION_MODE", "cutover")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}
	if cfg.EventBus.InteractionMode != EventBusInteractionModeCutover {
		t.Errorf("Expected EventBus.InteractionMode=cutover, got %q", cfg.EventBus.InteractionMode)
	}
}

func TestConfig_Validate_EventBusInteractionMode(t *testing.T) {
	tests := []struct {
		name     string
		mode     string
		wantErr  bool
		wantHint string
	}{
		{"off_ok", EventBusInteractionModeOff, false, ""},
		{"shadow_ok", EventBusInteractionModeShadow, false, ""},
		{"cutover_ok", EventBusInteractionModeCutover, false, ""},
		{"empty_rejected", "", true, "invalid mode"},
		{"gibberish_rejected", "garbage", true, "invalid mode"},
		{"uppercase_rejected", "OFF", true, "invalid mode"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := TestConfig()
			cfg.EventBus.InteractionMode = tt.mode

			err := cfg.Validate()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Expected validation error for mode=%q", tt.mode)
				}
				verr, ok := err.(ValidationErrors)
				if !ok {
					t.Fatalf("Expected ValidationErrors, got %T", err)
				}
				found := false
				for _, e := range verr {
					if e.Field == "EVENT_BUS_INTERACTION_MODE" {
						found = true
						if tt.wantHint != "" && !strings.Contains(e.Message, tt.wantHint) {
							t.Errorf("Expected message to contain %q, got %q", tt.wantHint, e.Message)
						}
						break
					}
				}
				if !found {
					t.Errorf("Expected EVENT_BUS_INTERACTION_MODE validation error, got: %v", err)
				}
			} else {
				if err != nil {
					t.Errorf("Expected no error for mode=%q, got: %v", tt.mode, err)
				}
			}
		})
	}
}

// TestConfig_CadenceMode_DefaultCutover asserts EVENT_BUS_CADENCE_MODE
// defaults to "cutover" post-cutover — the direct path is gone, the
// consumer is the sole writer, and shadow/off are no longer safe
// defaults.
func TestConfig_CadenceMode_DefaultCutover(t *testing.T) {
	WithEnv(t, "DATABASE_URL", "postgres://localhost/test")
	WithEnv(t, "NODE_ENV", "development")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}
	if cfg.EventBus.CadenceMode != EventBusCadenceModeCutover {
		t.Errorf("Expected default EventBus.CadenceMode=%q, got %q",
			EventBusCadenceModeCutover, cfg.EventBus.CadenceMode)
	}
}

// TestConfig_CadenceMode_OffRejectedWithoutUnsafeOverride asserts the
// startup gate: EVENT_BUS_CADENCE_MODE=off is a configuration error in
// non-test load paths because post-cutover the direct path is gone and
// "off" silently disables the sole writer of cadence columns.
func TestConfig_CadenceMode_OffRejectedWithoutUnsafeOverride(t *testing.T) {
	WithEnv(t, "DATABASE_URL", "postgres://localhost/test")
	WithEnv(t, "NODE_ENV", "development")
	WithEnv(t, "EVENT_BUS_CADENCE_MODE", "off")

	_, err := Load()
	if err == nil {
		t.Fatal("Expected Load() to reject EVENT_BUS_CADENCE_MODE=off without unsafe override")
	}
	verr, ok := err.(ValidationErrors)
	if !ok {
		t.Fatalf("Expected ValidationErrors, got %T: %v", err, err)
	}
	found := false
	for _, e := range verr {
		if e.Field == "EVENT_BUS_CADENCE_MODE" {
			found = true
			if !strings.Contains(e.Message, "git revert") {
				t.Errorf("Expected message to mention `git revert` rollback, got %q", e.Message)
			}
			if !strings.Contains(e.Message, "EVENT_BUS_CADENCE_UNSAFE_ALLOW_OFF=true") {
				t.Errorf("Expected message to mention unsafe override env var, got %q", e.Message)
			}
		}
	}
	if !found {
		t.Errorf("Expected EVENT_BUS_CADENCE_MODE validation error, got: %v", err)
	}
}

// TestConfig_CadenceMode_OffAllowedWithUnsafeOverride asserts the
// emergency escape hatch: EVENT_BUS_CADENCE_UNSAFE_ALLOW_OFF=true lets
// the startup gate accept "off" even though it's dangerous.
func TestConfig_CadenceMode_OffAllowedWithUnsafeOverride(t *testing.T) {
	WithEnv(t, "DATABASE_URL", "postgres://localhost/test")
	WithEnv(t, "NODE_ENV", "development")
	WithEnv(t, "EVENT_BUS_CADENCE_MODE", "off")
	WithEnv(t, "EVENT_BUS_CADENCE_UNSAFE_ALLOW_OFF", "true")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() should allow EVENT_BUS_CADENCE_MODE=off when unsafe override is set, got: %v", err)
	}
	if cfg.EventBus.CadenceMode != EventBusCadenceModeOff {
		t.Errorf("Expected EventBus.CadenceMode=off, got %q", cfg.EventBus.CadenceMode)
	}
	if !cfg.EventBus.UnsafeAllowOffMode {
		t.Error("Expected UnsafeAllowOffMode=true when EVENT_BUS_CADENCE_UNSAFE_ALLOW_OFF=true")
	}
}

// TestTestConfig_AllowsCadenceModeOff asserts TestConfig() keeps
// bypass behavior for "off" — unit/integration harnesses can exercise
// off-mode branches without tripping the startup gate.
func TestTestConfig_AllowsCadenceModeOff(t *testing.T) {
	cfg := TestConfig()
	if cfg.EventBus.CadenceMode != EventBusCadenceModeOff {
		t.Errorf("Expected TestConfig().EventBus.CadenceMode=off, got %q", cfg.EventBus.CadenceMode)
	}
	if !cfg.EventBus.UnsafeAllowOffMode {
		t.Error("TestConfig() must set UnsafeAllowOffMode=true so Validate() accepts mode=off")
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("TestConfig() must Validate cleanly even with CadenceMode=off, got: %v", err)
	}
}

// TestConfig_CadenceMode_CutoverFromEnv asserts "cutover" parses — PR 7
// logs an ERROR and treats it as shadow, but the config value must flow
// through for the startup log line to fire.
func TestConfig_CadenceMode_CutoverFromEnv(t *testing.T) {
	WithEnv(t, "DATABASE_URL", "postgres://localhost/test")
	WithEnv(t, "NODE_ENV", "development")
	WithEnv(t, "EVENT_BUS_CADENCE_MODE", "cutover")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}
	if cfg.EventBus.CadenceMode != EventBusCadenceModeCutover {
		t.Errorf("Expected EventBus.CadenceMode=cutover, got %q", cfg.EventBus.CadenceMode)
	}
}

func TestConfig_Validate_EventBusCadenceMode(t *testing.T) {
	tests := []struct {
		name     string
		mode     string
		wantErr  bool
		wantHint string
	}{
		// "off" accepted here because TestConfig() sets
		// UnsafeAllowOffMode=true (see TestTestConfig_AllowsCadenceModeOff).
		{"off_ok_with_test_override", EventBusCadenceModeOff, false, ""},
		{"shadow_ok", EventBusCadenceModeShadow, false, ""},
		{"cutover_ok", EventBusCadenceModeCutover, false, ""},
		{"empty_rejected", "", true, "invalid mode"},
		{"gibberish_rejected", "garbage", true, "invalid mode"},
		{"uppercase_rejected", "SHADOW", true, "invalid mode"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := TestConfig()
			cfg.EventBus.CadenceMode = tt.mode

			err := cfg.Validate()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Expected validation error for mode=%q", tt.mode)
				}
				verr, ok := err.(ValidationErrors)
				if !ok {
					t.Fatalf("Expected ValidationErrors, got %T", err)
				}
				found := false
				for _, e := range verr {
					if e.Field == "EVENT_BUS_CADENCE_MODE" {
						found = true
						if tt.wantHint != "" && !strings.Contains(e.Message, tt.wantHint) {
							t.Errorf("Expected message to contain %q, got %q", tt.wantHint, e.Message)
						}
						break
					}
				}
				if !found {
					t.Errorf("Expected EVENT_BUS_CADENCE_MODE validation error, got: %v", err)
				}
			} else if err != nil {
				t.Errorf("Expected no error for mode=%q, got: %v", tt.mode, err)
			}
		})
	}
}

// TestConfig_FollowUpMode_DefaultShadow asserts EVENT_BUS_FOLLOWUP_MODE
// defaults to "shadow" — the direct path is still the authoritative
// follow-up writer, and the consumer observes in parallel.
func TestConfig_FollowUpMode_DefaultShadow(t *testing.T) {
	WithEnv(t, "DATABASE_URL", "postgres://localhost/test")
	WithEnv(t, "NODE_ENV", "development")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}
	if cfg.EventBus.FollowUpMode != EventBusFollowUpModeShadow {
		t.Errorf("Expected default EventBus.FollowUpMode=%q, got %q",
			EventBusFollowUpModeShadow, cfg.EventBus.FollowUpMode)
	}
}

// TestConfig_FollowUpMode_OffAccepted asserts mode=off is a normal
// runtime value — unlike CadenceMode, there is no unsafe-override
// gate because the direct path is still live in shadow phase.
func TestConfig_FollowUpMode_OffAccepted(t *testing.T) {
	WithEnv(t, "DATABASE_URL", "postgres://localhost/test")
	WithEnv(t, "NODE_ENV", "development")
	WithEnv(t, "EVENT_BUS_FOLLOWUP_MODE", "off")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}
	if cfg.EventBus.FollowUpMode != EventBusFollowUpModeOff {
		t.Errorf("Expected EventBus.FollowUpMode=off, got %q", cfg.EventBus.FollowUpMode)
	}
}

// TestConfig_FollowUpMode_CutoverFromEnv asserts "cutover" parses —
// the subsequent PR flips the default, and the config value must flow
// through the startup seam without validation rejection.
func TestConfig_FollowUpMode_CutoverFromEnv(t *testing.T) {
	WithEnv(t, "DATABASE_URL", "postgres://localhost/test")
	WithEnv(t, "NODE_ENV", "development")
	WithEnv(t, "EVENT_BUS_FOLLOWUP_MODE", "cutover")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}
	if cfg.EventBus.FollowUpMode != EventBusFollowUpModeCutover {
		t.Errorf("Expected EventBus.FollowUpMode=cutover, got %q", cfg.EventBus.FollowUpMode)
	}
}

func TestConfig_Validate_EventBusFollowUpMode(t *testing.T) {
	tests := []struct {
		name    string
		mode    string
		wantErr bool
	}{
		{"off_ok", EventBusFollowUpModeOff, false},
		{"shadow_ok", EventBusFollowUpModeShadow, false},
		{"cutover_ok", EventBusFollowUpModeCutover, false},
		{"empty_rejected", "", true},
		{"gibberish_rejected", "garbage", true},
		{"uppercase_rejected", "SHADOW", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := TestConfig()
			cfg.EventBus.FollowUpMode = tt.mode

			err := cfg.Validate()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Expected validation error for mode=%q", tt.mode)
				}
				verr, ok := err.(ValidationErrors)
				if !ok {
					t.Fatalf("Expected ValidationErrors, got %T", err)
				}
				found := false
				for _, e := range verr {
					if e.Field == "EVENT_BUS_FOLLOWUP_MODE" {
						found = true
						if !strings.Contains(e.Message, "invalid mode") {
							t.Errorf("Expected message to mention invalid mode, got %q", e.Message)
						}
						break
					}
				}
				if !found {
					t.Errorf("Expected EVENT_BUS_FOLLOWUP_MODE validation error, got: %v", err)
				}
			} else if err != nil {
				t.Errorf("Expected no error for mode=%q, got: %v", tt.mode, err)
			}
		})
	}
}
