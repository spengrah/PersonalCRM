package logger

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newBufferSlog builds a slog.Logger bridged to a zerolog logger writing JSON
// into the returned buffer, at the given zerolog level. Parallel-safe: no
// global mutation, each test owns its own buffer + logger.
func newBufferSlog(level zerolog.Level) (*slog.Logger, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	zl := zerolog.New(buf).Level(level).With().Timestamp().Logger()
	return NewSlogLogger(&zl), buf
}

// decode parses the single JSON log line in the buffer into a map.
func decode(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	require.NotEmpty(t, buf.Bytes(), "expected a log line")
	var m map[string]any
	require.NoError(t, json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &m))
	return m
}

func TestSlogToZerologLevel(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   slog.Level
		want zerolog.Level
	}{
		{"debug band", slog.LevelDebug, zerolog.DebugLevel},
		{"below info still debug", slog.LevelInfo - 1, zerolog.DebugLevel},
		{"info band", slog.LevelInfo, zerolog.InfoLevel},
		{"below warn is info", slog.LevelWarn - 1, zerolog.InfoLevel},
		{"warn band", slog.LevelWarn, zerolog.WarnLevel},
		{"below error is warn", slog.LevelError - 1, zerolog.WarnLevel},
		{"error band", slog.LevelError, zerolog.ErrorLevel},
		{"above error is error", slog.LevelError + 4, zerolog.ErrorLevel},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, c.want, slogToZerologLevel(c.in))
		})
	}
}

func TestSlogBridgeLevelRoundTrip(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		log      func(l *slog.Logger)
		wantZLvl string
	}{
		{"debug", func(l *slog.Logger) { l.Debug("m") }, "debug"},
		{"info", func(l *slog.Logger) { l.Info("m") }, "info"},
		{"warn", func(l *slog.Logger) { l.Warn("m") }, "warn"},
		{"error", func(l *slog.Logger) { l.Error("m") }, "error"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			l, buf := newBufferSlog(zerolog.DebugLevel)
			c.log(l)
			m := decode(t, buf)
			assert.Equal(t, c.wantZLvl, m["level"])
			assert.Equal(t, "m", m["message"])
		})
	}
}

func TestSlogBridgeAttrKinds(t *testing.T) {
	t.Parallel()
	l, buf := newBufferSlog(zerolog.DebugLevel)
	l.Info("structured",
		slog.String("s", "hello"),
		slog.Int("n", 42),
		slog.Bool("flag", true),
		slog.Duration("d", 1500*time.Millisecond),
		slog.Any("err", errors.New("boom")),
	)
	m := decode(t, buf)
	assert.Equal(t, "hello", m["s"])
	assert.Equal(t, float64(42), m["n"])
	assert.Equal(t, true, m["flag"])
	// zerolog Dur defaults to milliseconds.
	assert.Equal(t, float64(1500), m["d"])
	assert.Equal(t, "boom", m["err"])
}

func TestSlogBridgeWithAttrsPersist(t *testing.T) {
	t.Parallel()
	l, buf := newBufferSlog(zerolog.DebugLevel)
	child := l.With(slog.String("component", "river"))

	child.Info("first")
	m := decode(t, buf)
	assert.Equal(t, "river", m["component"])

	// The bound attr persists across calls.
	buf.Reset()
	child.Warn("second")
	m = decode(t, buf)
	assert.Equal(t, "river", m["component"])
	assert.Equal(t, "warn", m["level"])
}

func TestSlogBridgeWithGroupDottedKeys(t *testing.T) {
	t.Parallel()
	l, buf := newBufferSlog(zerolog.DebugLevel)
	l.WithGroup("river").Info("grouped", slog.String("queue", "default"))
	m := decode(t, buf)
	assert.Equal(t, "default", m["river.queue"])
	_, bare := m["queue"]
	assert.False(t, bare, "key should be group-qualified, not bare")
}

func TestSlogBridgeWithGroupThenWithAttrs(t *testing.T) {
	t.Parallel()
	// The contract: attrs bound after WithGroup carry the group prefix.
	l, buf := newBufferSlog(zerolog.DebugLevel)
	l.WithGroup("river").With(slog.Int("attempt", 3)).Info("m")
	m := decode(t, buf)
	assert.Equal(t, float64(3), m["river.attempt"])
}

func TestSlogBridgeWithEmptyGroupIsNoOp(t *testing.T) {
	t.Parallel()
	l, buf := newBufferSlog(zerolog.DebugLevel)
	l.WithGroup("").Info("m", slog.String("k", "v"))
	m := decode(t, buf)
	// No group prefix applied.
	assert.Equal(t, "v", m["k"])
}

func TestSlogBridgeEnabledFiltering(t *testing.T) {
	t.Parallel()
	// Debug record against an info-level logger emits nothing.
	l, buf := newBufferSlog(zerolog.InfoLevel)
	l.Debug("should be filtered", slog.String("k", "v"))
	assert.Empty(t, buf.Bytes(), "debug below info level must not be written")

	// Info passes.
	l.Info("should pass")
	assert.NotEmpty(t, buf.Bytes())
}

// resolvable exercises the LogValuer resolution path.
type resolvable struct{ inner string }

func (r resolvable) LogValue() slog.Value { return slog.StringValue(r.inner) }

func TestSlogBridgeLogValuerResolves(t *testing.T) {
	t.Parallel()
	l, buf := newBufferSlog(zerolog.DebugLevel)
	l.Info("m", slog.Any("v", resolvable{inner: "resolved"}))
	m := decode(t, buf)
	assert.Equal(t, "resolved", m["v"])
}

func TestSlogBridgeUint64AndFloatAndTime(t *testing.T) {
	t.Parallel()
	l, buf := newBufferSlog(zerolog.DebugLevel)
	ts := time.Date(2026, 6, 12, 1, 2, 3, 0, time.UTC)
	l.Info("m",
		slog.Uint64("u", 18446744073709551615),
		slog.Float64("f", 3.5),
		slog.Time("t", ts),
	)
	m := decode(t, buf)
	// Large uint64 round-trips via JSON number.
	assert.Equal(t, float64(18446744073709551615), m["u"])
	assert.Equal(t, 3.5, m["f"])
	// zerolog default time format is RFC3339.
	assert.Equal(t, ts.Format(time.RFC3339), m["t"])
}
