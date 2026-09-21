// Package zlog is the company standard logger, built on go.uber.org/zap.
//
// Most code uses the package-level functions, which write to the logger
// installed by Init (a colored console logger at info level until then):
//
//	zlog.Info("player login", zlog.Int("uid", uid), zlog.Str("ip", ip))
//	zlog.Error("save player failed", zlog.Int("uid", uid), zlog.Err(err))
//	zlog.Infof("player %d login from %s", uid, ip) // printf style, no fields
//	zlog.Ctx(ctx).Info("saved")                    // with the fields of the request
//
// A field is one key and one value in a bracket of their own: Str, Int, Float,
// Bool, Dur, Time, Err, Any, Secret. A missing value, a value too many or a
// key that is not a string does not compile.
//
// The two formats carry the same content, with one exception: the fields that
// identify the process (service, env, host, version) are in the JSON records
// only.
//
// Every record carries the file and line of the call. The console format
// prints them in the form IDEs and terminals turn into a link: relative to
// the working directory, like compiler errors, or absolute for code outside
// of it.
//
// A JSON record never has the same key twice. A field named like one of the
// keys every record has (level, time, msg, caller, ...) is written as
// "fields.level" and so on, in both formats. Of two fields with the same key
// the later one is kept: a field of the call replaces the one given to With.
//
// A field can be given a color for the console, zlog.Int("age", 15).Blue(),
// and so can an argument of Infof and friends, zlog.Blue(uid).
//
// Init also routes the standard library's log and log/slog here, so that what
// dependencies print is in the same format.
//
// L returns the underlying *zap.Logger where the full zap API is wanted.
package zlog

