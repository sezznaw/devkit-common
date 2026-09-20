package zlog

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
)

func TestJSONRecord(t *testing.T) {
	var buf bytes.Buffer
	l := New(Options{Format: "json", Output: &buf, Service: "svc"})
	l.Info("hello", Str("k", "v"), Int("n", 7))

	if strings.Count(buf.String(), "\n") != 1 {
		t.Fatalf("want exactly one line, got: %q", buf.String())
	}
	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("not JSON: %v: %s", err, buf.String())
	}
	want := map[string]any{"level": "INFO", "msg": "hello", "service": "svc", "k": "v", "n": float64(7)}
	for k, v := range want {
		if rec[k] != v {
			t.Errorf("%s = %v, want %v (record: %s)", k, rec[k], v, buf.String())
		}
	}
	ts, _ := rec["time"].(string)
	if _, err := time.Parse(jsonTimeLayout, ts); err != nil {
		t.Errorf("time = %q, want RFC 3339 with milliseconds and a zone offset: %v", ts, err)
	}
	if strings.Contains(buf.String(), "\x1b[") {
		t.Errorf("JSON output must never be colored: %q", buf.String())
	}
}

func TestConsoleColorsPerLevel(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	var buf bytes.Buffer
	l := New(Options{Level: "debug", Output: &buf})
	l.Debug("d")
	l.Info("i")
	l.Warn("w")
	l.Error("e")

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	want := []string{
		colorDebug + "DEBUG" + ansiReset,
		colorInfo + "INFO" + ansiReset,
		colorWarn + "WARN" + ansiReset,
		colorError + "ERROR" + ansiReset,
	}
	if len(lines) != len(want) {
		t.Fatalf("want %d lines, got %d: %q", len(want), len(lines), buf.String())
	}
	for i, w := range want {
		if !strings.Contains(lines[i], w) {
			t.Errorf("line %d = %q, want it to contain %q", i, lines[i], w)
		}
	}
	seen := map[string]bool{}
	for _, c := range []string{colorDebug, colorInfo, colorWarn, colorError, colorFatal} {
		if seen[c] {
			t.Errorf("color %q is used by two levels", c)
		}
		seen[c] = true
	}
}

func TestNoColorEnv(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	var buf bytes.Buffer
	l := New(Options{Output: &buf})
	at := here(1)
	l.Error("boom", Str("k", "v"))
	if strings.Contains(buf.String(), "\x1b[") {
		t.Fatalf("NO_COLOR is set but output has escapes: %q", buf.String())
	}
	if _, err := time.ParseInLocation(consoleTimeLayout+" ", buf.String()[:len(consoleTimeLayout)+1], time.Local); err != nil {
		t.Fatalf("line must start with %q: %q", consoleTimeLayout, buf.String())
	}
	if !strings.Contains(buf.String(), "ERROR "+at+" boom") || !strings.HasSuffix(buf.String(), " boom k=v\n") {
		t.Fatalf("unexpected output: %q", buf.String())
	}
}

func TestLevelFilterAndSetLevel(t *testing.T) {
	var buf bytes.Buffer
	l := New(Options{Format: "json", Output: &buf}) // info by default
	child := l.With(Str("component", "repo"))

	l.Debug("hidden")
	if buf.Len() != 0 {
		t.Fatalf("debug record passed an info logger: %s", buf.String())
	}
	l.SetLevel("debug")
	child.Debug("shown")
	if !strings.Contains(buf.String(), `"msg":"shown"`) || !strings.Contains(buf.String(), `"component":"repo"`) {
		t.Fatalf("SetLevel did not reach the child logger: %s", buf.String())
	}

	buf.Reset()
	l.SetLevel("nonsense")
	l.Debug("still shown")
	if !strings.Contains(buf.String(), "unknown level") || !strings.Contains(buf.String(), "still shown") {
		t.Fatalf("an unknown level must be reported and ignored: %s", buf.String())
	}
}

func TestUnknownOptionsAreReported(t *testing.T) {
	var buf bytes.Buffer
	l := New(Options{Level: "inf", Format: "jsno", Output: &buf})
	if !strings.Contains(buf.String(), "unknown level") || !strings.Contains(buf.String(), "unknown format") {
		t.Fatalf("typos in Options were not reported: %q", buf.String())
	}
	buf.Reset()
	l.Debug("hidden")
	l.Info("shown")
	if strings.Contains(buf.String(), "hidden") || !strings.Contains(buf.String(), "shown") {
		t.Fatalf("fallback level is not info: %q", buf.String())
	}
}

