package config

import (
	"os"
	"strings"
	"testing"
)

// WithEnv is a test helper that sets environment variables for the duration of a test
func WithEnv(t *testing.T, key, value string) {
	t.Helper()
	original := os.Getenv(key)
	os.Setenv(key, value)
	t.Cleanup(func() {
		if original == "" {
			os.Unsetenv(key)
		} else {
			os.Setenv(key, original)
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
	WithEnv(t, "ENABLE_TELEGRAM_BOT", "true")
	// Don't set TELEGRAM_BOT_TOKEN

	_, err := Load()
	if err == nil {
		t.Fatal("Expected error when TELEGRAM_BOT_TOKEN is missing but ENABLE_TELEGRAM_BOT is true")
	}

	if verr, ok := err.(ValidationErrors); ok {
		found := false
		for _, e := range verr {
			if e.Field == "TELEGRAM_BOT_TOKEN" {
				found = true
				break
			}
		}
		if !found {
			t.Error("Expected validation error for TELEGRAM_BOT_TOKEN")
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
