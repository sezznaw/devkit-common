package kitexx

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/cloudwego/kitex/pkg/klog"
)

// slogLogger sends Kitex's own log lines through slog. Without it a service
// writes two formats to the same stream: ours (text or JSON) and Kitex's
// "2006/01/02 15:04:05 file.go:1: [Info] KITEX: ...", which breaks JSON log
// collection, most visibly for the multi-line stack trace of a handler panic.
type slogLogger struct {
	l     *slog.Logger
	level klog.Level
}

// bridgeKlog routes klog to l for the whole process.
func bridgeKlog(l *slog.Logger, level slog.Level) {
	kl := &slogLogger{l: l.With("logger", "kitex")}
	switch {
	case level <= slog.LevelDebug:
		kl.level = klog.LevelDebug
	case level <= slog.LevelInfo:
		kl.level = klog.LevelInfo
	case level <= slog.LevelWarn:
		kl.level = klog.LevelWarn
	default:
		kl.level = klog.LevelError
	}
	klog.SetLogger(kl)
	klog.SetLevel(kl.level)
}

func (s *slogLogger) log(ctx context.Context, lv klog.Level, msg string) {
	if lv < s.level {
		return
	}
	msg = strings.TrimPrefix(strings.TrimSpace(msg), "KITEX: ")
	var sl slog.Level
	switch {
	case lv <= klog.LevelDebug:
		sl = slog.LevelDebug
	case lv <= klog.LevelNotice:
		sl = slog.LevelInfo
	case lv == klog.LevelWarn:
		sl = slog.LevelWarn
	default:
		sl = slog.LevelError
	}
	// A panic report is "message\nstack...": keep the first line as the message
	// and carry the rest as one attribute, so it stays a single record.
	if head, stack, ok := strings.Cut(msg, "\n"); ok {
		s.l.Log(ctx, sl, head, "stack", stack)
	} else {
		s.l.Log(ctx, sl, msg)
	}
	if lv == klog.LevelFatal {
		os.Exit(1)
	}
}

func (s *slogLogger) Trace(v ...any) { s.log(context.Background(), klog.LevelTrace, fmt.Sprint(v...)) }
func (s *slogLogger) Debug(v ...any) { s.log(context.Background(), klog.LevelDebug, fmt.Sprint(v...)) }
func (s *slogLogger) Info(v ...any)  { s.log(context.Background(), klog.LevelInfo, fmt.Sprint(v...)) }
func (s *slogLogger) Notice(v ...any) {
	s.log(context.Background(), klog.LevelNotice, fmt.Sprint(v...))
}
func (s *slogLogger) Warn(v ...any)  { s.log(context.Background(), klog.LevelWarn, fmt.Sprint(v...)) }
func (s *slogLogger) Error(v ...any) { s.log(context.Background(), klog.LevelError, fmt.Sprint(v...)) }
func (s *slogLogger) Fatal(v ...any) { s.log(context.Background(), klog.LevelFatal, fmt.Sprint(v...)) }

func (s *slogLogger) Tracef(f string, v ...any) {
	s.log(context.Background(), klog.LevelTrace, fmt.Sprintf(f, v...))
}
func (s *slogLogger) Debugf(f string, v ...any) {
	s.log(context.Background(), klog.LevelDebug, fmt.Sprintf(f, v...))
}
func (s *slogLogger) Infof(f string, v ...any) {
	s.log(context.Background(), klog.LevelInfo, fmt.Sprintf(f, v...))
}
func (s *slogLogger) Noticef(f string, v ...any) {
	s.log(context.Background(), klog.LevelNotice, fmt.Sprintf(f, v...))
}
func (s *slogLogger) Warnf(f string, v ...any) {
	s.log(context.Background(), klog.LevelWarn, fmt.Sprintf(f, v...))
}
func (s *slogLogger) Errorf(f string, v ...any) {
	s.log(context.Background(), klog.LevelError, fmt.Sprintf(f, v...))
}
func (s *slogLogger) Fatalf(f string, v ...any) {
	s.log(context.Background(), klog.LevelFatal, fmt.Sprintf(f, v...))
}

func (s *slogLogger) CtxTracef(ctx context.Context, f string, v ...any) {
	s.log(ctx, klog.LevelTrace, fmt.Sprintf(f, v...))
}
func (s *slogLogger) CtxDebugf(ctx context.Context, f string, v ...any) {
	s.log(ctx, klog.LevelDebug, fmt.Sprintf(f, v...))
}
func (s *slogLogger) CtxInfof(ctx context.Context, f string, v ...any) {
	s.log(ctx, klog.LevelInfo, fmt.Sprintf(f, v...))
}
func (s *slogLogger) CtxNoticef(ctx context.Context, f string, v ...any) {
	s.log(ctx, klog.LevelNotice, fmt.Sprintf(f, v...))
}
func (s *slogLogger) CtxWarnf(ctx context.Context, f string, v ...any) {
	s.log(ctx, klog.LevelWarn, fmt.Sprintf(f, v...))
}
func (s *slogLogger) CtxErrorf(ctx context.Context, f string, v ...any) {
	s.log(ctx, klog.LevelError, fmt.Sprintf(f, v...))
}
func (s *slogLogger) CtxFatalf(ctx context.Context, f string, v ...any) {
	s.log(ctx, klog.LevelFatal, fmt.Sprintf(f, v...))
}

func (s *slogLogger) SetLevel(lv klog.Level) { s.level = lv }

// SetOutput is part of klog.Control. Output is owned by the slog handler.
func (s *slogLogger) SetOutput(io.Writer) {}
