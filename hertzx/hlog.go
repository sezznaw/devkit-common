// The records of Hertz itself ("HTTP server listening on ...", a request it
// could not parse) go through zlog like everything else, with logger=hertz:
// one format, one level, and JSON that stays JSON. This is kitexx/klog.go for
// the other framework; see there for the reasoning behind each step.

package hertzx

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/cloudwego/hertz/pkg/common/hlog"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/sezznaw/devkit-common/zlog"
)

// zlogLogger sends Hertz's own log lines through zlog. Without it a service
// writes two formats to the same stream: ours (console or JSON) and Hertz's
// "2006/01/02 15:04:05 file.go:1: [Info] KITEX: ...", which breaks JSON log
// collection, most visibly for the multi-line stack trace of a handler panic.
type zlogLogger struct {
	l *zlog.Logger
	// hlog filters by its own level before it calls us; ours is asked as well,
	// so that zlog.SetLevel reaches Hertz's lines too.
	level hlog.Level
}

// hlogFrames is what lies between the line in Hertz that logs and zap: the
// function of package hlog, the method below that it calls, and log.
const hlogFrames = 3

// bridgeHlog routes hlog to l for the whole process.
func bridgeHlog(l *zlog.Logger) {
	hlog.SetLogger(&zlogLogger{l: l.With(zlog.Str("logger", "hertz")), level: hlog.LevelTrace})
	hlog.SetLevel(hlog.LevelTrace)
}

func (s *zlogLogger) log(lv hlog.Level, msg string) {
	if lv < s.level {
		return
	}
	var zl zapcore.Level
	switch {
	case lv <= hlog.LevelDebug:
		zl = zapcore.DebugLevel
	case lv <= hlog.LevelNotice:
		zl = zapcore.InfoLevel
	case lv == hlog.LevelWarn:
		zl = zapcore.WarnLevel
	case lv == hlog.LevelFatal:
		zl = zapcore.FatalLevel
	default:
		zl = zapcore.ErrorLevel
	}
	if zl < zapcore.FatalLevel && !s.l.Enabled(zlog.Level(zl)) {
		return
	}
	// The zap logger is asked for each time: it is the one that follows
	// zlog.Init and SetLevel. It is used directly because the caller to report
	// is a line of Hertz, a fixed number of frames up.
	ce := s.l.Zap().WithOptions(zap.AddCallerSkip(hlogFrames)).Check(zl, "")
	if ce == nil {
		return
	}
	msg = strings.TrimPrefix(strings.TrimSpace(msg), "HERTZ: ")
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

func (s *zlogLogger) Trace(v ...any)  { s.log(hlog.LevelTrace, fmt.Sprint(v...)) }
func (s *zlogLogger) Debug(v ...any)  { s.log(hlog.LevelDebug, fmt.Sprint(v...)) }
func (s *zlogLogger) Info(v ...any)   { s.log(hlog.LevelInfo, fmt.Sprint(v...)) }
func (s *zlogLogger) Notice(v ...any) { s.log(hlog.LevelNotice, fmt.Sprint(v...)) }
func (s *zlogLogger) Warn(v ...any)   { s.log(hlog.LevelWarn, fmt.Sprint(v...)) }
func (s *zlogLogger) Error(v ...any)  { s.log(hlog.LevelError, fmt.Sprint(v...)) }
func (s *zlogLogger) Fatal(v ...any)  { s.log(hlog.LevelFatal, fmt.Sprint(v...)) }

func (s *zlogLogger) Tracef(f string, v ...any)  { s.log(hlog.LevelTrace, fmt.Sprintf(f, v...)) }
func (s *zlogLogger) Debugf(f string, v ...any)  { s.log(hlog.LevelDebug, fmt.Sprintf(f, v...)) }
func (s *zlogLogger) Infof(f string, v ...any)   { s.log(hlog.LevelInfo, fmt.Sprintf(f, v...)) }
func (s *zlogLogger) Noticef(f string, v ...any) { s.log(hlog.LevelNotice, fmt.Sprintf(f, v...)) }
func (s *zlogLogger) Warnf(f string, v ...any)   { s.log(hlog.LevelWarn, fmt.Sprintf(f, v...)) }
func (s *zlogLogger) Errorf(f string, v ...any)  { s.log(hlog.LevelError, fmt.Sprintf(f, v...)) }
func (s *zlogLogger) Fatalf(f string, v ...any)  { s.log(hlog.LevelFatal, fmt.Sprintf(f, v...)) }

func (s *zlogLogger) CtxTracef(_ context.Context, f string, v ...any) {
	s.log(hlog.LevelTrace, fmt.Sprintf(f, v...))
}
func (s *zlogLogger) CtxDebugf(_ context.Context, f string, v ...any) {
	s.log(hlog.LevelDebug, fmt.Sprintf(f, v...))
}
func (s *zlogLogger) CtxInfof(_ context.Context, f string, v ...any) {
	s.log(hlog.LevelInfo, fmt.Sprintf(f, v...))
}
func (s *zlogLogger) CtxNoticef(_ context.Context, f string, v ...any) {
	s.log(hlog.LevelNotice, fmt.Sprintf(f, v...))
}
func (s *zlogLogger) CtxWarnf(_ context.Context, f string, v ...any) {
	s.log(hlog.LevelWarn, fmt.Sprintf(f, v...))
}
func (s *zlogLogger) CtxErrorf(_ context.Context, f string, v ...any) {
	s.log(hlog.LevelError, fmt.Sprintf(f, v...))
}
func (s *zlogLogger) CtxFatalf(_ context.Context, f string, v ...any) {
	s.log(hlog.LevelFatal, fmt.Sprintf(f, v...))
}

func (s *zlogLogger) SetLevel(lv hlog.Level) { s.level = lv }

// SetOutput is part of hlog.Control. Output is owned by zlog.
func (s *zlogLogger) SetOutput(io.Writer) {}
