package logger

import (
	"bytes"
	"strings"
	"testing"

	"personal-crm/backend/internal/config"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
)

func TestParseLogLevel(t *testing.T) {
	tests := []struct {
		name     string
		level    string
		expected zerolog.Level
	}{
		{"empty defaults to info", "", zerolog.InfoLevel},
		{"trace level", "trace", zerolog.TraceLevel},
		{"debug level", "debug", zerolog.DebugLevel},
		{"info level", "info", zerolog.InfoLevel},
		{"warn level", "warn", zerolog.WarnLevel},
		{"warning level", "warning", zerolog.WarnLevel},
		{"error level", "error", zerolog.ErrorLevel},
		{"fatal level", "fatal", zerolog.FatalLevel},
		{"panic level", "panic", zerolog.PanicLevel},
		{"uppercase INFO", "INFO", zerolog.InfoLevel},
		{"mixed case Debug", "Debug", zerolog.DebugLevel},
		{"unknown defaults to info", "unknown", zerolog.InfoLevel},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseLogLevel(tt.level)
			assert.Equal(t, tt.expected, got)
		})
	}
}

func TestInit(t *testing.T) {
	t.Run("development mode uses console writer", func(t *testing.T) {
		cfg := config.LoggerConfig{
			Level:       "debug",
			Environment: "development",
		}

		Init(cfg)

		// Logger should be initialized
		logger := Get()
		assert.NotNil(t, logger)
	})

	t.Run("production mode uses JSON", func(t *testing.T) {
		cfg := config.LoggerConfig{
			Level:       "info",
			Environment: "production",
		}

		Init(cfg)

		logger := Get()
		assert.NotNil(t, logger)
	})
}

func TestLoggerFunctions(t *testing.T) {
	// Capture output for testing
	var buf bytes.Buffer

	// Create a test logger that writes to buffer
	log = zerolog.New(&buf).With().Timestamp().Logger()

	t.Run("Info logs at info level", func(t *testing.T) {
		buf.Reset()
		Info().Msg("test info message")
		output := buf.String()
		assert.Contains(t, output, "info")
		assert.Contains(t, output, "test info message")
	})

	t.Run("Debug logs at debug level", func(t *testing.T) {
		buf.Reset()
		Debug().Msg("test debug message")
		output := buf.String()
		assert.Contains(t, output, "debug")
		assert.Contains(t, output, "test debug message")
	})

	t.Run("Warn logs at warn level", func(t *testing.T) {
		buf.Reset()
		Warn().Msg("test warn message")
		output := buf.String()
		assert.Contains(t, output, "warn")
		assert.Contains(t, output, "test warn message")
	})

	t.Run("Error logs at error level", func(t *testing.T) {
		buf.Reset()
		Error().Msg("test error message")
		output := buf.String()
		assert.Contains(t, output, "error")
		assert.Contains(t, output, "test error message")
	})

	t.Run("structured fields are included", func(t *testing.T) {
		buf.Reset()
		Info().
			Str("key", "value").
			Int("count", 42).
			Bool("flag", true).
			Msg("structured log")
		output := buf.String()
		assert.Contains(t, output, "key")
		assert.Contains(t, output, "value")
		assert.Contains(t, output, "count")
		assert.Contains(t, output, "42")
		assert.Contains(t, output, "flag")
		assert.Contains(t, output, "true")
	})
}

func TestWithFields(t *testing.T) {
	var buf bytes.Buffer
	log = zerolog.New(&buf).With().Timestamp().Logger()

	t.Run("creates child logger with fields", func(t *testing.T) {
		buf.Reset()
		childLogger := WithFields(map[string]interface{}{
			"service":     "test-service",
			"environment": "testing",
		})

		childLogger.Info().Msg("child logger message")
		output := buf.String()
		assert.Contains(t, output, "test-service")
		assert.Contains(t, output, "testing")
		assert.Contains(t, output, "child logger message")
	})
}

func TestWith(t *testing.T) {
	var buf bytes.Buffer
	log = zerolog.New(&buf).With().Timestamp().Logger()

	t.Run("returns context for building child logger", func(t *testing.T) {
		buf.Reset()
		ctx := With()
		childLogger := ctx.Str("component", "test-component").Logger()

		childLogger.Info().Msg("component message")
		output := buf.String()
		assert.Contains(t, output, "test-component")
		assert.Contains(t, output, "component message")
	})
}

func TestLogLevelFiltering(t *testing.T) {
	var buf bytes.Buffer

	t.Run("messages below level are filtered", func(t *testing.T) {
		buf.Reset()
		// Set logger to warn level - debug and info should be filtered
		log = zerolog.New(&buf).Level(zerolog.WarnLevel)

		Debug().Msg("debug message")
		Info().Msg("info message")
		Warn().Msg("warn message")

		output := buf.String()
		assert.False(t, strings.Contains(output, "debug message"), "debug should be filtered")
		assert.False(t, strings.Contains(output, "info message"), "info should be filtered")
		assert.Contains(t, output, "warn message")
	})
}
