package zlog

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// Item 4: the same key given to With and to the call.
func TestLaterFieldReplacesEarlierOneOfTheSameKey(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	log := func(o Options) string {
		var buf bytes.Buffer
		o.Output = &buf
		l := New(o).With(Int("uid", 1), Str("component", "repo")).With(Int("uid", 2))
		l.Info("msg", Int("uid", 3), Str("a", "x"), Str("a", "y"), Err(nil), Err(nil))
		return buf.String()
	}
	js := log(Options{Format: "json"})
	seen := map[string]bool{}
	for _, k := range topLevelKeys(t, js) {
		if seen[k] {
			t.Errorf("key %q is there twice: %s", k, js)
		}
		seen[k] = true
	}
	if !strings.Contains(js, `"uid":3`) || !strings.Contains(js, `"a":"y"`) || !strings.Contains(js, `"component":"repo"`) {
		t.Errorf("the last value of a key must be the one that is written: %s", js)
	}
	if console := log(Options{}); !strings.HasSuffix(console, " msg component=repo uid=3 a=y\n") {
		t.Errorf("console: %q", console)
	}

	// The parent of a child is not changed by what the child replaces.
	var buf bytes.Buffer
	parent := New(Options{Output: &buf}).With(Int("uid", 1))
	parent.With(Int("uid", 2))
	parent.Info("msg")
	if !strings.HasSuffix(buf.String(), " msg uid=1\n") {
		t.Errorf("parent: %q", buf.String())
	}

	// The same through a logger that follows the default, and through Zap.
	restore := SetDefault(New(Options{Output: &buf}))
	defer restore()
	buf.Reset()
	With(Int("uid", 1)).With(Int("uid", 2)).Info("msg", Int("uid", 3))
	With(Int("uid", 1)).Zap().Info("raw")
	if got := buf.String(); !strings.Contains(got, " msg uid=3\n") || !strings.Contains(got, " raw uid=1\n") {
		t.Errorf("following logger: %q", got)
	}
}

// Item 5.
func TestSyncOnStdoutIsNotAnError(t *testing.T) {
	if err := New(Options{Output: os.Stdout}).Sync(); err != nil {
		t.Errorf("Sync on stdout: %v", err)
	}
	if err := Sync(); err != nil {
		t.Errorf("zlog.Sync: %v", err)
	}
	failing := New(Options{Output: failingSyncer{}})
	if err := failing.Sync(); err == nil {
		t.Error("a real error of Sync must not be hidden")
	}
}

type failingSyncer struct{}

func (failingSyncer) Write(p []byte) (int, error) { return len(p), nil }
func (failingSyncer) Sync() error                 { return errors.New("disk full") }

// Item 6: what is logged before Init obeys the environment.
func TestLoggerBeforeInitIsConfiguredByEnvironment(t *testing.T) {
	t.Setenv("ZLOG_FORMAT", "json")
	t.Setenv("ZLOG_LEVEL", "debug")
	t.Setenv("ZLOG_SERVICE", "ser-auth")
	t.Setenv("ZLOG_ENV", "prod")
	o := envOptions()
	var buf bytes.Buffer
	o.Output = &buf
	New(o).Debug("cannot read config")
	for _, w := range []string{`"level":"DEBUG"`, `"service":"ser-auth"`, `"env":"prod"`, `"msg":"cannot read config"`} {
		if !strings.Contains(buf.String(), w) {
			t.Errorf("lacks %s: %s", w, buf.String())
		}
	}
}

