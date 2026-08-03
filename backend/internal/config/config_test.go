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

func TestConfig_River_JobSampleRetentionDays_Default(t *testing.T) {
	WithEnv(t, "DATABASE_URL", "postgres://localhost/test")
	WithEnv(t, "NODE_ENV", "development")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}

	if cfg.River.JobSampleRetentionDays != DefaultRiverJobSampleRetentionDays {
		t.Errorf("Expected default JobSampleRetentionDays=%d, got %d",
			DefaultRiverJobSampleRetentionDays, cfg.River.JobSampleRetentionDays)
	}
}

// TestConfig_River_JobSampleRetentionDays_FromEnv asserts
// RIVER_JOB_SAMPLE_RETENTION_DAYS is parsed from the environment.
func TestConfig_River_JobSampleRetentionDays_FromEnv(t *testing.T) {
	WithEnv(t, "DATABASE_URL", "postgres://localhost/test")
	WithEnv(t, "NODE_ENV", "development")
	WithEnv(t, "RIVER_JOB_SAMPLE_RETENTION_DAYS", "30")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}

	if cfg.River.JobSampleRetentionDays != 30 {
		t.Errorf("Expected JobSampleRetentionDays=30, got %d", cfg.River.JobSampleRetentionDays)
	}
}

// TestConfig_Validate_RiverJobSampleRetentionDays exercises the [1, 365] range.
func TestConfig_Validate_RiverJobSampleRetentionDays(t *testing.T) {
	tests := []struct {
		name    string
		days    int
		wantErr bool
	}{
		{"zero", 0, true},
		{"negative", -1, true},
		{"at_min", 1, false},
		{"default", DefaultRiverJobSampleRetentionDays, false},
		{"at_max", 365, false},
		{"above_max", 366, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := TestConfig()
			cfg.River.JobSampleRetentionDays = tt.days

			err := cfg.Validate()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Expected validation error for days=%d", tt.days)
				}
				verr, ok := err.(ValidationErrors)
				if !ok {
					t.Fatalf("Expected ValidationErrors, got %T", err)
				}
				found := false
				for _, e := range verr {
					if e.Field == "RIVER_JOB_SAMPLE_RETENTION_DAYS" {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("Expected RIVER_JOB_SAMPLE_RETENTION_DAYS validation error, got: %v", err)
				}
			} else if err != nil {
				t.Errorf("Expected no error for days=%d, got: %v", tt.days, err)
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
// defaults to "cutover". "off" is retained as a valid config value for
// rollback flexibility but is effectively a no-op — no direct path
// remains.
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

// TestConfig_EventBus_OffFromEnv asserts "off" still parses — rollback
// flexibility. "off" disables publisher-driven writes entirely (see
// EventBusConfig doc comment).
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
		{"cutover_ok", EventBusInteractionModeCutover, false, ""},
		{"shadow_rejected", "shadow", true, "invalid mode"},
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

// TestConfig_CadenceMode_CutoverFromEnv asserts "cutover" parses and
// flows through as the cutover mode value.
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
		{"cutover_ok", EventBusCadenceModeCutover, false, ""},
		{"shadow_rejected", "shadow", true, "invalid mode"},
		{"empty_rejected", "", true, "invalid mode"},
		{"gibberish_rejected", "garbage", true, "invalid mode"},
		{"uppercase_rejected", "CUTOVER", true, "invalid mode"},
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

// TestConfig_FollowUpMode_DefaultCutover asserts EVENT_BUS_FOLLOWUP_MODE
// defaults to "cutover" — FollowUpManager is the sole writer of
// follow-up tasks post-cutover.
func TestConfig_FollowUpMode_DefaultCutover(t *testing.T) {
	WithEnv(t, "DATABASE_URL", "postgres://localhost/test")
	WithEnv(t, "NODE_ENV", "development")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}
	if cfg.EventBus.FollowUpMode != EventBusFollowUpModeCutover {
		t.Errorf("Expected default EventBus.FollowUpMode=%q, got %q",
			EventBusFollowUpModeCutover, cfg.EventBus.FollowUpMode)
	}
}

// TestConfig_FollowUpMode_OffRejectedWithoutUnsafeOverride asserts
// that mode=off without the unsafe-override flag fails startup — the
// direct path is gone, so "off" silently breaks follow-ups.
func TestConfig_FollowUpMode_OffRejectedWithoutUnsafeOverride(t *testing.T) {
	WithEnv(t, "DATABASE_URL", "postgres://localhost/test")
	WithEnv(t, "NODE_ENV", "development")
	WithEnv(t, "EVENT_BUS_FOLLOWUP_MODE", "off")

	_, err := Load()
	if err == nil {
		t.Fatal("Load() expected validation error for mode=off without unsafe override, got nil")
	}
}

// TestConfig_FollowUpMode_OffAllowedWithUnsafeOverride asserts that
// mode=off plus EVENT_BUS_FOLLOWUP_UNSAFE_ALLOW_OFF=true lets the
// server boot (for emergency ops). The MaybeWarnUnsafeOff log fires
// separately.
func TestConfig_FollowUpMode_OffAllowedWithUnsafeOverride(t *testing.T) {
	WithEnv(t, "DATABASE_URL", "postgres://localhost/test")
	WithEnv(t, "NODE_ENV", "development")
	WithEnv(t, "EVENT_BUS_FOLLOWUP_MODE", "off")
	WithEnv(t, "EVENT_BUS_FOLLOWUP_UNSAFE_ALLOW_OFF", "true")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}
	if cfg.EventBus.FollowUpMode != EventBusFollowUpModeOff {
		t.Errorf("Expected EventBus.FollowUpMode=off, got %q", cfg.EventBus.FollowUpMode)
	}
	if !cfg.EventBus.FollowUpUnsafeAllowOff {
		t.Error("Expected FollowUpUnsafeAllowOff=true")
	}
}

func TestConfig_Validate_EventBusFollowUpMode(t *testing.T) {
	tests := []struct {
		name        string
		mode        string
		unsafeAllow bool
		wantErr     bool
	}{
		{"cutover_ok", EventBusFollowUpModeCutover, false, false},
		{"cutover_with_unsafe_ok", EventBusFollowUpModeCutover, true, false},
		{"off_with_unsafe_ok", EventBusFollowUpModeOff, true, false},
		{"off_without_unsafe_rejected", EventBusFollowUpModeOff, false, true},
		{"shadow_rejected", "shadow", false, true},
		{"shadow_with_unsafe_still_rejected", "shadow", true, true},
		{"empty_rejected", "", false, true},
		{"gibberish_rejected", "garbage", false, true},
		{"uppercase_rejected", "CUTOVER", false, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := TestConfig()
			cfg.EventBus.FollowUpMode = tt.mode
			cfg.EventBus.FollowUpUnsafeAllowOff = tt.unsafeAllow

			err := cfg.Validate()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Expected validation error for mode=%q unsafeAllow=%v", tt.mode, tt.unsafeAllow)
				}
				verr, ok := err.(ValidationErrors)
				if !ok {
					t.Fatalf("Expected ValidationErrors, got %T", err)
				}
				found := false
				for _, e := range verr {
					if e.Field == "EVENT_BUS_FOLLOWUP_MODE" {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("Expected EVENT_BUS_FOLLOWUP_MODE validation error, got: %v", err)
				}
			} else if err != nil {
				t.Errorf("Expected no error for mode=%q unsafeAllow=%v, got: %v", tt.mode, tt.unsafeAllow, err)
			}
		})
	}
}

