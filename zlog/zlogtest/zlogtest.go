// Package zlogtest lets the tests of a service look at what it logs, or keep
// it quiet.
//
//	func TestLogin(t *testing.T) {
//		logs := zlogtest.Capture(t)
//		Login(ctx, "neo", "wrong")
//		if !logs.Has("WARN", "login failed") {
//			t.Error("a failed login must be logged")
//		}
//	}
package zlogtest

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/sezznaw/devkit-common/zlog"
)

// Record is one log record. Fields holds everything but level and message,
// as decoded from JSON: numbers are float64.
type Record struct {
	Level  string
	Msg    string
	Fields map[string]any
}

// Logs is what has been logged since Capture.
type Logs struct {
	t   testing.TB
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *Logs) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

// Capture makes the default logger, down to level debug, write into the
// returned Logs until the test ends. Everything that logs through the
// package-level functions, zlog.With, zlog.Ctx or zlog.Default is covered; a
// logger made with zlog.New is not. Tests that capture must not run in
// parallel: the default logger is one per process.
func Capture(t testing.TB) *Logs {
	t.Helper()
	logs := &Logs{t: t}
	t.Cleanup(zlog.SetDefault(zlog.New(zlog.Options{Level: "debug", Format: zlog.FormatJSON, Output: logs})))
	return logs
}

// Discard keeps the default logger quiet until the test ends.
func Discard(t testing.TB) {
	t.Helper()
	t.Cleanup(zlog.SetDefault(zlog.New(zlog.Options{Level: "error", Output: io.Discard})))
}

// Records returns the records logged so far, oldest first.
func (l *Logs) Records() []Record {
	l.t.Helper()
	l.mu.Lock()
	text := l.buf.String()
	l.mu.Unlock()

	var out []Record
	for _, line := range strings.Split(strings.TrimSpace(text), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			l.t.Fatalf("zlogtest: %v: %s", err, line)
		}
		r := Record{Fields: m}
		r.Level, _ = m["level"].(string)
		r.Msg, _ = m["msg"].(string)
		delete(m, "level")
		delete(m, "msg")
		out = append(out, r)
	}
	return out
}

// Has reports whether a record of that level ("DEBUG", "INFO", "WARN",
// "ERROR") has been logged whose message contains msg.
func (l *Logs) Has(level, msg string) bool {
	l.t.Helper()
	for _, r := range l.Records() {
		if r.Level == level && strings.Contains(r.Msg, msg) {
			return true
		}
	}
	return false
}

// String returns the records as they were written, for a failure message.
func (l *Logs) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}
