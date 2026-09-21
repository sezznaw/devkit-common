package kitexx

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/cloudwego/kitex/pkg/klog"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/sezznaw/devkit-common/zlog"
)

// zlogLogger sends Kitex's own log lines through zlog. Without it a service
// writes two formats to the same stream: ours (console or JSON) and Kitex's
// "2006/01/02 15:04:05 file.go:1: [Info] KITEX: ...", which breaks JSON log
// collection, most visibly for the multi-line stack trace of a handler panic.
type zlogLogger struct {
	l *zlog.Logger
	// klog filters by its own level before it calls us; ours is asked as well,
	// so that zlog.SetLevel reaches Kitex's lines too.
	level klog.Level
}

// klogFrames is what lies between the line in Kitex that logs and zap: the
// function of package klog, the method below that it calls, and log.
const klogFrames = 3

// bridgeKlog routes klog to l for the whole process.
func bridgeKlog(l *zlog.Logger) {
	klog.SetLogger(&zlogLogger{l: l.With(zlog.Str("logger", "kitex")), level: klog.LevelTrace})
	klog.SetLevel(klog.LevelTrace)
}

func (s *zlogLogger) log(lv klog.Level, msg string) {
	if lv < s.level {
		return
	}
	var zl zapcore.Level
	switch {
	case lv <= klog.LevelDebug:
		zl = zapcore.DebugLevel
	case lv <= klog.LevelNotice:
		zl = zapcore.InfoLevel
	case lv == klog.LevelWarn:
		zl = zapcore.WarnLevel
	case lv == klog.LevelFatal:
		zl = zapcore.FatalLevel
	default:
		zl = zapcore.ErrorLevel
	}
	if zl < zapcore.FatalLevel && !s.l.Enabled(zlog.Level(zl)) {
		return
	}
	// The zap logger is asked for each time: it is the one that follows
	// zlog.Init and SetLevel. It is used directly because the caller to report
	// is a line of Kitex, a fixed number of frames up.
	ce := s.l.Zap().WithOptions(zap.AddCallerSkip(klogFrames)).Check(zl, "")
	if ce == nil {
		return
	}
	msg = strings.TrimPrefix(strings.TrimSpace(msg), "KITEX: ")
	// A panic report is "message\nstack...": keep the first line as the message
	// and carry the rest as one field, so it stays a single record.
	if head, stack, ok := strings.Cut(msg, "\n"); ok {
		ce.Message = head
		ce.Write(zap.String("stack", stack))
	} else {
		ce.Message = msg
		ce.Write()
	}
}

func (s *zlogLogger) Trace(v ...any)  { s.log(klog.LevelTrace, fmt.Sprint(v...)) }
func (s *zlogLogger) Debug(v ...any)  { s.log(klog.LevelDebug, fmt.Sprint(v...)) }
func (s *zlogLogger) Info(v ...any)   { s.log(klog.LevelInfo, fmt.Sprint(v...)) }
func (s *zlogLogger) Notice(v ...any) { s.log(klog.LevelNotice, fmt.Sprint(v...)) }
func (s *zlogLogger) Warn(v ...any)   { s.log(klog.LevelWarn, fmt.Sprint(v...)) }
func (s *zlogLogger) Error(v ...any)  { s.log(klog.LevelError, fmt.Sprint(v...)) }
func (s *zlogLogger) Fatal(v ...any)  { s.log(klog.LevelFatal, fmt.Sprint(v...)) }

func (s *zlogLogger) Tracef(f string, v ...any)  { s.log(klog.LevelTrace, fmt.Sprintf(f, v...)) }
func (s *zlogLogger) Debugf(f string, v ...any)  { s.log(klog.LevelDebug, fmt.Sprintf(f, v...)) }
func (s *zlogLogger) Infof(f string, v ...any)   { s.log(klog.LevelInfo, fmt.Sprintf(f, v...)) }
func (s *zlogLogger) Noticef(f string, v ...any) { s.log(klog.LevelNotice, fmt.Sprintf(f, v...)) }
func (s *zlogLogger) Warnf(f string, v ...any)   { s.log(klog.LevelWarn, fmt.Sprintf(f, v...)) }
func (s *zlogLogger) Errorf(f string, v ...any)  { s.log(klog.LevelError, fmt.Sprintf(f, v...)) }
func (s *zlogLogger) Fatalf(f string, v ...any)  { s.log(klog.LevelFatal, fmt.Sprintf(f, v...)) }

func (s *zlogLogger) CtxTracef(_ context.Context, f string, v ...any) {
	s.log(klog.LevelTrace, fmt.Sprintf(f, v...))
}
func (s *zlogLogger) CtxDebugf(_ context.Context, f string, v ...any) {
	s.log(klog.LevelDebug, fmt.Sprintf(f, v...))
}
func (s *zlogLogger) CtxInfof(_ context.Context, f string, v ...any) {
	s.log(klog.LevelInfo, fmt.Sprintf(f, v...))
}
func (s *zlogLogger) CtxNoticef(_ context.Context, f string, v ...any) {
	s.log(klog.LevelNotice, fmt.Sprintf(f, v...))
}
func (s *zlogLogger) CtxWarnf(_ context.Context, f string, v ...any) {
	s.log(klog.LevelWarn, fmt.Sprintf(f, v...))
}
func (s *zlogLogger) CtxErrorf(_ context.Context, f string, v ...any) {
	s.log(klog.LevelError, fmt.Sprintf(f, v...))
}
func (s *zlogLogger) CtxFatalf(_ context.Context, f string, v ...any) {
	s.log(klog.LevelFatal, fmt.Sprintf(f, v...))
}

func (s *zlogLogger) SetLevel(lv klog.Level) { s.level = lv }

// SetOutput is part of klog.Control. Output is owned by zlog.
func (s *zlogLogger) SetOutput(io.Writer) {}
