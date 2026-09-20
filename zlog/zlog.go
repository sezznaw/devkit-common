// Package zlog is the company standard logger, built on go.uber.org/zap.
//
// Most code uses the package-level functions, which write to the logger
// installed by Init (a colored console logger at info level until then):
//
//	zlog.Info("player login", zlog.Int("uid", uid), zlog.Str("ip", ip))
//	zlog.Error("save player failed", zlog.Int("uid", uid), zlog.Err(err))
//	zlog.Infof("player %d login from %s", uid, ip) // printf style, no fields
//
// A field is one key and one value in a bracket of their own: Str, Int, Float,
// Bool, Dur, Time, Err, Any. A missing value, a value too many or a key that
// is not a string does not compile.
//
// The two formats carry the same content, with one exception: the fields that
// identify the process (Options.Service) are in the JSON records only.
//
// Every record carries the file and line of the call. The console format
// prints them in the form IDEs and terminals turn into a link: relative to
// the working directory, like compiler errors, or absolute for code outside
// of it.
//
// A field may be named like one of the keys every record has (level, time,
// msg, caller, service): it is written as "fields.level" and so on, in both
// formats, so that a JSON record never has the same key twice.
//
// A field can be given a color for the console, zlog.Int("age", 15).Blue(),
// and so can an argument of Infof and friends, zlog.Blue(uid).
//
// L returns the underlying *zap.Logger where the full zap API is wanted.
package zlog

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync/atomic"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

const (
	FormatConsole = "console"
	FormatJSON    = "json"
)

// Options controls how a Logger is built.
type Options struct {
	// Level is one of debug, info, warn, error. Defaults to info.
	Level string `yaml:"level"`
	// Format is "console" (colored, for people; the default) or "json" (one
	// object per line, for log collectors; recommended in production).
	Format string `yaml:"format"`
	// Service is attached to every JSON record as the "service" field. The
	// console format leaves it out; see identityFields.
	Service string `yaml:"service"`
	// Output defaults to os.Stdout. Not loadable from config.
	Output io.Writer `yaml:"-"`
}

// Logger is a leveled, structured logger. There are two kinds:
//
//   - Default and the package-level With return a logger that follows the
//     default: it looks up the logger installed by Init each time it logs.
//     It is therefore safe to create one before main has called Init, which
//     is what a package-level variable does:
//
//     var log = zlog.With(zlog.Str("component", "repo"))
//
//   - New and Init return a logger of its own, as do the children made from
//     it with With. A later Init does not affect them.
//
// A Logger must not be copied; pass the pointer around.
type Logger struct {
	own *core // nil for a logger that follows the default

	// Only for a logger that follows the default: the fields added by With,
	// and the default they were last applied to.
	fields []Field
	cached atomic.Pointer[derived]
}

// core is a configured zap logger in the two forms the API needs.
type core struct {
	z       *zap.Logger // handed out by Zap and L
	z2      *zap.Logger // behind the logging functions, by way of core.log and core.logf
	level   zap.AtomicLevel
	colored bool // a console that renders colors
}

type derived struct {
	from *core // the default that c was derived from
	c    *core
}

var (
	std       atomic.Pointer[core]
	followStd = &Logger{}
)

func init() {
	std.Store(New(Options{}).own)
}

// New builds a Logger without installing it as the default.
func New(o Options) *Logger {
	if o.Output == nil {
		o.Output = os.Stdout
	}
	lv, levelOK := parseLevel(o.Level)
	level := zap.NewAtomicLevelAt(lv)

	var enc zapcore.Encoder
	var identity []zap.Field
	colored := false
	format := strings.ToLower(o.Format)
	switch format {
	case FormatJSON:
		enc = zapcore.NewJSONEncoder(jsonEncoderConfig())
		identity = identityFields(o)
	default:
		colored = colorEnabled()
		enc = newConsoleEncoder(colored)
	}

	z := zap.New(zapcore.NewCore(enc, zapcore.Lock(zapcore.AddSync(o.Output)), level), zap.AddCaller()).With(identity...)
	l := &Logger{own: wrap(z, level, colored)}

	// A typo in the config must not pass silently as "info" or "console".
	if !levelOK {
		l.Warn("zlog: unknown level, using info", Str("given", o.Level))
	}
	if format != "" && format != FormatJSON && format != FormatConsole {
		l.Warn("zlog: unknown format, using console", Str("given", o.Format))
	}
	return l
}