func TestConfig_Staleness_Defaults(t *testing.T) {
	WithEnv(t, "DATABASE_URL", "postgres://localhost/test")
	WithEnv(t, "NODE_ENV", "development")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}

	if cfg.Staleness.HeartbeatThreshold != DefaultStalenessHeartbeatThreshold {
		t.Errorf("HeartbeatThreshold = %s, want %s", cfg.Staleness.HeartbeatThreshold, DefaultStalenessHeartbeatThreshold)
	}
	if cfg.Staleness.PullThreshold != DefaultStalenessPullThreshold {
		t.Errorf("PullThreshold = %s, want %s", cfg.Staleness.PullThreshold, DefaultStalenessPullThreshold)
	}
	if cfg.Staleness.PushThreshold != DefaultStalenessPushThreshold {
		t.Errorf("PushThreshold = %s, want %s", cfg.Staleness.PushThreshold, DefaultStalenessPushThreshold)
	}
	if cfg.Staleness.ErrorMinCount != DefaultStalenessErrorMinCount {
		t.Errorf("ErrorMinCount = %d, want %d", cfg.Staleness.ErrorMinCount, DefaultStalenessErrorMinCount)
	}
	if cfg.Staleness.ErrorThreshold != DefaultStalenessErrorThreshold {
		t.Errorf("ErrorThreshold = %s, want %s", cfg.Staleness.ErrorThreshold, DefaultStalenessErrorThreshold)
	}

	// Built-in per-source overrides are baked in with zero env.
	wantOverrides := map[string]time.Duration{
		"gcontacts":        72 * time.Hour,
		"phone_calls":      168 * time.Hour,
		"icloud_contacts":  336 * time.Hour,
		"anarlog_sessions": 168 * time.Hour,
		"anarlog_humans":   336 * time.Hour,
	}
	for src, want := range wantOverrides {
		if got := cfg.Staleness.SourceOverrides[src]; got != want {
			t.Errorf("SourceOverrides[%q] = %s, want %s", src, got, want)
		}
	}
}

