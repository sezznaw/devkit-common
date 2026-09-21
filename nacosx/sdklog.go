package nacosx

import (
	"fmt"
	"strings"
	"sync"

	sdklogger "github.com/nacos-group/nacos-sdk-go/v2/common/logger"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/sezznaw/devkit-common/zlog"
)

// sdkLogger sends the Nacos SDK's own records through zlog. Left alone, the
// SDK writes them to a file of its own (nacos-sdk.log), where nobody looks:
// "fail to connect server ... connection refused" is then not in the output of
// the service that cannot connect.
type sdkLogger struct {
	l *zlog.Logger
	// min is the SDK's own threshold, on top of the level of zlog: see
	// Config.SDKLogLevel for why it is warn unless asked otherwise.
	min zapcore.Level
}

// routine are records the SDK writes at warn about what is normal, several of
// them with every read of a configuration. They are taken down to debug: at
// warn they would bury the records that do mean something.
var routine = []string{
	// There is no failover file unless somebody puts one there by hand.
	"read Config Content failed. cause file doesn't exist",
	// Nor a key file for a configuration that is not encrypted.
	"Config Encrypted Data Key failed. cause file doesn't exist",
	// A configuration that does not exist: whoever asked for it says so,
	// in terms of the data id, or has a reason not to mind.
	"config data not exist",
	// A service without instances: "instances changed: none can take
	// requests" is our record of it.
	"instance list is empty, updateCacheWhenEmpty is set to false",
}

// sdkFrames is what lies between the line in the SDK that logs and zap: the
// function of the SDK's logger package, the method below that it calls, and
// log.
const sdkFrames = 3

var bridgeOnce sync.Once

// bridgeSDKLog routes the SDK's logger to zlog for the whole process. The
// SDK's logger is one per process, so the level of the first client counts.
// It has to happen before the first SDK client is created: creating one
// installs the file logger unless a logger is already there.
func bridgeSDKLog(level string) {
	bridgeOnce.Do(func() {
		min := zapcore.WarnLevel
		switch strings.ToLower(strings.TrimSpace(level)) {
		case "debug":
			min = zapcore.DebugLevel
		case "info":
			min = zapcore.InfoLevel
		case "error":
			min = zapcore.ErrorLevel
		case "", "warn", "warning":
		default:
			zlog.Warn("nacos.sdk_log_level is not one of debug, info, warn, error; using warn", zlog.Str("value", level))
		}
		sdklogger.SetLogger(&sdkLogger{l: zlog.With(zlog.Str("logger", "nacos-sdk")), min: min})
	})
}

func (s *sdkLogger) log(lv zapcore.Level, msg string) {
	if lv == zapcore.WarnLevel {
		for _, r := range routine {
			if strings.Contains(msg, r) {
				lv = zapcore.DebugLevel
				break
			}
		}
	}
	if lv < s.min || !s.l.Enabled(zlog.Level(lv)) {
		return
	}
	// At debug everything is wanted, the SDK's account of an outage included.
	if lv >= zapcore.WarnLevel && s.min > zapcore.DebugLevel && outage.swallow(msg) {
		return
	}
	// The zap logger is asked for each time: it is the one that follows
	// zlog.Init and SetLevel. It is used directly because the caller to report
	// is a line of the SDK, a fixed number of frames up.
	ce := s.l.Zap().WithOptions(zap.AddCallerSkip(sdkFrames)).Check(lv, "")
	if ce == nil {
		return
	}
	// Errors formatted with %+v bring their stack: keep the first line as the
	// message and carry the rest as one field, so it stays a single record.
	if head, rest, ok := strings.Cut(strings.TrimSpace(msg), "\n"); ok {
		ce.Message = head
		ce.Write(zap.String("detail", rest))
	} else {
		ce.Message = head
		ce.Write()
	}
}

func (s *sdkLogger) Debug(v ...any) { s.log(zapcore.DebugLevel, fmt.Sprint(v...)) }
func (s *sdkLogger) Info(v ...any)  { s.log(zapcore.InfoLevel, fmt.Sprint(v...)) }
func (s *sdkLogger) Warn(v ...any)  { s.log(zapcore.WarnLevel, fmt.Sprint(v...)) }
func (s *sdkLogger) Error(v ...any) { s.log(zapcore.ErrorLevel, fmt.Sprint(v...)) }

func (s *sdkLogger) Debugf(f string, v ...any) { s.log(zapcore.DebugLevel, fmt.Sprintf(f, v...)) }
func (s *sdkLogger) Infof(f string, v ...any)  { s.log(zapcore.InfoLevel, fmt.Sprintf(f, v...)) }
func (s *sdkLogger) Warnf(f string, v ...any)  { s.log(zapcore.WarnLevel, fmt.Sprintf(f, v...)) }
func (s *sdkLogger) Errorf(f string, v ...any) { s.log(zapcore.ErrorLevel, fmt.Sprintf(f, v...)) }

// Close is part of the SDK's Logger interface. The output belongs to zlog.
func (s *sdkLogger) Close() error { return nil }