// identityFields say which process wrote a record. They are the one and only
// difference in content between the two formats: a collector mixes the
// records of many processes and needs them on every record, whereas the
// console of a process would repeat the same words on every line. Everything
// the code passes in, to With or to a logging call, appears in both formats.
//
// A field that is to be JSON-only belongs here and nowhere else, and its key
// belongs in fieldKey.
func identityFields(o Options) []zap.Field {
	var f []zap.Field
	if o.Service != "" {
		f = append(f, zap.String(keyService, o.Service))
	}
	return f
}

// Init builds a Logger and installs it as the one used by the package-level
// functions. Call it once at the start of main.
func Init(o Options) *Logger {
	l := New(o)
	std.Store(l.own)
	return l
}

// Default returns a logger that writes where the package-level functions do.
// It follows Init; see Logger.
func Default() *Logger { return followStd }

// wrap takes a zap logger that reports its direct caller. The frames of this
// package between the caller and zap are fixed on every path, and the skips
// account for them: two behind the logging functions (the function itself and
// core.log or core.logf), none for z.
func wrap(z *zap.Logger, level zap.AtomicLevel, colored bool) *core {
	return &core{
		z:       z,
		z2:      z.WithOptions(zap.AddCallerSkip(2)),
		level:   level,
		colored: colored,
	}
}

func (c *core) with(fields []Field) *core {
	return wrap(c.z.With(c.zap(fields)...), c.level, c.colored)
}

// log converts the fields only once the record is known to be written, so a
// call below the level costs no allocation: the caller's fields never leave
// its stack.
func (c *core) log(lv zapcore.Level, msg string, fields []Field) {
	if ce := c.z2.Check(lv, msg); ce != nil {
		ce.Write(c.zap(fields)...)
	}
}

// logf builds the message itself rather than leaving it to zap's sugared
// logger, because only here is it known whether colored arguments are to keep
// their color. format and args stay a printf pair all the way to fmt.Sprintf,
// which is what lets go vet check the calls of Infof and friends.
func (c *core) logf(lv zapcore.Level, format string, args ...any) {
	if !c.level.Enabled(lv) {
		return
	}
	// args is not assigned to: go vet gives up on a function that does.
	msg := format
	switch {
	case len(args) == 0:
	case c.colored:
		msg = fmt.Sprintf(format, args...)
	default:
		msg = fmt.Sprintf(format, plainArgs(args)...)
	}
	if ce := c.z2.Check(lv, msg); ce != nil {
		ce.Write()
	}
}

// core returns what l logs to right now. For a logger that follows the
// default, the fields are applied again only after Init has replaced it. Two
// goroutines may both do that once; the results are interchangeable.
func (l *Logger) core() *core {
	if l.own != nil {
		return l.own
	}
	cur := std.Load()
	if len(l.fields) == 0 {
		return cur
	}
	if d := l.cached.Load(); d != nil && d.from == cur {
		return d.c
	}
	d := &derived{from: cur, c: cur.with(l.fields)}
	l.cached.Store(d)
	return d.c
}

func parseLevel(s string) (zapcore.Level, bool) {
	switch strings.ToLower(s) {
	case "debug":
		return zapcore.DebugLevel, true
	case "", "info":
		return zapcore.InfoLevel, true
	case "warn", "warning":
		return zapcore.WarnLevel, true
	case "error":
		return zapcore.ErrorLevel, true
	default:
		return zapcore.InfoLevel, false
	}
}

// With returns a child logger that adds the fields to every record, e.g.
// l.With(zlog.Str("component", "repo")).
func (l *Logger) With(fields ...Field) *Logger {
	if l.own != nil {
		return &Logger{own: l.own.with(fields)}
	}
	all := make([]Field, 0, len(l.fields)+len(fields))
	return &Logger{fields: append(append(all, l.fields...), fields...)}
}