func TestPrintfStyle(t *testing.T) {
	var buf bytes.Buffer
	l := New(Options{Format: "json", Output: &buf})
	l.Warnf("player %d from %s", 42, "cn")
	if !strings.Contains(buf.String(), `"msg":"player 42 from cn"`) || !strings.Contains(buf.String(), `"level":"WARN"`) {
		t.Fatalf("unexpected output: %s", buf.String())
	}
}

func TestPackageLevelFunctionsUseInit(t *testing.T) {
	prev := std.Load()
	t.Cleanup(func() { std.Store(prev) })

	var buf bytes.Buffer
	Init(Options{Format: "json", Output: &buf, Service: "svc"})
	Info("via package", Str("k", "v"))
	With(Str("component", "repo")).Errorf("failed %d", 1)
	L().Info("raw zap", zap.Int64("uid", 9))

	out := buf.String()
	for _, w := range []string{
		`"msg":"via package"`, `"k":"v"`,
		`"component":"repo"`, `"msg":"failed 1"`,
		`"msg":"raw zap"`, `"uid":9`,
	} {
		if !strings.Contains(out, w) {
			t.Errorf("output lacks %s: %s", w, out)
		}
	}
	if strings.Count(out, `"service":"svc"`) != 3 {
		t.Errorf("service must be on every record: %s", out)
	}
}

// here returns "<file base name>:<line of the call + offset>".
func here(offset int) string {
	_, file, line, _ := runtime.Caller(1)
	return filepath.Base(file) + ":" + strconv.Itoa(line+offset)
}

// The caller must be the line that logs, whichever way the call goes in. The
// tests run in the package directory, so the console form is the bare name.
func TestCallerOnEveryPath(t *testing.T) {
	prev := std.Load()
	t.Cleanup(func() { std.Store(prev) })
	t.Setenv("NO_COLOR", "1")

	var buf bytes.Buffer
	l := Init(Options{Output: &buf})

	var want []string
	want = append(want, here(1))
	Info("package function")
	want = append(want, here(1))
	Infof("package %s", "printf")
	want = append(want, here(1))
	l.Warn("method")
	want = append(want, here(1))
	l.Warnf("method %s", "printf")
	want = append(want, here(1))
	l.With(Str("k", "v")).Error("child")
	want = append(want, here(1))
	With(Str("k", "v")).With(Str("k2", "v2")).Errorf("grand%s", "child")
	want = append(want, here(1))
	L().Info("raw zap")
	want = append(want, here(1))
	l.With(Str("k", "v")).Zap().Info("raw zap of a child")

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != len(want) {
		t.Fatalf("want %d lines, got %d: %q", len(want), len(lines), buf.String())
	}
	for i, w := range want {
		// White space on both sides is what makes a console see a link.
		if !strings.Contains(lines[i], " "+w+" ") {
			t.Errorf("line %d = %q, want caller %q", i, lines[i], w)
		}
	}
}

func TestJSONCallerIsShort(t *testing.T) {
	var buf bytes.Buffer
	l := New(Options{Format: "json", Output: &buf})
	want := "zlog/" + here(1)
	l.Info("hello")
	if !strings.Contains(buf.String(), `"caller":"`+want+`"`) {
		t.Fatalf("want caller %q: %s", want, buf.String())
	}
}

func TestClickablePath(t *testing.T) {
	abs := func(p string) string { return filepath.FromSlash(p) }
	for _, c := range []struct{ name, cwd, file, want string }{
		{"own code is relative to the working directory", abs("/w/game/ser-auth"), abs("/w/game/ser-auth/handler/handler.go"), "handler/handler.go"},
		{"file in the working directory itself", abs("/w/game/ser-auth"), abs("/w/game/ser-auth/main.go"), "main.go"},
		{"a sibling service stays absolute", abs("/w/game/ser-auth"), abs("/w/game/ser-user/handler/handler.go"), abs("/w/game/ser-user/handler/handler.go")},
		{"a directory that only shares the prefix", abs("/w/game/ser"), abs("/w/game/ser-auth/main.go"), abs("/w/game/ser-auth/main.go")},
		{"the module cache stays absolute", abs("/w/game/ser-auth"), abs("/go/pkg/mod/x@v1.0.0/x.go"), abs("/go/pkg/mod/x@v1.0.0/x.go")},
		{"a name starting with two dots is not a parent", abs("/w"), abs("/w/..hidden/x.go"), "..hidden/x.go"},
		{"-trimpath build", abs("/w/game/ser-auth"), "game/ser-auth/main.go", "game/ser-auth/main.go"},
		{"unknown working directory", "", abs("/w/game/ser-auth/main.go"), abs("/w/game/ser-auth/main.go")},
	} {
		if got := clickablePath(c.cwd, c.file); got != c.want {
			t.Errorf("%s: clickablePath(%q, %q) = %q, want %q", c.name, c.cwd, c.file, got, c.want)
		}
	}
}