func TestConfig_Staleness_FromEnv(t *testing.T) {
	WithEnv(t, "DATABASE_URL", "postgres://localhost/test")
	WithEnv(t, "NODE_ENV", "development")
	WithEnv(t, "SYNC_STALENESS_HEARTBEAT_THRESHOLD", "30m")
	WithEnv(t, "SYNC_STALENESS_PULL_THRESHOLD", "12h")
	WithEnv(t, "SYNC_STALENESS_PUSH_THRESHOLD", "72h")
	WithEnv(t, "SYNC_STALENESS_ERROR_MIN_COUNT", "5")
	WithEnv(t, "SYNC_STALENESS_ERROR_THRESHOLD", "2h")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}

	if cfg.Staleness.HeartbeatThreshold != 30*time.Minute {
		t.Errorf("HeartbeatThreshold = %s, want 30m", cfg.Staleness.HeartbeatThreshold)
	}
	if cfg.Staleness.PullThreshold != 12*time.Hour {
		t.Errorf("PullThreshold = %s, want 12h", cfg.Staleness.PullThreshold)
	}
	if cfg.Staleness.PushThreshold != 72*time.Hour {
		t.Errorf("PushThreshold = %s, want 72h", cfg.Staleness.PushThreshold)
	}
	if cfg.Staleness.ErrorMinCount != 5 {
		t.Errorf("ErrorMinCount = %d, want 5", cfg.Staleness.ErrorMinCount)
	}
	if cfg.Staleness.ErrorThreshold != 2*time.Hour {
		t.Errorf("ErrorThreshold = %s, want 2h", cfg.Staleness.ErrorThreshold)
	}
}

func TestConfig_Staleness_SourceOverridesMergeAndDisable(t *testing.T) {
	WithEnv(t, "DATABASE_URL", "postgres://localhost/test")
	WithEnv(t, "NODE_ENV", "development")
	// Override one built-in (gcontacts), add a new source (todoist), and
	// disable one (phone_calls=0s).
	WithEnv(t, "SYNC_STALENESS_SOURCE_OVERRIDES", "gcontacts=96h, todoist=10m ,phone_calls=0s")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}

	if got := cfg.Staleness.SourceOverrides["gcontacts"]; got != 96*time.Hour {
		t.Errorf("env override gcontacts = %s, want 96h", got)
	}
	if got := cfg.Staleness.SourceOverrides["todoist"]; got != 10*time.Minute {
		t.Errorf("env override todoist = %s, want 10m", got)
	}
	if got, ok := cfg.Staleness.SourceOverrides["phone_calls"]; !ok || got != 0 {
		t.Errorf("env override phone_calls = (%s, present=%v), want (0s, true)", got, ok)
	}
	// A built-in left untouched by env survives.
	if got := cfg.Staleness.SourceOverrides["icloud_contacts"]; got != 336*time.Hour {
		t.Errorf("untouched built-in icloud_contacts = %s, want 336h", got)
	}
}

