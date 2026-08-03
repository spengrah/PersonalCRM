package whatsapp

import (
	"bytes"
	"testing"

	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/logger"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	waLog "go.mau.fi/whatsmeow/util/log"
)

// captureLogs redirects the global logger to a buffer for the duration of the
// test, at debug level so every whatsmeow level is observable.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	logger.Init(config.LoggerConfig{Level: "debug", Environment: "production"})
	var buf bytes.Buffer
	restore := logger.SetOutput(&buf)
	t.Cleanup(restore)
	return &buf
}

// TestWALogger_SatisfiesWhatsmeowInterface is the compile-time seam check the
// plan names: whatsmeow accepts only its own tiny logger interface, and a
// mismatch is a build failure rather than a runtime one.
func TestWALogger_SatisfiesWhatsmeowInterface(t *testing.T) {
	l := newWALogger("whatsapp")
	require.NotNil(t, l)
	require.Implements(t, (*waLog.Logger)(nil), l)
}

func TestWALogger_SubCarriesBothPrefixes(t *testing.T) {
	buf := captureLogs(t)

	root := newWALogger("whatsapp")
	sub := root.Sub("Client")
	require.NotNil(t, sub)

	sub.Infof("connected as %s", "device-1")

	out := buf.String()
	assert.Contains(t, out, "whatsapp: ", "every line carries the package prefix")
	assert.Contains(t, out, "Client", "the sub-module name is preserved")
	assert.Contains(t, out, "connected as device-1", "format args are applied")
}

func TestWALogger_SubNestsRatherThanReplaces(t *testing.T) {
	buf := captureLogs(t)

	newWALogger("whatsapp").Sub("Client").Sub("Recv").Warnf("stanza dropped")

	out := buf.String()
	assert.Contains(t, out, "Client.Recv",
		"nested Sub() calls accumulate, so a nested line is distinguishable from a top-level one")
}

func TestWALogger_LevelsMapToZerologLevels(t *testing.T) {
	tests := []struct {
		name      string
		emit      func(waLog.Logger)
		wantLevel string
	}{
		{"debug", func(l waLog.Logger) { l.Debugf("d") }, `"level":"debug"`},
		{"info", func(l waLog.Logger) { l.Infof("i") }, `"level":"info"`},
		{"warn", func(l waLog.Logger) { l.Warnf("w") }, `"level":"warn"`},
		{"error", func(l waLog.Logger) { l.Errorf("e") }, `"level":"error"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			buf := captureLogs(t)
			tt.emit(newWALogger("whatsapp"))
			assert.Contains(t, buf.String(), tt.wantLevel,
				"whatsmeow debug must land on zerolog debug so LOG_LEVEL=warn silences it in production")
		})
	}
}
