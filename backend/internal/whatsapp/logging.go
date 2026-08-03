package whatsapp

import (
	"fmt"

	"personal-crm/backend/internal/logger"

	waLog "go.mau.fi/whatsmeow/util/log"
)

// waLogger adapts the repo's zerolog logger to whatsmeow's waLog.Logger
// interface. whatsmeow is chatty at debug level, so its Debugf is routed to
// zerolog Debug — production runs at LOG_LEVEL=warn and therefore stays silent.
type waLogger struct {
	// module is the dotted path of Sub() calls, e.g. "whatsapp.Client.Recv".
	module string
}

// newWALogger returns the whatsmeow logger for a top-level module. Every line
// it emits is prefixed "whatsapp:" so WhatsApp traffic is greppable in a log
// stream shared with the rest of the backend.
func newWALogger(module string) waLog.Logger {
	return &waLogger{module: module}
}

var _ waLog.Logger = (*waLogger)(nil)

// prefix renders "whatsapp: <module>: " for a sub-logger, or "whatsapp: " when
// the module is the package root itself.
func (l *waLogger) prefix() string {
	if l.module == "" || l.module == "whatsapp" {
		return "whatsapp: "
	}
	return "whatsapp: " + l.module + ": "
}

func (l *waLogger) render(msg string, args ...any) string {
	if len(args) == 0 {
		return l.prefix() + msg
	}
	return l.prefix() + fmt.Sprintf(msg, args...)
}

func (l *waLogger) Warnf(msg string, args ...any) {
	logger.Warn().Msg(l.render(msg, args...))
}

func (l *waLogger) Errorf(msg string, args ...any) {
	logger.Error().Msg(l.render(msg, args...))
}

func (l *waLogger) Infof(msg string, args ...any) {
	logger.Info().Msg(l.render(msg, args...))
}

func (l *waLogger) Debugf(msg string, args ...any) {
	logger.Debug().Msg(l.render(msg, args...))
}

// Sub returns a logger for a nested module. whatsmeow calls this for each of
// its internal subsystems, so the module names accumulate rather than replace:
// losing the parent would make a nested line indistinguishable from a top-level
// one.
func (l *waLogger) Sub(module string) waLog.Logger {
	switch {
	case module == "":
		return l
	case l.module == "":
		return &waLogger{module: module}
	default:
		return &waLogger{module: l.module + "." + module}
	}
}