// A package-level "var log = zlog.With(...)" runs before main calls Init. Such
// a logger must pick up what Init installs instead of staying on the defaults
// the package starts with: wrong level, no service field, and console text in
// the middle of a JSON stream.
func TestLoggersCreatedBeforeInitFollowIt(t *testing.T) {
	prev := std.Load()
	t.Cleanup(func() { std.Store(prev) })

	early := With(Str("component", "repo"))
	grandchild := early.With(Str("table", "player"))
	def := Default()

	var buf bytes.Buffer
	Init(Options{Level: "debug", Format: "json", Service: "svc", Output: &buf})
	at := here(1)
	early.Debug("from early")
	grandchild.Debug("from grandchild")
	def.Debug("from default")

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("want 3 JSON lines, got %d: %q", len(lines), buf.String())
	}
	for i, want := range [][]string{
		{`"msg":"from early"`, `"service":"svc"`, `"component":"repo"`, `"caller":"zlog/` + at + `"`},
		{`"msg":"from grandchild"`, `"service":"svc"`, `"component":"repo"`, `"table":"player"`},
		{`"msg":"from default"`, `"service":"svc"`},
	} {
		for _, w := range want {
			if !strings.Contains(lines[i], w) {
				t.Errorf("line %d lacks %s: %s", i, w, lines[i])
			}
		}
	}
	if strings.Contains(lines[2], "component") {
		t.Errorf("With must not add fields to the logger it was called on: %s", lines[2])
	}

	// A second Init, as tests do, is followed as well.
	var buf2 bytes.Buffer
	Init(Options{Format: "json", Output: &buf2})
	buf.Reset()
	early.Info("after second init")
	if buf.Len() != 0 || !strings.Contains(buf2.String(), `"component":"repo"`) || strings.Contains(buf2.String(), "svc") {
		t.Fatalf("early logger did not move to the second Init: old=%q new=%q", buf.String(), buf2.String())
	}
}

func TestOwnLoggersIgnoreInit(t *testing.T) {
	prev := std.Load()
	t.Cleanup(func() { std.Store(prev) })

	var own, def bytes.Buffer
	l := New(Options{Format: "json", Output: &own})
	child := l.With(Str("component", "repo"))
	Init(Options{Format: "json", Output: &def})

	l.Info("parent")
	child.Info("child")
	if def.Len() != 0 || strings.Count(own.String(), "\n") != 2 || !strings.Contains(own.String(), `"component":"repo"`) {
		t.Fatalf("a logger from New must keep its own output: own=%q default=%q", own.String(), def.String())
	}
}

// Run with -race: loggers that follow the default are used while Init swaps it.
func TestFollowingLoggerIsSafeForConcurrentUse(t *testing.T) {
	prev := std.Load()
	t.Cleanup(func() { std.Store(prev) })

	Init(Options{Output: io.Discard})
	l := With(Str("component", "repo"))
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				l.Info("x", Int("i", i))
			}
		}()
	}
	for i := 0; i < 20; i++ {
		Init(Options{Format: "json", Output: io.Discard})
	}
	wg.Wait()
}

// fieldsOf logs one console record without colors and returns what follows
// the message.
func fieldsOf(t *testing.T, log func(l *Logger)) string {
	t.Helper()
	t.Setenv("NO_COLOR", "1")
	var buf bytes.Buffer
	log(New(Options{Output: &buf}))
	_, after, ok := strings.Cut(buf.String(), " msg")
	if !ok || !strings.HasSuffix(after, "\n") || strings.Count(after, "\n") != 1 {
		t.Fatalf("unexpected record: %q", buf.String())
	}
	return strings.TrimSuffix(after, "\n")
}

