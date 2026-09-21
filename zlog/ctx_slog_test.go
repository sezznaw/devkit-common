package zlog

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"log/slog"
	"strings"
	"testing"
	"testing/slogtest"
)

// Item 2.
func TestCtx(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	var buf bytes.Buffer
	restore := SetDefault(New(Options{Output: &buf}))
	defer restore()

	// Without a logger in it, and with a nil context, Ctx is the default.
	Ctx(context.Background()).Info("plain")
	Ctx(nil).Info("nil") //nolint:staticcheck // a nil context must not panic

	ctx := CtxWith(context.Background(), Str("trace_id", "t1"), Str("method", "Ping"))
	at := here(1)
	Ctx(ctx).Info("in request", Int("uid", 1))

	// Fields add up down the call chain and do not leak back up.
	inner := CtxWith(ctx, Int("uid", 7))
	Ctx(inner).Infof("inner %d", 1)
	Ctx(ctx).Info("outer again")

	want := []string{
		" plain\n", " nil\n",
		" " + at + " in request trace_id=t1 method=Ping uid=1\n",
		" inner 1 trace_id=t1 method=Ping uid=7\n",
		" outer again trace_id=t1 method=Ping\n",
	}
	lines := strings.SplitAfter(buf.String(), "\n")
	for i, w := range want {
		if !strings.HasSuffix(lines[i], w) {
			t.Errorf("line %d = %q, want suffix %q", i, lines[i], w)
		}
	}

	// A logger of its own can be the logger of a context too.
	var own bytes.Buffer
	ctx = NewCtx(context.Background(), New(Options{Output: &own}).With(Str("job", "cron")))
	Ctx(ctx).Info("tick")
	if !strings.HasSuffix(own.String(), " tick job=cron\n") {
		t.Errorf("own: %q", own.String())
	}
}

// The logger of a context made before Init follows Init like any other.
func TestCtxFollowsInit(t *testing.T) {
	prev := std.Load()
	defer std.Store(prev)
	ctx := CtxWith(context.Background(), Str("trace_id", "t1"))
	var buf bytes.Buffer
	Init(Options{Format: "json", Service: "svc", Output: &buf})
	Ctx(ctx).Info("hello")
	if !strings.Contains(buf.String(), `"service":"svc"`) || !strings.Contains(buf.String(), `"trace_id":"t1"`) {
		t.Errorf("%s", buf.String())
	}
}

// Item 3: what dependencies log with log/slog or log comes out in our format.
func TestStandardLoggersAreRedirected(t *testing.T) {
	prevSlog, prevStd := slog.Default(), std.Load()
	prevFlags, prevOut := log.Flags(), log.Writer()
	defer func() { slog.SetDefault(prevSlog); std.Store(prevStd); log.SetFlags(prevFlags); log.SetOutput(prevOut) }()

	var buf bytes.Buffer
	Init(Options{Format: "json", Level: "info", Service: "svc", Output: &buf})

	at := here(1)
	slog.Warn("from slog", "uid", 1001, "level", "mine", slog.Group("req", slog.String("id", "r1")))
	slog.Debug("below the level")
	slog.With("component", "dep").WithGroup("g").Info("grouped", "k", "v")
	log.Printf("from the log package: %d", 42)

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("want 3 records, got %d:\n%s", len(lines), buf.String())
	}
	for _, w := range []string{`"level":"WARN"`, `"msg":"from slog"`, `"service":"svc"`, `"uid":1001`, `"fields.level":"mine"`, `"req":{"id":"r1"}`, `"caller":"zlog/` + at + `"`} {
		if !strings.Contains(lines[0], w) {
			t.Errorf("slog record lacks %s: %s", w, lines[0])
		}
	}
	if !strings.Contains(lines[1], `"component":"dep","g":{"k":"v"}`) {
		t.Errorf("WithAttrs / WithGroup: %s", lines[1])
	}
	if !strings.Contains(lines[2], `"level":"INFO"`) || !strings.Contains(lines[2], `"msg":"from the log package: 42"`) {
		t.Errorf("log package record: %s", lines[2])
	}
	for _, ln := range lines {
		seen := map[string]bool{}
		for _, k := range topLevelKeys(t, ln) {
			if seen[k] {
				t.Errorf("key %q twice: %s", k, ln)
			}
			seen[k] = true
		}
	}

	// The redirection follows a later Init and SetLevel without being set up again.
	var second bytes.Buffer
	Init(Options{Format: "json", Level: "debug", Output: &second})
	slog.Debug("now visible")
	if !strings.Contains(second.String(), `"msg":"now visible"`) {
		t.Errorf("after the second Init: %q", second.String())
	}
}

// The handler passes the conformance suite of the standard library.
func TestSlogHandlerConformance(t *testing.T) {
	var buf bytes.Buffer
	restore := SetDefault(New(Options{Format: "json", Level: "debug", Output: &buf}))
	defer restore()

	err := slogtest.TestHandler(slogHandler{}, func() []map[string]any {
		var out []map[string]any
		for _, ln := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
			var m map[string]any
			if err := json.Unmarshal([]byte(ln), &m); err != nil {
				t.Fatalf("%v: %s", err, ln)
			}
			// slogtest looks for slog's names of the built-in keys.
			if v, ok := m["level"]; ok {
				m[slog.LevelKey] = v
			}
			if v, ok := m["msg"]; ok {
				m[slog.MessageKey] = v
			}
			if v, ok := m["time"]; ok {
				m[slog.TimeKey] = v
			}
			delete(m, "caller")
			out = append(out, m)
		}
		return out
	})
	if err != nil {
		t.Error(err)
	}
}