func TestConfig_Staleness_SourceOverridesMalformed(t *testing.T) {
	WithEnv(t, "DATABASE_URL", "postgres://localhost/test")
	WithEnv(t, "NODE_ENV", "development")

	cases := []string{
		"gcontacts",          // missing =duration
		"gcontacts=",         // empty duration
		"=24h",               // empty key
		"gcontacts=notaspan", // unparseable duration
	}
	for _, raw := range cases {
		t.Run(raw, func(t *testing.T) {
			WithEnv(t, "SYNC_STALENESS_SOURCE_OVERRIDES", raw)
			if _, err := Load(); err == nil {
				t.Fatalf("expected Load() to fail for overrides %q", raw)
			}
		})
	}
}

func TestParseStalenessSourceOverrides_Empty(t *testing.T) {
	got, err := parseStalenessSourceOverrides("   ")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil map for blank input, got %v", got)
	}
}

func TestConfig_Staleness_ValidateRejectsNegatives(t *testing.T) {
	tests := []struct {
		name  string
		mutfn func(*Config)
		field string
	}{
		{
			name:  "negative_heartbeat",
			mutfn: func(c *Config) { c.Staleness.HeartbeatThreshold = -1 * time.Minute },
			field: "SYNC_STALENESS_HEARTBEAT_THRESHOLD",
		},
		{
			name:  "negative_pull",
			mutfn: func(c *Config) { c.Staleness.PullThreshold = -1 * time.Hour },
			field: "SYNC_STALENESS_PULL_THRESHOLD",
		},
		{
			name:  "negative_push",
			mutfn: func(c *Config) { c.Staleness.PushThreshold = -1 * time.Hour },
			field: "SYNC_STALENESS_PUSH_THRESHOLD",
		},
		{
			name:  "negative_error_threshold",
			mutfn: func(c *Config) { c.Staleness.ErrorThreshold = -1 * time.Hour },
			field: "SYNC_STALENESS_ERROR_THRESHOLD",
		},
		{
			name:  "negative_error_min_count",
			mutfn: func(c *Config) { c.Staleness.ErrorMinCount = -1 },
			field: "SYNC_STALENESS_ERROR_MIN_COUNT",
		},
		{
			name:  "negative_override",
			mutfn: func(c *Config) { c.Staleness.SourceOverrides = map[string]time.Duration{"gcontacts": -1 * time.Hour} },
			field: "SYNC_STALENESS_SOURCE_OVERRIDES",
		},
		{
			name:  "empty_override_key",
			mutfn: func(c *Config) { c.Staleness.SourceOverrides = map[string]time.Duration{"": 24 * time.Hour} },
			field: "SYNC_STALENESS_SOURCE_OVERRIDES",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := TestConfig()
			tt.mutfn(cfg)

			err := cfg.Validate()
			if err == nil {
				t.Fatalf("expected validation error for %s", tt.name)
			}
			verr, ok := err.(ValidationErrors)
			if !ok {
				t.Fatalf("expected ValidationErrors, got %T", err)
			}
			found := false
			for _, e := range verr {
				if e.Field == tt.field {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("expected %s validation error, got: %v", tt.field, err)
			}
		})
	}
}