import (
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

const (
	FormatConsole = "console"
	FormatJSON    = "json"
)

// Level is the severity of a record, for Enabled.
type Level int8

const (
	LevelDebug = Level(zapcore.DebugLevel)
	LevelInfo  = Level(zapcore.InfoLevel)
	LevelWarn  = Level(zapcore.WarnLevel)
	LevelError = Level(zapcore.ErrorLevel)
)

// Options controls how a Logger is built. Only Level and Format matter to
// most services; the zero value of everything else is a sensible default.
type Options struct {
	// Level is one of debug, info, warn, error. Defaults to info.
	Level string `yaml:"level"`
	// Format is "console" (colored, for people; the default) or "json" (one
	// object per line, for log collectors; recommended in production).
	Format string `yaml:"format"`
	// JSON is what the log package before zlog had instead of Format.
	//
	// Deprecated: use Format. It is still read, because a configuration file
	// that says "json: true" would otherwise turn into console output in
	// production without anybody noticing.
	JSON bool `yaml:"json"`

	// Service, Env and Version identify the process on every JSON record, next
	// to "host", which is the host name (the pod name in Kubernetes). The
	// console format leaves them out; see identityFields. An empty Version
	// falls back to the VCS revision the binary was built from.
	Service string `yaml:"service"`
	Env     string `yaml:"env"`
	Version string `yaml:"version"`
	// ServiceNote and EnvNote are for code that fills in Service or Env instead
	// of the configuration file, as kitexx does: a few words on where the value
	// is from, shown behind it by the "logger configured" record of Init, e.g.
	// "from APP_ENV". Not loadable from config.
	ServiceNote string `yaml:"-"`
	EnvNote     string `yaml:"-"`

	// Stacktrace is the level from which records carry the stack of the call,
	// usually "error". Empty means never. On the console every line of the
	// stack is a link.
	Stacktrace string `yaml:"stacktrace"`

	// Sampling protects disks and collectors from a flood of one and the same
	// record. Off unless First is set.
	Sampling Sampling `yaml:"sampling"`
	// Buffer collects output in memory and writes it in larger pieces. Off
	// unless Size is set.
	Buffer Buffer `yaml:"buffer"`
	// MaxFieldBytes cuts the message and every field value that is longer.
	// Container runtimes split lines of more than 16 KB, after which a
	// collector no longer sees one JSON object. 0 means no limit.
	MaxFieldBytes int `yaml:"max_field_bytes"`

	// Output defaults to os.Stdout. Not loadable from config.
	Output io.Writer `yaml:"-"`
}

// Sampling: within every second, records with the same level and message are
// written First times, and after that only every Thereafter-th one. The
// message of Infof and friends is the formatted one, so records that differ in
// their arguments are different records.
//
// What is left out is counted: for every second with drops there is one record
// per level and message, "zlog: records dropped by sampling" with the fields
// dropped_msg and dropped, so that the size of a flood can be read from the
// log. Sync writes the count of the current second at once.
type Sampling struct {
	First      int `yaml:"first"`
	Thereafter int `yaml:"thereafter"`
}

// Buffer: up to Size bytes wait in memory for at most FlushInterval (default
// 1s). Records of level Fatal are flushed at once, and Sync flushes; what is
// lost is the last interval when the process is killed.
type Buffer struct {
	Size          int           `yaml:"size"`
	FlushInterval time.Duration `yaml:"flush_interval"`
}

// Logger is a leveled, structured logger. There are two kinds:
//
//   - Default, Ctx and the package-level With return a logger that follows the
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

// core is a configured zap logger and the fields added with With. The fields
// are not handed to zap's own With, which would encode them for good: a field
// of a call has to be able to replace one of them.
type core struct {
	z0       *zap.Logger // reports its direct caller; the base of Zap and L
	z2       *zap.Logger // behind the logging functions, by way of core.log and core.logf
	level    zap.AtomicLevel
	colored  bool // a console that renders colors
	maxField int
	stop     func()        // reports pending drops; flushes and stops the buffer, if there is one
	drops    *dropReporter // nil without sampling
	config   []zap.Field   // what Init announces: the settings in effect

	added     []zap.Field // the fields of With, ready to be written
	addedKeys []string    // the keys they were given, added[i] under addedKeys[i]
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
	std.Store(New(envOptions()).own)
}

// envOptions configures the logger that is in place until Init is called. A
// service logs before that when it cannot read its configuration, and that is
// the line a collector must not fail to parse: deployments set
// ZLOG_FORMAT=json.
func envOptions() Options {
	return Options{
		Level:   os.Getenv("ZLOG_LEVEL"),
		Format:  os.Getenv("ZLOG_FORMAT"),
		Service: os.Getenv("ZLOG_SERVICE"),
		Env:     os.Getenv("ZLOG_ENV"),
	}
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
	if format == "" && o.JSON {
		format = FormatJSON
	}
	switch format {
	case FormatJSON:
		enc = zapcore.NewJSONEncoder(jsonEncoderConfig())
		identity = identityFields(o)
	default:
		colored = colorEnabled()
		enc = newConsoleEncoder(colored)
	}

	var ws zapcore.WriteSyncer
	var stop func()
	if o.Buffer.Size > 0 {
		b := &zapcore.BufferedWriteSyncer{WS: zapcore.AddSync(o.Output), Size: o.Buffer.Size, FlushInterval: o.Buffer.FlushInterval}
		if b.FlushInterval <= 0 {
			b.FlushInterval = time.Second
		}
		ws, stop = b, func() { _ = b.Stop() }
	} else {
		ws = zapcore.Lock(zapcore.AddSync(o.Output))
	}

	zc := zapcore.NewCore(enc, ws, level)
	var drops *dropReporter
	if o.Sampling.First > 0 {
		// The reports carry the identity fields like every other record.
		drops = &dropReporter{out: zc.With(identity)}
		zc = zapcore.NewSamplerWithOptions(zc, dropWindow, o.Sampling.First, o.Sampling.Thereafter, zapcore.SamplerHook(drops.hook))
		flushBuffer := stop
		stop = func() { // first the report, then the buffer it may sit in
			drops.flush()
			if flushBuffer != nil {
				flushBuffer()
			}
		}
	}
	zopts := []zap.Option{zap.AddCaller()}
	stackLevel, stackOK := parseLevel(o.Stacktrace)
	if o.Stacktrace != "" && stackOK {
		zopts = append(zopts, zap.AddStacktrace(stackLevel))
	}
	z := zap.New(zc, zopts...).With(identity...)

	l := &Logger{own: &core{
		z0:       z,
		z2:       z.WithOptions(zap.AddCallerSkip(2)),
		level:    level,
		colored:  colored,
		maxField: o.MaxFieldBytes,
		stop:     stop,
		drops:    drops,
		config:   describe(o, format, lv, colored, stackLevel, o.Stacktrace != "" && stackOK),
	}}

	// A typo in the config must not pass silently as a default. These go past
	// the level too: with "level: error" a warning would hide itself.
	if !levelOK {
		l.own.write(zapcore.WarnLevel, "zlog: unknown level, using info", zap.String("given", o.Level))
	}
	if format != "" && format != FormatJSON && format != FormatConsole {
		l.own.write(zapcore.WarnLevel, "zlog: unknown format, using console", zap.String("given", o.Format))
	}
	if !stackOK {
		l.own.write(zapcore.WarnLevel, "zlog: unknown stacktrace level, no stack traces", zap.String("given", o.Stacktrace))
	}
	return l
}

// write hands a record to the core directly, past the level, for what has to
// be seen whatever the level is: how the logger is configured, and that its
// configuration has a typo. There is no caller: the line would be one of this
// package.
func (c *core) write(lv zapcore.Level, msg string, fields ...zap.Field) {
	_ = c.z0.Core().Write(zapcore.Entry{Level: lv, Time: time.Now(), Message: msg}, fields)
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
	add := func(key, v string) {
		if v != "" {
			f = append(f, zap.String(key, v))
		}
	}
	add(keyService, o.Service)
	add(keyEnv, o.Env)
	host, _ := os.Hostname()
	add(keyHost, host)
	if o.Version == "" {
		o.Version = vcsRevision()
	}
	add(keyVersion, o.Version)
	return f
}

func vcsRevision() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	for _, s := range info.Settings {
		if s.Key == "vcs.revision" {
			return s.Value[:min(12, len(s.Value))]
		}
	}
	return ""
}