func TestConsoleFieldsAreKeyValue(t *testing.T) {
	type point struct {
		X, Y int
	}
	at := time.Date(2026, 9, 20, 8, 30, 0, 0, time.UTC)
	for _, c := range []struct {
		name string
		log  func(l *Logger)
		want string
	}{
		{"no fields", func(l *Logger) { l.Info("msg") }, ""},
		{"scalars, in the order given", func(l *Logger) { l.Info("msg", Int("uid", 1001), Bool("vip", true), Float("rate", 0.5)) }, " uid=1001 vip=true rate=0.5"},
		{"a plain word is not quoted", func(l *Logger) { l.Info("msg", Str("ip", "10.0.0.7"), Str("name", "张三")) }, " ip=10.0.0.7 name=张三"},
		{"quoted when it would not read as one word", func(l *Logger) {
			l.Info("msg", Str("a", "two words"), Str("b", ""), Str("c", "x=y"), Str("d", "line\nbreak"))
		}, ` a="two words" b="" c="x=y" d="line\nbreak"`},
		{"error", func(l *Logger) { l.Info("msg", Err(errors.New("redis: i/o timeout"))) }, ` err="redis: i/o timeout"`},
		{"duration and time", func(l *Logger) { l.Info("msg", Dur("took", 1500*time.Millisecond), Time("at", at)) }, " took=1.5s at=2026-09-20T08:30:00.000Z"},
		{"values without a typed field are JSON", func(l *Logger) { l.Info("msg", Any("pos", point{1, 2}), Any("ids", []int{1, 2}), Any("none", nil)) }, ` pos={"X":1,"Y":2} ids=[1,2] none=null`},
		{"With comes first and children do not leak into the parent", func(l *Logger) {
			child := l.With(Str("component", "repo"))
			child.With(Str("table", "player"))
			child.Info("msg", Int("uid", 1))
		}, " component=repo uid=1"},
		{"namespace", func(l *Logger) {
			l.Zap().With(zap.Namespace("req"), zap.String("id", "r1")).Info("msg", zap.Int("n", 2))
		}, " req.id=r1 req.n=2"},
	} {
		if got := fieldsOf(t, c.log); got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.name, got, c.want)
		}
	}
}

func TestConsoleKeysAreDimmed(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	var buf bytes.Buffer
	New(Options{Output: &buf}).Info("msg", Int("uid", 1001))
	if want := " " + ansiDim + "uid=" + ansiReset + "1001\n"; !strings.HasSuffix(buf.String(), want) {
		t.Fatalf("got %q, want suffix %q", buf.String(), want)
	}
}

// The fields that identify the process are the only content the console
// leaves out.
func TestServiceIsInJSONOnly(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	var console, js bytes.Buffer
	New(Options{Service: "svc", Output: &console}).With(Str("component", "repo")).Info("hello", Int("uid", 1))
	New(Options{Service: "svc", Output: &js, Format: "json"}).With(Str("component", "repo")).Info("hello", Int("uid", 1))

	if strings.Contains(console.String(), "svc") || !strings.HasSuffix(console.String(), " hello component=repo uid=1\n") {
		t.Errorf("console: %q", console.String())
	}
	for _, w := range []string{`"service":"svc"`, `"component":"repo"`, `"uid":1`} {
		if !strings.Contains(js.String(), w) {
			t.Errorf("JSON lacks %s: %s", w, js.String())
		}
	}
}

type roomState string

func TestFieldConstructors(t *testing.T) {
	var (
		i32  int32   = -7
		u64  uint64  = math.MaxUint64
		f32  float32 = 0.1
		open         = roomState("open")
		none error
	)
	got := fieldsOf(t, func(l *Logger) {
		l.Info("msg",
			Int("i32", i32), Int("i64", int64(1)<<40), Int("u8", uint8(255)), Int("u64", u64),
			Float("f32", f32), Float("f64", 0.25),
			Str("state", open),
			Err(none), // a nil error adds no field
			Bool("ok", false),
		)
	})
	want := " i32=-7 i64=1099511627776 u8=255 u64=18446744073709551615 f32=0.1 f64=0.25 state=open ok=false"
	if got != want {
		t.Errorf("\n got %q\nwant %q", got, want)
	}
}

// The same record in JSON: numbers stay numbers, the error is under "err".
func TestFieldsInJSON(t *testing.T) {
	var buf bytes.Buffer
	New(Options{Format: "json", Output: &buf}).Error("save failed",
		Int("uid", int64(1001)), Float("rate", float32(0.1)), Dur("took", 1500*time.Millisecond), Err(errors.New("timeout")))
	for _, w := range []string{`"uid":1001`, `"rate":0.1`, `"took":"1.5s"`, `"err":"timeout"`} {
		if !strings.Contains(buf.String(), w) {
			t.Errorf("JSON lacks %s: %s", w, buf.String())
		}
	}
}