func TestConfig_Staleness_ZeroDisablesValidatesCleanly(t *testing.T) {
	// Zero thresholds are the documented "disable this check" sentinel and
	// must validate without error.
	cfg := TestConfig()
	cfg.Staleness.HeartbeatThreshold = 0
	cfg.Staleness.PullThreshold = 0
	cfg.Staleness.PushThreshold = 0
	cfg.Staleness.ErrorMinCount = 0
	cfg.Staleness.ErrorThreshold = 0
	cfg.Staleness.SourceOverrides = map[string]time.Duration{"gcontacts": 0}

	if err := cfg.Validate(); err != nil {
		t.Errorf("expected zero-disables config to validate cleanly, got: %v", err)
	}
}

func TestConfig_Health_Defaults(t *testing.T) {
	WithEnv(t, "DATABASE_URL", "postgres://localhost/test")
	WithEnv(t, "NODE_ENV", "development")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}

	if cfg.Health.RiverDiscardedMax != DefaultHealthRiverDiscardedMax {
		t.Errorf("RiverDiscardedMax = %d, want %d", cfg.Health.RiverDiscardedMax, DefaultHealthRiverDiscardedMax)
	}
	if cfg.Health.RiverOldestDueMax != DefaultHealthRiverOldestDueMax {
		t.Errorf("RiverOldestDueMax = %s, want %s", cfg.Health.RiverOldestDueMax, DefaultHealthRiverOldestDueMax)
	}
	if cfg.Health.SyncWatchdogMaxAge != DefaultHealthSyncWatchdogMaxAge {
		t.Errorf("SyncWatchdogMaxAge = %s, want %s", cfg.Health.SyncWatchdogMaxAge, DefaultHealthSyncWatchdogMaxAge)
	}
	if cfg.Health.DiskPath != DefaultHealthDiskPath {
		t.Errorf("DiskPath = %q, want %q", cfg.Health.DiskPath, DefaultHealthDiskPath)
	}
	if cfg.Health.DiskMinFreePercent != DefaultHealthDiskMinFreePercent {
		t.Errorf("DiskMinFreePercent = %d, want %d", cfg.Health.DiskMinFreePercent, DefaultHealthDiskMinFreePercent)
	}
}

func TestConfig_Health_FromEnv(t *testing.T) {
	WithEnv(t, "DATABASE_URL", "postgres://localhost/test")
	WithEnv(t, "NODE_ENV", "development")
	WithEnv(t, "HEALTH_RIVER_DISCARDED_MAX", "5")
	WithEnv(t, "HEALTH_RIVER_OLDEST_DUE_MAX", "15m")
	WithEnv(t, "HEALTH_SYNC_WATCHDOG_MAX_AGE", "20m")
	WithEnv(t, "HEALTH_DISK_PATH", "/data")
	WithEnv(t, "HEALTH_DISK_MIN_FREE_PERCENT", "25")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}

	if cfg.Health.RiverDiscardedMax != 5 {
		t.Errorf("RiverDiscardedMax = %d, want 5", cfg.Health.RiverDiscardedMax)
	}
	if cfg.Health.RiverOldestDueMax != 15*time.Minute {
		t.Errorf("RiverOldestDueMax = %s, want 15m", cfg.Health.RiverOldestDueMax)
	}
	if cfg.Health.SyncWatchdogMaxAge != 20*time.Minute {
		t.Errorf("SyncWatchdogMaxAge = %s, want 20m", cfg.Health.SyncWatchdogMaxAge)
	}
	if cfg.Health.DiskPath != "/data" {
		t.Errorf("DiskPath = %q, want %q", cfg.Health.DiskPath, "/data")
	}
	if cfg.Health.DiskMinFreePercent != 25 {
		t.Errorf("DiskMinFreePercent = %d, want 25", cfg.Health.DiskMinFreePercent)
	}
}