// Zap returns the underlying zap logger, for what this package does not wrap
// (Named, Check, WithOptions, ...). Fields passed to it directly are not
// renamed when they clash with a key of the record; see fieldKey. The result is a plain zap logger and cannot follow Init: ask for it where it
// is used rather than keeping it in a package-level variable.
func (l *Logger) Zap() *zap.Logger { return l.core().z }

// SetLevel changes the level at runtime. It also applies to every logger
// derived from l with With. An unknown level is ignored and reported.
func (l *Logger) SetLevel(level string) {
	lv, ok := parseLevel(level)
	if !ok {
		l.Warn("zlog: unknown level, keeping the current one", Str("given", level))
		return
	}
	l.core().level.SetLevel(lv)
}

// Sync flushes buffered records. Call it before the process exits.
func (l *Logger) Sync() error { return l.core().z.Sync() }

func (l *Logger) Debug(msg string, fields ...Field) { l.core().log(zapcore.DebugLevel, msg, fields) }
func (l *Logger) Info(msg string, fields ...Field)  { l.core().log(zapcore.InfoLevel, msg, fields) }
func (l *Logger) Warn(msg string, fields ...Field)  { l.core().log(zapcore.WarnLevel, msg, fields) }
func (l *Logger) Error(msg string, fields ...Field) { l.core().log(zapcore.ErrorLevel, msg, fields) }

// Fatal logs the record and then calls os.Exit(1).
func (l *Logger) Fatal(msg string, fields ...Field) { l.core().log(zapcore.FatalLevel, msg, fields) }

func (l *Logger) Debugf(format string, args ...any) {
	l.core().logf(zapcore.DebugLevel, format, args...)
}
func (l *Logger) Infof(format string, args ...any) {
	l.core().logf(zapcore.InfoLevel, format, args...)
}
func (l *Logger) Warnf(format string, args ...any) {
	l.core().logf(zapcore.WarnLevel, format, args...)
}
func (l *Logger) Errorf(format string, args ...any) {
	l.core().logf(zapcore.ErrorLevel, format, args...)
}

// Fatalf logs the record and then calls os.Exit(1).
func (l *Logger) Fatalf(format string, args ...any) {
	l.core().logf(zapcore.FatalLevel, format, args...)
}

// With returns a child of the default logger; see Logger.With. The child
// follows Init, so it may be created at any time, also before Init.
func With(fields ...Field) *Logger { return followStd.With(fields...) }

// L returns the default logger's underlying zap logger; see Logger.Zap.
func L() *zap.Logger { return std.Load().z }

// SetLevel changes the default logger's level at runtime; see Logger.SetLevel.
func SetLevel(level string) { followStd.SetLevel(level) }

// Sync flushes the default logger; see Logger.Sync.
func Sync() error { return std.Load().z.Sync() }

func Debug(msg string, fields ...Field) { std.Load().log(zapcore.DebugLevel, msg, fields) }
func Info(msg string, fields ...Field)  { std.Load().log(zapcore.InfoLevel, msg, fields) }
func Warn(msg string, fields ...Field)  { std.Load().log(zapcore.WarnLevel, msg, fields) }
func Error(msg string, fields ...Field) { std.Load().log(zapcore.ErrorLevel, msg, fields) }

// Fatal logs the record and then calls os.Exit(1).
func Fatal(msg string, fields ...Field) { std.Load().log(zapcore.FatalLevel, msg, fields) }

func Debugf(format string, args ...any) {
	std.Load().logf(zapcore.DebugLevel, format, args...)
}
func Infof(format string, args ...any) {
	std.Load().logf(zapcore.InfoLevel, format, args...)
}
func Warnf(format string, args ...any) {
	std.Load().logf(zapcore.WarnLevel, format, args...)
}
func Errorf(format string, args ...any) {
	std.Load().logf(zapcore.ErrorLevel, format, args...)
}

// Fatalf logs the record and then calls os.Exit(1).
func Fatalf(format string, args ...any) {
	std.Load().logf(zapcore.FatalLevel, format, args...)
}