func BenchmarkDisabledLevel(b *testing.B) {
	l := New(Options{Format: "json", Output: io.Discard})
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		l.Debug("cache miss", Int("uid", i), Str("ip", "10.0.0.7"))
	}
}

func BenchmarkJSON(b *testing.B) {
	l := New(Options{Format: "json", Output: io.Discard})
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		l.Info("player login", Int("uid", i), Str("ip", "10.0.0.7"))
	}
}

func BenchmarkConsole(b *testing.B) {
	l := New(Options{Output: io.Discard})
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		l.Info("player login", Int("uid", i), Str("ip", "10.0.0.7"))
	}
}

func TestFieldColors(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	var buf bytes.Buffer
	l := New(Options{Output: &buf})
	l.Info("msg", Int("uid", 1), Int("age", 15).Blue(), Str("note", "two words").Red(), Bool("ok", true))

	// A colored field is in its color from the key to the end of the value;
	// its neighbours keep the dimmed key and the plain value.
	want := " " + ansiDim + "uid=" + ansiReset + "1" +
		" \x1b[34mage=15" + ansiReset +
		" \x1b[31mnote=\"two words\"" + ansiReset +
		" " + ansiDim + "ok=" + ansiReset + "true\n"
	if !strings.HasSuffix(buf.String(), want) {
		t.Fatalf("\n got %q\nwant suffix %q", buf.String(), want)
	}

	// Every predefined color is a different one.
	seen := map[string]string{}
	for name, f := range map[string]Field{
		"Red": Int("k", 1).Red(), "Green": Int("k", 1).Green(), "Yellow": Int("k", 1).Yellow(), "Blue": Int("k", 1).Blue(),
		"Purple": Int("k", 1).Purple(), "Cyan": Int("k", 1).Cyan(), "Gray": Int("k", 1).Gray(),
	} {
		code := fieldColorCodes[f.color]
		if code == "" {
			t.Errorf("%s has no escape code", name)
		}
		if other, dup := seen[code]; dup {
			t.Errorf("%s and %s share %q", name, other, code)
		}
		seen[code] = name
	}
}

func TestFieldColorOnChildLogger(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	var buf bytes.Buffer
	New(Options{Output: &buf}).With(Str("component", "repo").Cyan()).Info("msg")
	if want := " msg \x1b[36mcomponent=repo" + ansiReset + "\n"; !strings.HasSuffix(buf.String(), want) {
		t.Fatalf("\n got %q\nwant suffix %q", buf.String(), want)
	}
}

// A color must change nothing where colors are not rendered.
func TestFieldColorIsIgnoredWithoutColors(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	var console, js bytes.Buffer
	New(Options{Output: &console}).Info("msg", Int("age", 15).Blue())
	New(Options{Output: &js, Format: "json"}).With(Str("component", "repo").Cyan()).Info("msg", Int("age", 15).Blue())

	if !strings.HasSuffix(console.String(), " msg age=15\n") {
		t.Errorf("console with NO_COLOR: %q", console.String())
	}
	if strings.Contains(js.String(), "\x1b") || !strings.Contains(js.String(), `"component":"repo","age":15}`) {
		t.Errorf("JSON: %q", js.String())
	}
}

func BenchmarkConsoleColoredField(b *testing.B) {
	b.Setenv("NO_COLOR", "")
	l := New(Options{Output: io.Discard})
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		l.Info("player login", Int("uid", i).Blue(), Str("ip", "10.0.0.7"))
	}
}

// topLevelKeys returns the keys of a JSON object in order, duplicates included,
// which json.Unmarshal would hide.
func topLevelKeys(t *testing.T, record string) []string {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(record))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		t.Fatalf("not a JSON object: %q", record)
	}
	var keys []string
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			t.Fatalf("%v: %q", err, record)
		}
		keys = append(keys, tok.(string))
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			t.Fatalf("%v: %q", err, record)
		}
	}
	return keys
}

