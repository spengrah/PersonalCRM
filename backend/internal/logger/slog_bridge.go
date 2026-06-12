package logger

import (
	"context"
	"log/slog"

	"github.com/rs/zerolog"
)

// NewSlogLogger returns a *slog.Logger that writes through the given zerolog
// logger, so a dependency that only speaks slog (e.g. River) joins the app's
// structured zerolog stream instead of writing to its own default handler.
//
// Known cosmetic limitation: the app logger is built with .Caller(), so lines
// emitted through this bridge report THIS file's source location as `caller`
// rather than the original call site. The message text identifies the source
// (e.g. River's own messages), so caller-skip plumbing isn't worth it.
func NewSlogLogger(zl *zerolog.Logger) *slog.Logger {
	return slog.New(&slogZerologHandler{zl: zl})
}

// slogZerologHandler implements slog.Handler by forwarding records to a zerolog
// logger. Group prefixes are flattened into dotted keys (group.key), which is
// adequate because the bridged dependencies barely use groups.
type slogZerologHandler struct {
	zl     *zerolog.Logger
	groups []string
}

// Enabled reports whether a record at the given slog level would be emitted by
// the underlying zerolog logger. This filters a dependency's debug chatter at
// the app's configured level before any record is allocated.
func (h *slogZerologHandler) Enabled(_ context.Context, level slog.Level) bool {
	return slogToZerologLevel(level) >= h.zl.GetLevel()
}

// Handle forwards a single record to zerolog at the mapped level. It is safe to
// call even when Enabled returned false: WithLevel produces a disabled event
// below the logger's level, so nothing is written.
func (h *slogZerologHandler) Handle(_ context.Context, record slog.Record) error {
	event := h.zl.WithLevel(slogToZerologLevel(record.Level))
	if event == nil {
		return nil
	}
	record.Attrs(func(attr slog.Attr) bool {
		appendAttr(event, h.groups, attr)
		return true
	})
	// Timestamp comes from the zerolog logger's own Timestamp() hook; we do not
	// duplicate record.Time (microsecond skew is irrelevant here).
	event.Msg(record.Message)
	return nil
}

// WithAttrs returns a handler that pre-binds the given attrs into a child
// zerolog logger, qualifying their keys with the current group prefix (the
// slog.Handler contract requires attrs added after WithGroup to be scoped by
// it).
func (h *slogZerologHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	ctx := h.zl.With()
	for _, attr := range attrs {
		ctx = appendAttrToContext(ctx, h.groups, attr)
	}
	child := ctx.Logger()
	return &slogZerologHandler{zl: &child, groups: h.groups}
}

// WithGroup returns a handler that qualifies subsequent keys with the given
// group name. Per the slog contract, an empty name returns the receiver
// unchanged.
func (h *slogZerologHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	groups := make([]string, len(h.groups)+1)
	copy(groups, h.groups)
	groups[len(h.groups)] = name
	return &slogZerologHandler{zl: h.zl, groups: groups}
}

// slogToZerologLevel maps an slog level band onto a zerolog level. slog levels
// are coarser bands (Debug/Info/Warn/Error at multiples of 4), so anything
// below Info is Debug, below Warn is Info, below Error is Warn, else Error.
func slogToZerologLevel(level slog.Level) zerolog.Level {
	switch {
	case level < slog.LevelInfo:
		return zerolog.DebugLevel
	case level < slog.LevelWarn:
		return zerolog.InfoLevel
	case level < slog.LevelError:
		return zerolog.WarnLevel
	default:
		return zerolog.ErrorLevel
	}
}

// qualifyKey joins the active group prefix onto an attr key as group.subgroup.key.
func qualifyKey(groups []string, key string) string {
	if len(groups) == 0 {
		return key
	}
	out := ""
	for _, g := range groups {
		out += g + "."
	}
	return out + key
}

// appendAttr writes a single resolved attr onto a zerolog event, honoring the
// group prefix. Empty attrs (zero key and value) are elided per the slog
// contract.
func appendAttr(event *zerolog.Event, groups []string, attr slog.Attr) {
	attr.Value = attr.Value.Resolve()
	if attr.Equal(slog.Attr{}) {
		return
	}
	key := qualifyKey(groups, attr.Key)
	switch attr.Value.Kind() {
	case slog.KindString:
		event.Str(key, attr.Value.String())
	case slog.KindInt64:
		event.Int64(key, attr.Value.Int64())
	case slog.KindUint64:
		event.Uint64(key, attr.Value.Uint64())
	case slog.KindFloat64:
		event.Float64(key, attr.Value.Float64())
	case slog.KindBool:
		event.Bool(key, attr.Value.Bool())
	case slog.KindDuration:
		event.Dur(key, attr.Value.Duration())
	case slog.KindTime:
		event.Time(key, attr.Value.Time())
	default:
		if err, ok := attr.Value.Any().(error); ok {
			event.AnErr(key, err)
			return
		}
		event.Interface(key, attr.Value.Any())
	}
}

// appendAttrToContext binds a single resolved attr into a zerolog child-logger
// context (used by WithAttrs), honoring the group prefix. Mirrors appendAttr's
// kind switch.
func appendAttrToContext(ctx zerolog.Context, groups []string, attr slog.Attr) zerolog.Context {
	attr.Value = attr.Value.Resolve()
	if attr.Equal(slog.Attr{}) {
		return ctx
	}
	key := qualifyKey(groups, attr.Key)
	switch attr.Value.Kind() {
	case slog.KindString:
		return ctx.Str(key, attr.Value.String())
	case slog.KindInt64:
		return ctx.Int64(key, attr.Value.Int64())
	case slog.KindUint64:
		return ctx.Uint64(key, attr.Value.Uint64())
	case slog.KindFloat64:
		return ctx.Float64(key, attr.Value.Float64())
	case slog.KindBool:
		return ctx.Bool(key, attr.Value.Bool())
	case slog.KindDuration:
		return ctx.Dur(key, attr.Value.Duration())
	case slog.KindTime:
		return ctx.Time(key, attr.Value.Time())
	default:
		if err, ok := attr.Value.Any().(error); ok {
			return ctx.AnErr(key, err)
		}
		return ctx.Interface(key, attr.Value.Any())
	}
}