// The same end to end: a process that logs without ever calling Init.
func TestProcessWithoutInit(t *testing.T) {
	if os.Getenv("ZLOG_TEST_CHILD") == "noinit" {
		Error("cannot read config", Err(errors.New("no such file")))
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestProcessWithoutInit$")
	cmd.Env = append(os.Environ(), "ZLOG_TEST_CHILD=noinit", "ZLOG_FORMAT=json", "ZLOG_SERVICE=ser-auth")
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	line, _, _ := strings.Cut(string(out), "\n")
	var rec map[string]any
	if err := json.Unmarshal([]byte(line), &rec); err != nil {
		t.Fatalf("the first line is not JSON: %q", out)
	}
	if rec["service"] != "ser-auth" || rec["err"] != "no such file" {
		t.Errorf("record: %s", line)
	}
}

// stackErr prints like the errors of github.com/pkg/errors: the message for
// %v, the message and a stack of several lines for %+v, which zap writes under
// "errVerbose".
type stackErr struct{}

func (stackErr) Error() string { return "boom" }
func (stackErr) Format(f fmt.State, verb rune) {
	if verb == 'v' && f.Flag('+') {
		fmt.Fprint(f, "boom\nmain.save\n\t/w/game/ser-auth/repo/options_test.go:88")
		return
	}
	fmt.Fprint(f, "boom")
}

// Items 7 and 17: values of several lines go below the record.
func TestConsolePrintsMultilineValuesBelowTheRecord(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	var buf bytes.Buffer
	l := New(Options{Output: &buf})
	l.Error("save failed", Int("uid", 1), Str("sql", "SELECT 1\nFROM t"), Str("after", "x"))
	want := " save failed uid=1 after=x\n    sql:\n      SELECT 1\n      FROM t\n"
	if !strings.HasSuffix(buf.String(), want) {
		t.Errorf("\n got %q\nwant suffix %q", buf.String(), want)
	}

	// An error with a stack (pkg/errors): the message on the line, the stack
	// below, every frame with its path so that it is a link.
	buf.Reset()
	l.Error("save failed", Err(stackErr{}))
	got := buf.String()
	first, rest, _ := strings.Cut(got, "\n")
	if !strings.HasSuffix(first, " save failed err=boom") {
		t.Errorf("first line: %q", first)
	}
	if !strings.HasPrefix(rest, "    errVerbose:\n      boom\n") || !strings.Contains(rest, "options_test.go:") {
		t.Errorf("block: %q", rest)
	}

	// JSON keeps one line whatever the value.
	buf.Reset()
	New(Options{Output: &buf, Format: "json"}).Error("save failed", Err(stackErr{}), Str("sql", "a\nb"))
	if strings.Count(buf.String(), "\n") != 1 {
		t.Errorf("JSON record of several lines: %q", buf.String())
	}
}

// Item 8.
func TestEnabled(t *testing.T) {
	l := New(Options{Level: "warn", Output: &bytes.Buffer{}})
	for lv, want := range map[Level]bool{LevelDebug: false, LevelInfo: false, LevelWarn: true, LevelError: true} {
		if got := l.Enabled(lv); got != want {
			t.Errorf("Enabled(%d) = %v at level warn", lv, got)
		}
	}
	l.SetLevel("debug")
	if !l.With(Str("k", "v")).Enabled(LevelDebug) {
		t.Error("Enabled must follow SetLevel, for children too")
	}

	restore := SetDefault(l)
	defer restore()
	if !Enabled(LevelDebug) || !With(Str("k", "v")).Enabled(LevelDebug) {
		t.Error("package-level Enabled must follow the default")
	}
}

// Item 9.
func TestStacktrace(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	var js, console, off bytes.Buffer
	l := New(Options{Format: "json", Stacktrace: "error", Output: &js})
	l.Warn("no stack below error")
	l.Error("with stack", Str("stack", "mine"))
	lines := strings.Split(strings.TrimSpace(js.String()), "\n")
	if strings.Contains(lines[0], `"stack"`) {
		t.Errorf("warn has a stack: %s", lines[0])
	}
	var rec map[string]any
	if err := json.Unmarshal([]byte(lines[1]), &rec); err != nil {
		t.Fatal(err)
	}
	// The stack starts at the caller, not inside zlog or zap.
	if stack, _ := rec["stack"].(string); !strings.HasPrefix(stack, "github.com/sezznaw/devkit-common/zlog.TestStacktrace\n") {
		t.Errorf("stack = %q", rec["stack"])
	}
	if rec["fields.stack"] != "mine" {
		t.Errorf("a field named stack must be renamed: %s", lines[1])
	}

	New(Options{Stacktrace: "error", Output: &console}).Error("with stack")
	if got := console.String(); !strings.Contains(got, " with stack\n    stack:\n      github.com/sezznaw/devkit-common/zlog.TestStacktrace\n") || !strings.Contains(got, "options_test.go:") {
		t.Errorf("console: %q", got)
	}

	New(Options{Format: "json", Output: &off}).Error("default: no stack")
	if strings.Contains(off.String(), `"stack"`) {
		t.Errorf("stack without being asked for: %s", off.String())
	}

	var typo bytes.Buffer
	New(Options{Stacktrace: "eror", Output: &typo})
	if !strings.Contains(typo.String(), "unknown stacktrace level") {
		t.Errorf("typo not reported: %q", typo.String())
	}
}

// Item 10.
func TestIdentityFields(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	host, _ := os.Hostname()
	var js, console bytes.Buffer
	o := Options{Service: "ser-auth", Env: "prod", Version: "v1.2.3"}
	o.Format, o.Output = "json", &js
	New(o).Info("hello", Str("env", "mine"), Str("host", "mine"), Str("version", "mine"))
	for _, w := range []string{
		`"service":"ser-auth"`, `"env":"prod"`, `"version":"v1.2.3"`, fmt.Sprintf(`"host":%q`, host),
		`"fields.env":"mine"`, `"fields.host":"mine"`, `"fields.version":"mine"`,
	} {
		if !strings.Contains(js.String(), w) {
			t.Errorf("JSON lacks %s: %s", w, js.String())
		}
	}
	for _, k := range topLevelKeys(t, js.String()) {
		if fieldKey(k) == k && !strings.HasPrefix(k, clashPrefix) {
			t.Errorf("records have the key %q, but a field of that name is not renamed", k)
		}
	}

	o.Format, o.Output = "console", &console
	New(o).Info("hello")
	if strings.Contains(console.String(), "ser-auth") || strings.Contains(console.String(), "prod") || strings.Contains(console.String(), host) {
		t.Errorf("the console must leave the identity out: %q", console.String())
	}
}

// Item 11.
func TestSampling(t *testing.T) {
	var buf bytes.Buffer
	l := New(Options{Format: "json", Output: &buf, Sampling: Sampling{First: 3, Thereafter: 10}})
	for i := 0; i < 23; i++ {
		l.Error("redis down", Int("i", i))
	}
	l.Error("another message")
	// 3 at once, then the 10th and the 20th after those, and the other message.
	if got := strings.Count(buf.String(), "\n"); got != 6 {
		t.Errorf("wrote %d records, want 6:\n%s", got, buf.String())
	}

	buf.Reset()
	off := New(Options{Format: "json", Output: &buf})
	for i := 0; i < 23; i++ {
		off.Error("redis down")
	}
	if got := strings.Count(buf.String(), "\n"); got != 23 {
		t.Errorf("without sampling: %d records, want 23", got)
	}
}

type countingWriter struct {
	mu     sync.Mutex
	writes int
	buf    bytes.Buffer
}

func (w *countingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.writes++
	return w.buf.Write(p)
}

func (w *countingWriter) snapshot() (int, string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.writes, w.buf.String()
}

func TestBuffer(t *testing.T) {
	w := &countingWriter{}
	l := New(Options{Format: "json", Output: w, Buffer: Buffer{Size: 64 << 10, FlushInterval: time.Hour}})
	for i := 0; i < 100; i++ {
		l.Info("buffered", Int("i", i))
	}
	if n, _ := w.snapshot(); n != 0 {
		t.Errorf("%d writes before Sync, want none", n)
	}
	if err := l.Sync(); err != nil {
		t.Fatal(err)
	}
	if n, out := w.snapshot(); n != 1 || strings.Count(out, "\n") != 100 {
		t.Errorf("after Sync: %d writes, %d records; want 1 and 100", n, strings.Count(out, "\n"))
	}

	// The default flush interval is a second; Init stops the buffer of the
	// logger it replaces, which flushes it.
	prev := std.Load()
	defer std.Store(prev)
	w2 := &countingWriter{}
	Init(Options{Format: "json", Output: w2, Buffer: Buffer{Size: 64 << 10, FlushInterval: time.Hour}})
	Info("before the next Init")
	Init(Options{Format: "json", Output: &bytes.Buffer{}})
	if _, out := w2.snapshot(); !strings.Contains(out, "before the next Init") {
		t.Errorf("Init did not flush the logger it replaced: %q", out)
	}
}

// Item 12.
func TestMaxFieldBytes(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	type resp struct{ Body string }
	big := strings.Repeat("x", 5000)
	var buf bytes.Buffer
	l := New(Options{Format: "json", Output: &buf, MaxFieldBytes: 100})
	l.Info("msg "+big, Str("s", big), Any("resp", resp{big}), Err(errors.New(big)), Str("short", "kept"), Any("small", resp{"ok"}), Int("n", 7))
	l.Infof("formatted %s", big)
	l.With(Str("w", big)).Info("child")

	if buf.Len() > 2500 {
		t.Errorf("3 records take %d bytes", buf.Len())
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	var rec map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
		t.Fatalf("%v: %s", err, lines[0])
	}
	for _, k := range []string{"msg", "s", "resp", "err"} {
		if v, _ := rec[k].(string); !strings.HasSuffix(v, "bytes)") || !strings.Contains(v, "...(truncated, 50") || len(v) > 140 {
			t.Errorf("%s = %q", k, rec[k])
		}
	}
	if rec["short"] != "kept" || rec["n"] != float64(7) || fmt.Sprint(rec["small"]) != "map[Body:ok]" {
		t.Errorf("values within the limit must not change: %s", lines[0])
	}
	if !strings.Contains(lines[1], "...(truncated, ") || !strings.Contains(lines[2], "...(truncated, ") {
		t.Errorf("Infof and With are not limited:\n%s\n%s", lines[1], lines[2])
	}

	// Cutting never splits a character.
	buf.Reset()
	New(Options{Output: &buf, MaxFieldBytes: 4}).Info("m", Str("name", "张三丰"))
	if !strings.Contains(buf.String(), `name="张...(truncated, 9 bytes)"`) {
		t.Errorf("console: %q", buf.String())
	}

	// No limit unless asked for.
	buf.Reset()
	New(Options{Format: "json", Output: &buf}).Info("m", Str("s", big))
	if !strings.Contains(buf.String(), big) {
		t.Error("the default must not cut anything")
	}
}

func TestOptionsFromYAML(t *testing.T) {
	var o Options
	err := yaml.Unmarshal([]byte(`
level: debug
format: json
service: ser-auth
env: prod
version: v1.2.3
stacktrace: error
max_field_bytes: 8192
sampling: {first: 100, thereafter: 100}
buffer: {size: 262144, flush_interval: 500ms}
`), &o)
	if err != nil {
		t.Fatal(err)
	}
	want := Options{Level: "debug", Format: "json", Service: "ser-auth", Env: "prod", Version: "v1.2.3", Stacktrace: "error",
		MaxFieldBytes: 8192, Sampling: Sampling{100, 100}, Buffer: Buffer{262144, 500 * time.Millisecond}}
	if o != want {
		t.Errorf("\n got %+v\nwant %+v", o, want)
	}
}

// Item 13.
func TestSecret(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	var buf bytes.Buffer
	New(Options{Output: &buf}).Info("login", Str("user", "neo"), Secret("password", "hunter2"))
	New(Options{Output: &buf, Format: "json"}).Info("login", Secret("token", []byte("hunter2")))
	if got := buf.String(); strings.Contains(got, "hunter2") || !strings.Contains(got, " login user=neo password=***\n") || !strings.Contains(got, `"token":"***"`) {
		t.Errorf("%q", got)
	}
}

// Item 16: Fatal writes the record and ends the process with status 1.
func TestFatal(t *testing.T) {
	if os.Getenv("ZLOG_TEST_CHILD") == "fatal" {
		Init(Options{Format: "json", Buffer: Buffer{Size: 64 << 10, FlushInterval: time.Hour}})
		Info("before")
		Fatal("cannot start", Str("reason", "test"))
		fmt.Println("still running")
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestFatal$")
	cmd.Env = append(os.Environ(), "ZLOG_TEST_CHILD=fatal")
	out, err := cmd.Output()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 {
		t.Fatalf("err = %v, want exit status 1; output: %s", err, out)
	}
	// Buffered output must be flushed by Fatal, the record before it included.
	got := string(out)
	if !strings.Contains(got, `"msg":"before"`) || !strings.Contains(got, `"level":"FATAL"`) || !strings.Contains(got, `"reason":"test"`) ||
		!strings.Contains(got, `"caller":"zlog/options_test.go:`) || strings.Contains(got, "still running") {
		t.Errorf("output: %s", got)
	}
}

// Item 18.
func TestErrIsRedOnTheConsole(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	var buf bytes.Buffer
	l := New(Options{Output: &buf})
	l.Error("failed", Err(errors.New("boom")))
	l.Error("failed", Err(errors.New("boom")).Yellow())
	lines := strings.Split(buf.String(), "\n")
	if !strings.HasSuffix(lines[0], " failed \x1b[31merr=boom"+ansiReset) {
		t.Errorf("default: %q", lines[0])
	}
	if !strings.HasSuffix(lines[1], " failed \x1b[33merr=boom"+ansiReset) {
		t.Errorf("another color must win: %q", lines[1])
	}
}

// A configuration written for the log package that zlog replaces.
func TestDeprecatedJSONOption(t *testing.T) {
	var o Options
	if err := yaml.Unmarshal([]byte("level: info\njson: true\n"), &o); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	o.Output = &buf
	New(o).Info("hello")
	if !strings.HasPrefix(buf.String(), "{") {
		t.Errorf("json: true must still mean JSON: %q", buf.String())
	}
	buf.Reset()
	New(Options{JSON: true, Format: "console", Output: &buf}).Info("hello")
	if strings.HasPrefix(buf.String(), "{") {
		t.Errorf("format must win over the deprecated json: %q", buf.String())
	}
}