func TestConfig_Health_ValidateRejectsInvalid(t *testing.T) {
	tests := []struct {
		name  string
		mutfn func(*Config)
		field string
	}{
		{
			name:  "negative_river_discarded_max",
			mutfn: func(c *Config) { c.Health.RiverDiscardedMax = -1 },
			field: "HEALTH_RIVER_DISCARDED_MAX",
		},
		{
			name:  "negative_river_oldest_due",
			mutfn: func(c *Config) { c.Health.RiverOldestDueMax = -1 * time.Minute },
			field: "HEALTH_RIVER_OLDEST_DUE_MAX",
		},
		{
			name:  "negative_watchdog_max_age",
			mutfn: func(c *Config) { c.Health.SyncWatchdogMaxAge = -1 * time.Minute },
			field: "HEALTH_SYNC_WATCHDOG_MAX_AGE",
		},
		{
			name:  "negative_disk_min_free_percent",
			mutfn: func(c *Config) { c.Health.DiskMinFreePercent = -1 },
			field: "HEALTH_DISK_MIN_FREE_PERCENT",
		},
		{
			name:  "disk_min_free_percent_over_100",
			mutfn: func(c *Config) { c.Health.DiskMinFreePercent = 101 },
			field: "HEALTH_DISK_MIN_FREE_PERCENT",
		},
		{
			name:  "empty_disk_path",
			mutfn: func(c *Config) { c.Health.DiskPath = "" },
			field: "HEALTH_DISK_PATH",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := TestConfig()
			tt.mutfn(cfg)

			err := cfg.Validate()
			if err == nil {
				t.Fatalf("expected validation error for %s", tt.name)
			}
			verr, ok := err.(ValidationErrors)
			if !ok {
				t.Fatalf("expected ValidationErrors, got %T", err)
			}
			found := false
			for _, e := range verr {
				if e.Field == tt.field {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("expected %s validation error, got: %v", tt.field, err)
			}
		})
	}
}

func TestConfig_Health_ZeroDisablesValidatesCleanly(t *testing.T) {
	// Zero thresholds are the documented "disable this check" sentinel and
	// must validate without error (DiskPath stays non-empty — it is a path,
	// not a threshold).
	cfg := TestConfig()
	cfg.Health.RiverDiscardedMax = 0
	cfg.Health.RiverOldestDueMax = 0
	cfg.Health.SyncWatchdogMaxAge = 0
	cfg.Health.DiskMinFreePercent = 0

	if err := cfg.Validate(); err != nil {
		t.Errorf("expected zero-disables health config to validate cleanly, got: %v", err)
	}
}

func TestIsProductionCRMEnv(t *testing.T) {
	tests := []struct {
		env  string
		want bool
	}{
		{"production", true},
		{"prod", true},
		{"", true}, // unset CRM_ENV defaults to production at load
		{"staging", false},
		{"test", false},
		{"testing", false},
		{"accelerated", false},
		{"development", false}, // not a valid CRM_ENV, treated non-prod
	}
	for _, tt := range tests {
		t.Run(tt.env, func(t *testing.T) {
			if got := IsProductionCRMEnv(tt.env); got != tt.want {
				t.Errorf("IsProductionCRMEnv(%q) = %v, want %v", tt.env, got, tt.want)
			}
		})
	}
}

