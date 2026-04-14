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
