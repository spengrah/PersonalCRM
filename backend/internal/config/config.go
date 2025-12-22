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
}

// DatabaseConfig holds database connection settings
type DatabaseConfig struct {
	URL            string        // Required
	MigrationsPath string        // Default: "migrations"
	HealthTimeout  time.Duration // Default: 5s
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
	EnableVectorSearch bool // Default: false
	EnableTelegramBot  bool // Default: false
	EnableCalendarSync bool // Default: false
}

// RuntimeConfig holds runtime-only settings (not validated at startup)
type RuntimeConfig struct {
	CRMEnvironment   string // production|staging|test|accelerated (affects cadence)
	TimeAcceleration int    // Default: 1 (no acceleration)
	TimeBase         string // RFC3339 timestamp for acceleration base
}

// ExternalConfig holds external service credentials
type ExternalConfig struct {
	SessionSecret    string // Required in production
	AnthropicAPIKey  string // Optional (future use)
	TelegramBotToken string // Optional (if EnableTelegramBot)
	BackupPath       string // Optional
	HomeServerHost   string // Optional
	HomeServerUser   string // Optional
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
		sb.WriteString(fmt.Sprintf("  - %s: %s\n", err.Field, err.Message))
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
)

// Load reads configuration from environment variables
func Load() (*Config, error) {
	cfg := &Config{
		Database: DatabaseConfig{
			URL:            getEnv("DATABASE_URL", ""),
			MigrationsPath: getEnv("MIGRATIONS_PATH", DefaultMigrationsPath),
			HealthTimeout:  DefaultHealthCheckTimeout,
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
			EnableVectorSearch: getEnvAsBool("ENABLE_VECTOR_SEARCH", false),
			EnableTelegramBot:  getEnvAsBool("ENABLE_TELEGRAM_BOT", false),
			EnableCalendarSync: getEnvAsBool("ENABLE_CALENDAR_SYNC", false),
		},
		Runtime: RuntimeConfig{
			CRMEnvironment:   getEnv("CRM_ENV", DefaultCRMEnvironment),
			TimeAcceleration: getEnvAsInt("TIME_ACCELERATION", 1),
			TimeBase:         getEnv("TIME_BASE", ""),
		},
		External: ExternalConfig{
			SessionSecret:    getEnv("SESSION_SECRET", ""),
			AnthropicAPIKey:  getEnv("ANTHROPIC_API_KEY", ""),
			TelegramBotToken: getEnv("TELEGRAM_BOT_TOKEN", ""),
			BackupPath:       getEnv("BACKUP_PATH", ""),
			HomeServerHost:   getEnv("HOME_SERVER_HOST", ""),
			HomeServerUser:   getEnv("HOME_SERVER_USER", ""),
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

	// Dependency validation: Telegram token required if feature enabled
	if c.Features.EnableTelegramBot && c.External.TelegramBotToken == "" {
		errors = append(errors, ValidationError{
			Field:   "TELEGRAM_BOT_TOKEN",
			Message: "telegram bot token is required when ENABLE_TELEGRAM_BOT is true",
		})
	}

	// CORS validation: FrontendURL should be set if not allowing all
	if !c.CORS.AllowAll && c.CORS.FrontendURL == "" {
		errors = append(errors, ValidationError{
			Field:   "FRONTEND_URL",
			Message: "frontend URL should be set when CORS_ALLOW_ALL is false",
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
			URL:            "postgres://test:test@localhost:5432/test?sslmode=disable",
			MigrationsPath: "../../migrations",
			HealthTimeout:  DefaultHealthCheckTimeout,
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
			EnableVectorSearch: false,
			EnableTelegramBot:  false,
			EnableCalendarSync: false,
		},
		Runtime: RuntimeConfig{
			CRMEnvironment:   "test",
			TimeAcceleration: 1,
			TimeBase:         "",
		},
		External: ExternalConfig{
			SessionSecret: "test-secret",
		},
	}
}