// TestConfig_WhatsAppDefaults pins the shipped defaults for the four WhatsApp
// tuning knobs and the feature flag. The flag defaults OFF: nothing WhatsApp is
// constructed, registered or routed until an operator opts in.
func TestConfig_WhatsAppDefaults(t *testing.T) {
	WithEnv(t, "DATABASE_URL", "postgres://localhost/test")
	WithEnv(t, "NODE_ENV", "development")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}
	if cfg.Features.EnableWhatsAppSync {
		t.Error("EnableWhatsAppSync must default to false")
	}
	if cfg.WhatsApp.BurstWindowHours != DefaultWhatsAppBurstWindowHours {
		t.Errorf("BurstWindowHours = %d, want %d", cfg.WhatsApp.BurstWindowHours, DefaultWhatsAppBurstWindowHours)
	}
	if cfg.WhatsApp.ReplyBridgeHours != DefaultWhatsAppReplyBridgeHours {
		t.Errorf("ReplyBridgeHours = %d, want %d", cfg.WhatsApp.ReplyBridgeHours, DefaultWhatsAppReplyBridgeHours)
	}
	if cfg.WhatsApp.DiscoveryMinMessages != DefaultWhatsAppDiscoveryMinMessages {
		t.Errorf("DiscoveryMinMessages = %d, want %d", cfg.WhatsApp.DiscoveryMinMessages, DefaultWhatsAppDiscoveryMinMessages)
	}
	if cfg.WhatsApp.GroupMaxMembers != DefaultWhatsAppGroupMaxMembers {
		t.Errorf("GroupMaxMembers = %d, want %d", cfg.WhatsApp.GroupMaxMembers, DefaultWhatsAppGroupMaxMembers)
	}
	// TestConfig() must agree with Load()'s defaults, or DB-backed tests run
	// against a different WhatsApp shape than production.
	tc := TestConfig()
	if tc.WhatsApp != cfg.WhatsApp || tc.Features.EnableWhatsAppSync {
		t.Errorf("TestConfig() WhatsApp shape %+v (flag %v) diverges from Load() defaults %+v",
			tc.WhatsApp, tc.Features.EnableWhatsAppSync, cfg.WhatsApp)
	}
}

// TestConfig_WhatsAppRequiresExternalSync pins the prerequisite in BOTH
// directions: the inconsistent pair refuses to boot naming ENABLE_EXTERNAL_SYNC,
// while WhatsApp-off and both-on load cleanly. Without the negative direction
// the guard could be unconditional and look identical.
func TestConfig_WhatsAppRequiresExternalSync(t *testing.T) {
	t.Run("whatsapp on without external sync fails", func(t *testing.T) {
		WithEnv(t, "DATABASE_URL", "postgres://localhost/test")
		WithEnv(t, "NODE_ENV", "development")
		WithEnv(t, "ENABLE_WHATSAPP_SYNC", "true")
		WithEnv(t, "ENABLE_EXTERNAL_SYNC", "false")

		_, err := Load()
		if err == nil {
			t.Fatal("expected ENABLE_WHATSAPP_SYNC without ENABLE_EXTERNAL_SYNC to fail validation")
		}
		verr, ok := err.(ValidationErrors)
		if !ok {
			t.Fatalf("expected ValidationErrors, got %T: %v", err, err)
		}
		found := false
		for _, e := range verr {
			if e.Field == "ENABLE_EXTERNAL_SYNC" {
				found = true
			}
		}
		if !found {
			t.Errorf("expected a ValidationError naming ENABLE_EXTERNAL_SYNC, got %v", verr)
		}
	})

	t.Run("whatsapp off is fine without external sync", func(t *testing.T) {
		WithEnv(t, "DATABASE_URL", "postgres://localhost/test")
		WithEnv(t, "NODE_ENV", "development")
		WithEnv(t, "ENABLE_WHATSAPP_SYNC", "false")
		WithEnv(t, "ENABLE_EXTERNAL_SYNC", "false")

		if _, err := Load(); err != nil {
			t.Fatalf("Load() failed with WhatsApp off: %v", err)
		}
	})

	t.Run("both on loads", func(t *testing.T) {
		WithEnv(t, "DATABASE_URL", "postgres://localhost/test")
		WithEnv(t, "NODE_ENV", "development")
		WithEnv(t, "ENABLE_WHATSAPP_SYNC", "true")
		WithEnv(t, "ENABLE_EXTERNAL_SYNC", "true")

		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load() failed with both flags on: %v", err)
		}
		if !cfg.Features.EnableWhatsAppSync {
			t.Error("EnableWhatsAppSync should be true")
		}
	})
}