// Whatever key is added to the records later (env, host, ...) must be added to
// fieldKey as well; this is what notices if it was forgotten.
func TestEveryRecordKeyIsProtected(t *testing.T) {
	var buf bytes.Buffer
	New(Options{Format: "json", Service: "svc", Output: &buf}).Info("hello")
	for _, k := range topLevelKeys(t, buf.String()) {
		if fieldKey(k) == k {
			t.Errorf("records have the key %q, but a field of that name is not renamed", k)
		}
	}
}

func TestFieldsNamedLikeRecordKeysAreRenamed(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	log := func(o Options) string {
		var buf bytes.Buffer
		o.Service, o.Output = "svc", &buf
		New(o).With(Str("service", "mine"), Str("caller", "me")).
			Error("level up", Int("level", 15), Str("msg", "hi"), Str("time", "noon"), Int("uid", 1))
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
	for _, w := range []string{
		`"level":"ERROR"`, `"msg":"level up"`, `"service":"svc"`, // the record's own
		`"fields.level":15`, `"fields.msg":"hi"`, `"fields.time":"noon"`, `"fields.service":"mine"`, `"fields.caller":"me"`,
		`"uid":1`, // an ordinary key is left alone
	} {
		if !strings.Contains(js, w) {
			t.Errorf("JSON lacks %s: %s", w, js)
		}
	}

	// The console shows the final names.
	if console := log(Options{}); !strings.HasSuffix(console,
		" level up fields.service=mine fields.caller=me fields.level=15 fields.msg=hi fields.time=noon uid=1\n") {
		t.Errorf("console: %q", console)
	}
}

func TestColoredPrintfArguments(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	var buf bytes.Buffer
	l := New(Options{Output: &buf})
	for _, c := range []struct {
		name string
		log  func()
		want string
	}{
		{"only the wrapped argument is colored", func() { l.Infof("player %d from %s", Blue(1001), "cn") }, " player \x1b[34m1001" + ansiReset + " from cn\n"},
		{"precision applies to the value", func() { l.Infof("rate %.2f", Red(0.12345)) }, " rate \x1b[31m0.12" + ansiReset + "\n"},
		{"width and flags apply to the value", func() { l.Infof("[%5d] [%-4s] [%04x]", Green(42), Cyan("ab"), Yellow(255)) },
			" [\x1b[32m   42" + ansiReset + "] [\x1b[36mab  " + ansiReset + "] [\x1b[33m00ff" + ansiReset + "]\n"},
		{"%v and %+v", func() { l.Infof("%v %+v", Purple([]int{1, 2}), Gray(struct{ X int }{1})) }, " \x1b[38;5;135m[1 2]" + ansiReset + " " + ansiDim + "{X:1}" + ansiReset + "\n"},
		{"the outer color wins", func() { l.Infof("%d", Blue(Red(7))) }, " \x1b[34m7" + ansiReset + "\n"},
	} {
		buf.Reset()
		c.log()
		if !strings.HasSuffix(buf.String(), c.want) {
			t.Errorf("%s:\n got %q\nwant suffix %q", c.name, buf.String(), c.want)
		}
	}
}

// Where colors are not rendered the message must be the one without them.
func TestColoredPrintfArgumentsWithoutColors(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	var console, js bytes.Buffer
	args := []any{Blue(1001), "cn", Red(0.12345)}
	New(Options{Output: &console}).Infof("player %d from %s rate %.2f", args...)
	New(Options{Output: &js, Format: "json"}).Infof("player %d from %s rate %.2f", args...)

	if !strings.HasSuffix(console.String(), " player 1001 from cn rate 0.12\n") {
		t.Errorf("console with NO_COLOR: %q", console.String())
	}
	if strings.Contains(js.String(), `\u001b`) || !strings.Contains(js.String(), `"msg":"player 1001 from cn rate 0.12"`) {
		t.Errorf("JSON: %s", js.String())
	}
	if _, ok := args[0].(coloredArg); !ok {
		t.Error("the caller's argument slice was modified")
	}
}

func TestAnyTakesTheColorOfItsValue(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	var buf bytes.Buffer
	New(Options{Output: &buf}).Info("msg", Any("uid", Blue(1001)))
	if want := " msg \x1b[34muid=1001" + ansiReset + "\n"; !strings.HasSuffix(buf.String(), want) {
		t.Fatalf("\n got %q\nwant suffix %q", buf.String(), want)
	}
}

func BenchmarkPrintf(b *testing.B) {
	l := New(Options{Format: "json", Output: io.Discard})
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		l.Infof("player %d login from %s", i, "10.0.0.7")
	}
}
