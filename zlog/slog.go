package zlog

import (
	"context"
	"log/slog"
	"runtime"
	"slices"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// redirectStdLoggers makes log/slog, and with it the standard library's log,
// write through the default logger. Dependencies that log with either would
// otherwise print their own format into the output: plain text in the middle
// of a JSON stream is what a collector cannot parse.
func redirectStdLoggers() {
	slog.SetDefault(slog.New(slogHandler{}))
}

// slogHandler writes slog records to the default logger, whichever that is at
// the time of the record, so it never needs to be installed again.
type slogHandler struct {
	// What WithAttrs and WithGroup have collected. A group is a zap namespace,
	// opened only once an attribute follows: slog wants empty groups left out.
	fields []zap.Field
	groups []string // opened by WithGroup, not yet written to fields
}

func (h slogHandler) Enabled(_ context.Context, lv slog.Level) bool {
	return std.Load().level.Enabled(slogLevel(lv))
}

func slogLevel(lv slog.Level) zapcore.Level {
	switch {
	case lv >= slog.LevelError:
		return zapcore.ErrorLevel
	case lv >= slog.LevelWarn:
		return zapcore.WarnLevel
	case lv >= slog.LevelInfo:
		return zapcore.InfoLevel
	default:
		return zapcore.DebugLevel
	}
}

func (h slogHandler) Handle(_ context.Context, r slog.Record) error {
	c := std.Load()
	ent := zapcore.Entry{Level: slogLevel(r.Level), Time: r.Time, Message: c.clip(r.Message)}
	if r.PC != 0 {
		f, _ := runtime.CallersFrames([]uintptr{r.PC}).Next()
		ent.Caller = zapcore.NewEntryCaller(f.PC, f.File, f.Line, f.File != "")
	}
	ce := c.z0.Core().Check(ent, nil)
	if ce == nil {
		return nil
	}
	fields, groups := slices.Clone(h.fields), h.groups
	r.Attrs(func(a slog.Attr) bool {
		fields, groups = appendAttr(c, fields, groups, a, len(h.groups) == 0 && !hasNamespace(h.fields))
		return true
	})
	ce.Write(fields...)
	return nil
}

func hasNamespace(fields []zap.Field) bool {
	for _, f := range fields {
		if f.Type == zapcore.NamespaceType {
			return true
		}
	}
	return false
}

// appendAttr adds one attribute. top says that it lands at the top level of
// the record, where its key must not clash with the record's own.
func appendAttr(c *core, fields []zap.Field, groups []string, a slog.Attr, top bool) ([]zap.Field, []string) {
	a.Value = a.Value.Resolve()
	if a.Equal(slog.Attr{}) {
		return fields, groups
	}
	if a.Value.Kind() == slog.KindGroup {
		attrs := a.Value.Group()
		if len(attrs) == 0 {
			return fields, groups
		}
		if a.Key == "" { // an inline group: its attributes belong to the parent
			for _, ga := range attrs {
				fields, groups = appendAttr(c, fields, groups, ga, top)
			}
			return fields, groups
		}
		// A group inside a record is an object of its own, which zap can only
		// express as a nested value.
		m := make(map[string]any, len(attrs))
		for _, ga := range attrs {
			if v := ga.Value.Resolve(); !ga.Equal(slog.Attr{}) {
				m[ga.Key] = v.Any()
			}
		}
		a = slog.Any(a.Key, m)
	}
	for _, g := range groups {
		fields = append(fields, zap.Namespace(g))
		top = false
	}
	groups = nil

	key := a.Key
	if top {
		key = fieldKey(key)
	}
	var z zap.Field
	switch a.Value.Kind() {
	case slog.KindString:
		z = zap.String(key, a.Value.String())
	case slog.KindInt64:
		z = zap.Int64(key, a.Value.Int64())
	case slog.KindUint64:
		z = zap.Uint64(key, a.Value.Uint64())
	case slog.KindFloat64:
		z = zap.Float64(key, a.Value.Float64())
	case slog.KindBool:
		z = zap.Bool(key, a.Value.Bool())
	case slog.KindDuration:
		z = zap.Duration(key, a.Value.Duration())
	case slog.KindTime:
		z = zap.Time(key, a.Value.Time())
	default:
		z = zap.Any(key, a.Value.Any())
	}
	if c.maxField > 0 {
		z = clipField(z, c.maxField)
	}
	return append(fields, z), groups
}

func (h slogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	c := std.Load()
	fields, groups := slices.Clone(h.fields), h.groups
	for _, a := range attrs {
		fields, groups = appendAttr(c, fields, groups, a, len(h.groups) == 0 && !hasNamespace(h.fields))
	}
	return slogHandler{fields: fields, groups: groups}
}

func (h slogHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	return slogHandler{fields: h.fields, groups: append(slices.Clone(h.groups), name)}
}
