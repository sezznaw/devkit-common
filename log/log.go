// Package log is the company standard logger, a thin layer over log/slog.
package log

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
)

type ctxKey struct{}

// Options controls how the logger is built.
type Options struct {
	// Level is one of debug, info, warn, error. Defaults to info.
	Level string `yaml:"level"`
	// JSON switches the output to JSON (recommended in production).
	JSON bool `yaml:"json"`
	// Service is attached to every record as the "service" attribute.
	Service string `yaml:"service"`
	// Output defaults to os.Stderr. Not loadable from config.
	Output io.Writer `yaml:"-"`
}

// New builds a *slog.Logger and installs it as the slog default.
func New(o Options) *slog.Logger {
	if o.Output == nil {
		o.Output = os.Stderr
	}
	h := &slog.HandlerOptions{Level: ParseLevel(o.Level)}
	var handler slog.Handler
	if o.JSON {
		handler = slog.NewJSONHandler(o.Output, h)
	} else {
		handler = slog.NewTextHandler(o.Output, h)
	}
	l := slog.New(handler)
	if o.Service != "" {
		l = l.With("service", o.Service)
	}
	slog.SetDefault(l)
	return l
}

// Named returns a child logger tagged with a component name, e.g. Named(l, "repo").
func Named(l *slog.Logger, name string) *slog.Logger {
	return l.With("component", name)
}

// WithContext stores l in ctx so code deeper in the call chain can use FromContext.
func WithContext(ctx context.Context, l *slog.Logger) context.Context {
	return context.WithValue(ctx, ctxKey{}, l)
}

// FromContext returns the logger stored by WithContext, or the slog default.
func FromContext(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(ctxKey{}).(*slog.Logger); ok {
		return l
	}
	return slog.Default()
}

// ParseLevel maps a string to a slog.Level, defaulting to info.
func ParseLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