// TestConfig_WhatsAppValidationRejectsOutOfRange walks every knob's bounds.
// getEnvAsInt falls back to the default on a malformed value, so only a
// well-formed but out-of-range value reaches Validate — which is exactly the
// case that would otherwise silently misconfigure ingest.
func TestConfig_WhatsAppValidationRejectsOutOfRange(t *testing.T) {
	cases := []struct {
		env   string
		value string
		field string
		valid bool
	}{
		{"WHATSAPP_BURST_WINDOW_HOURS", "0", "WHATSAPP_BURST_WINDOW_HOURS", false},
		{"WHATSAPP_BURST_WINDOW_HOURS", "25", "WHATSAPP_BURST_WINDOW_HOURS", false},
		{"WHATSAPP_BURST_WINDOW_HOURS", "24", "WHATSAPP_BURST_WINDOW_HOURS", true},
		{"WHATSAPP_REPLY_BRIDGE_HOURS", "0", "WHATSAPP_REPLY_BRIDGE_HOURS", false},
		{"WHATSAPP_REPLY_BRIDGE_HOURS", "169", "WHATSAPP_REPLY_BRIDGE_HOURS", false},
		{"WHATSAPP_REPLY_BRIDGE_HOURS", "168", "WHATSAPP_REPLY_BRIDGE_HOURS", true},
		{"WHATSAPP_DISCOVERY_MIN_MESSAGES", "0", "WHATSAPP_DISCOVERY_MIN_MESSAGES", false},
		{"WHATSAPP_DISCOVERY_MIN_MESSAGES", "101", "WHATSAPP_DISCOVERY_MIN_MESSAGES", false},
		{"WHATSAPP_DISCOVERY_MIN_MESSAGES", "1", "WHATSAPP_DISCOVERY_MIN_MESSAGES", true},
		// 1 is the case that matters: a legal integer that would disable all
		// group ingest by making every group "too large".
		{"WHATSAPP_GROUP_MAX_MEMBERS", "0", "WHATSAPP_GROUP_MAX_MEMBERS", false},
		{"WHATSAPP_GROUP_MAX_MEMBERS", "1", "WHATSAPP_GROUP_MAX_MEMBERS", false},
		{"WHATSAPP_GROUP_MAX_MEMBERS", "101", "WHATSAPP_GROUP_MAX_MEMBERS", false},
		{"WHATSAPP_GROUP_MAX_MEMBERS", "2", "WHATSAPP_GROUP_MAX_MEMBERS", true},
	}
	for _, tc := range cases {
		t.Run(tc.env+"="+tc.value, func(t *testing.T) {
			WithEnv(t, "DATABASE_URL", "postgres://localhost/test")
			WithEnv(t, "NODE_ENV", "development")
			WithEnv(t, "ENABLE_WHATSAPP_SYNC", "true")
			WithEnv(t, "ENABLE_EXTERNAL_SYNC", "true")
			WithEnv(t, tc.env, tc.value)

			_, err := Load()
			if tc.valid {
				if err != nil {
					t.Fatalf("expected %s=%s to be accepted, got %v", tc.env, tc.value, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected %s=%s to be rejected", tc.env, tc.value)
			}
			verr, ok := err.(ValidationErrors)
			if !ok {
				t.Fatalf("expected ValidationErrors, got %T: %v", err, err)
			}
			found := false
			for _, e := range verr {
				if e.Field == tc.field {
					found = true
				}
			}
			if !found {
				t.Errorf("expected a ValidationError naming %s, got %v", tc.field, verr)
			}
		})
	}
}

// TestConfig_WhatsAppRangesUnenforcedWhenDisabled proves the range checks are
// scoped to the enabled feature: an absurd knob on a WhatsApp-off deployment is
// inert configuration, not a boot failure.
func TestConfig_WhatsAppRangesUnenforcedWhenDisabled(t *testing.T) {
	WithEnv(t, "DATABASE_URL", "postgres://localhost/test")
	WithEnv(t, "NODE_ENV", "development")
	WithEnv(t, "ENABLE_WHATSAPP_SYNC", "false")
	WithEnv(t, "WHATSAPP_GROUP_MAX_MEMBERS", "0")

	if _, err := Load(); err != nil {
		t.Fatalf("out-of-range knob must not fail validation while WhatsApp is off: %v", err)
	}
}