// Init builds a Logger and installs it as the one used by the package-level
// functions, and by the standard library's log and log/slog. Call it once at
// the start of main, and zlog.Sync before the process exits.
//
// Its first record, "logger configured", lists the settings in effect, so that
// the start of a service shows how it logs without anybody having to work out
// which file, default or environment variable applied.
func Init(o Options) *Logger {
	l := New(o)
	if old := std.Swap(l.own); old != nil && old.stop != nil {
		old.stop()
	}
	redirectStdLoggers()
	l.own.announce()
	return l
}

// announce writes the settings in effect, past the level like write: a service
// set to "error" is the one whose settings somebody will want to see. The
// caller is whoever called Init.
func (c *core) announce() {
	ent := zapcore.Entry{Level: zapcore.InfoLevel, Time: time.Now(), Message: "logger configured"}
	if _, file, line, ok := runtime.Caller(2); ok {
		ent.Caller = zapcore.NewEntryCaller(0, file, line, true)
	}
	_ = c.z0.Core().Write(ent, c.config)
}

// describe lists the settings in effect under the names they have in the
// configuration file: not what was written there, but what came of it, a
// default where nothing was written and the fallback where it was a typo.
//
// JSON gets an object, "config": {...}, whose keys cannot clash with those of
// the record, and next to it "config_notes": {...} for the settings that were
// not written down as they are: "default", or where the value is from. The
// console gets the same as lines below the record, the note in brackets behind
// the value.
func describe(o Options, format string, lv zapcore.Level, colored bool, stackLevel zapcore.Level, stack bool) []zap.Field {
	host, _ := os.Hostname()
	version := o.Version
	if version == "" {
		version = vcsRevision()
	}
	if format != FormatJSON {
		format = FormatConsole
	}
	stacktrace, sampling, buffer, output, maxField := "off", "off", "off", "custom writer", "no limit"
	if o.MaxFieldBytes > 0 {
		maxField = strconv.Itoa(o.MaxFieldBytes)
	}
	if stack {
		stacktrace = stackLevel.String()
	}
	if o.Sampling.First > 0 {
		sampling = fmt.Sprintf("first %d per second and message, then every %d", o.Sampling.First, o.Sampling.Thereafter)
	}
	if o.Buffer.Size > 0 {
		flush := o.Buffer.FlushInterval
		if flush <= 0 {
			flush = time.Second
		}
		buffer = fmt.Sprintf("%d bytes, flushed every %s", o.Buffer.Size, flush)
	}
	switch o.Output {
	case os.Stdout:
		output = "stdout"
	case os.Stderr:
		output = "stderr"
	}

	type setting struct {
		key, value string
		note       string // "" for a value that was written down as it is
	}
	ifDefault := func(unset bool) string {
		if unset {
			return "default"
		}
		return ""
	}
	settings := []setting{
		{"level", lv.String(), ifDefault(o.Level == "")},
		{"format", format, ifDefault(o.Format == "" && !o.JSON)},
		{"stacktrace", stacktrace, ifDefault(o.Stacktrace == "")},
		{"sampling", sampling, ifDefault(o.Sampling.First <= 0)},
		{"buffer", buffer, ifDefault(o.Buffer.Size <= 0)},
		{"max_field_bytes", maxField, ifDefault(o.MaxFieldBytes <= 0)},
		{"service", o.Service, o.ServiceNote},
		{"env", o.Env, o.EnvNote},
		{"version", version, ifDefault(o.Version == "")},
		{"host", host, ""},
		{"output", output, ""},
	}
	if format == FormatConsole {
		settings = append(settings, setting{"color", strconv.FormatBool(colored), ""})
	}

	if format == FormatJSON {
		notes := map[string]string{}
		fields := []zap.Field{zap.Namespace("config")}
		for _, s := range settings {
			fields = append(fields, zap.String(s.key, s.value))
			if s.note != "" && s.value != "" {
				notes[s.key] = s.note
			}
		}
		// Before the namespace, which takes in everything that follows it.
		return append([]zap.Field{zap.Any("config_notes", notes)}, fields...)
	}
	var b strings.Builder
	for _, s := range settings {
		switch {
		case s.value == "":
			fmt.Fprintf(&b, "%s: (not set)\n", s.key)
		case s.note != "":
			fmt.Fprintf(&b, "%s: %s (%s)\n", s.key, s.value, s.note)
		default:
			fmt.Fprintf(&b, "%s: %s\n", s.key, s.value)
		}
	}
	return []zap.Field{zap.String("config", b.String())}
}

