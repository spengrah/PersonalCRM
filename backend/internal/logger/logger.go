package logger

import (
	"io"
	"os"
	"strings"
	"time"

	"personal-crm/backend/internal/config"

	"github.com/rs/zerolog"
)

// Global logger instance
var log zerolog.Logger

// Init initializes the global logger with the provided configuration.
// Supported levels: trace, debug, info, warn, error, fatal, panic
func Init(cfg config.LoggerConfig) {
	logLevel := parseLogLevel(cfg.Level)

	// Use console writer for development, JSON for production
	var output io.Writer
	if cfg.Environment == "production" {
		output = os.Stdout
	} else {
		output = zerolog.ConsoleWriter{
			Out:        os.Stdout,
			TimeFormat: time.RFC3339,
		}
	}

	log = zerolog.New(output).
		Level(logLevel).
		With().
		Timestamp().
		Caller().
		Logger()
}

// parseLogLevel converts string log level to zerolog.Level
func parseLogLevel(level string) zerolog.Level {
	switch strings.ToLower(level) {
	case "trace":
		return zerolog.TraceLevel
	case "debug":
		return zerolog.DebugLevel
	case "info":
		return zerolog.InfoLevel
	case "warn", "warning":
		return zerolog.WarnLevel
	case "error":
		return zerolog.ErrorLevel
	case "fatal":
		return zerolog.FatalLevel
	case "panic":
		return zerolog.PanicLevel
	default:
		return zerolog.InfoLevel
	}
}

// Get returns the global logger instance
func Get() *zerolog.Logger {
	return &log
}

// Debug returns a debug level event
func Debug() *zerolog.Event {
	return log.Debug()
}

// Info returns an info level event
func Info() *zerolog.Event {
	return log.Info()
}

// Warn returns a warn level event
func Warn() *zerolog.Event {
	return log.Warn()
}

// Error returns an error level event
func Error() *zerolog.Event {
	return log.Error()
}

// Fatal returns a fatal level event
func Fatal() *zerolog.Event {
	return log.Fatal()
}

// Panic returns a panic level event
func Panic() *zerolog.Event {
	return log.Panic()
}

// With creates a child logger with additional context
func With() zerolog.Context {
	return log.With()
}

// WithFields creates a child logger with structured fields
func WithFields(fields map[string]interface{}) zerolog.Logger {
	ctx := log.With()
	for k, v := range fields {
		ctx = ctx.Interface(k, v)
	}
	return ctx.Logger()
}