// SetDefault installs l as the default, as Init does with the logger it
// builds, and returns a function that puts the previous one back. It is meant
// for tests; see package zlogtest.
func SetDefault(l *Logger) (restore func()) {
	prev := std.Swap(l.core())
	return func() { std.Store(prev) }
}

// Default returns a logger that writes where the package-level functions do.
// It follows Init; see Logger.
func Default() *Logger { return followStd }

// with returns a core that also writes fields. Of two fields with the same key
// the later one stays, at the position of the earlier one.
func (c *core) with(fields []Field) *core {
	d := *c
	d.added = make([]zap.Field, 0, len(c.added)+len(fields))
	d.addedKeys = make([]string, 0, len(c.added)+len(fields))
	d.added = append(d.added, c.added...)
	d.addedKeys = append(d.addedKeys, c.addedKeys...)
next:
	for _, f := range fields {
		for i, k := range d.addedKeys {
			if k == f.z.Key && k != "" {
				d.added[i] = c.convert(f)
				continue next
			}
		}
		d.added = append(d.added, c.convert(f))
		d.addedKeys = append(d.addedKeys, f.z.Key)
	}
	return &d
}

// The frames of this package between the caller and zap are fixed on every
// path, and z2 skips them: the logging function itself and log or logf.
//
// log converts the fields only once the record is known to be written, so a
// call below the level costs no allocation: the caller's fields never leave
// its stack.
func (c *core) log(lv zapcore.Level, msg string, fields []Field) {
	if ce := c.z2.Check(lv, c.clip(msg)); ce != nil {
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
	if ce := c.z2.Check(lv, c.clip(msg)); ce != nil {
		ce.Write(c.zap(nil)...)
	}
}

// sync leaves out the error that is not one: a terminal or a pipe cannot be
// synced, and stdout is one of the two more often than not.
func (c *core) sync() error {
	if c.drops != nil {
		c.drops.flush()
	}
	err := c.z0.Sync()
	if errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOTTY) || errors.Is(err, syscall.EBADF) {
		return nil
	}
	return err
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
// l.With(zlog.Str("component", "repo")). A field with a key that l already
// adds replaces that one.
func (l *Logger) With(fields ...Field) *Logger {
	if l.own != nil {
		return &Logger{own: l.own.with(fields)}
	}
	all := make([]Field, 0, len(l.fields)+len(fields))
	return &Logger{fields: append(append(all, l.fields...), fields...)}
}

// Zap returns the underlying zap logger, for what this package does not wrap
// (Named, Check, WithOptions, ...). Fields passed to it directly are neither
// renamed when they clash with a key of the record nor replaced by a later
// field of the same key. The result is a plain zap logger and cannot follow
// Init: ask for it where it is used rather than keeping it in a package-level
// variable.
func (l *Logger) Zap() *zap.Logger {
	c := l.core()
	if len(c.added) == 0 {
		return c.z0
	}
	return c.z0.With(c.added...)
}

// Enabled reports whether a record of that level would be written. It is for
// records that are expensive to put together, because Go evaluates the
// arguments of a call whether or not anything is logged:
//
//	if zlog.Enabled(zlog.LevelDebug) {
//		zlog.Debug("room state", zlog.Any("room", room.Snapshot()))
//	}
func (l *Logger) Enabled(lv Level) bool { return l.core().level.Enabled(zapcore.Level(lv)) }

// SetLevel changes the level at runtime. It also applies to every logger
// derived from l with With. An unknown level is ignored and reported.
func (l *Logger) SetLevel(level string) {
	lv, ok := parseLevel(level)
	if !ok || level == "" {
		l.Warn("zlog: unknown level, keeping the current one", Str("given", level))
		return
	}
	l.core().level.SetLevel(lv)
}

// Sync flushes buffered records. Call it before the process exits.
func (l *Logger) Sync() error { return l.core().sync() }

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
func L() *zap.Logger { return followStd.Zap() }

// Enabled reports whether the default logger writes records of that level;
// see Logger.Enabled.
func Enabled(lv Level) bool { return std.Load().level.Enabled(zapcore.Level(lv)) }

// SetLevel changes the default logger's level at runtime; see Logger.SetLevel.
func SetLevel(level string) { followStd.SetLevel(level) }

// Sync flushes the default logger; see Logger.Sync.
func Sync() error { return std.Load().sync() }

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
