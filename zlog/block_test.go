package zlog

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

type steps []string

func (s steps) LogBlock(colored bool) string {
	if len(s) == 0 {
		return ""
	}
	out := strings.Join(s, "\n")
	if colored {
		out = "\x1b[32m" + out + ansiReset
	}
	return out
}

// A Blocker is lines below the record for people and a value for collectors.
func TestBlocker(t *testing.T) {
	t.Run("console", func(t *testing.T) {
		t.Setenv("NO_COLOR", "1")
		var buf bytes.Buffer
		New(Options{Output: &buf}).Info("deployed", Any("steps", steps{"build", "push"}), Int("n", 2))
		want := " deployed n=2\n    steps:\n      build\n      push\n"
		if !strings.HasSuffix(buf.String(), want) {
			t.Errorf("got %q, want it to end in %q", buf.String(), want)
		}
	})
	t.Run("colored console", func(t *testing.T) {
		t.Setenv("NO_COLOR", "")
		var buf bytes.Buffer
		New(Options{Output: &buf}).Info("deployed", Any("steps", steps{"build"}))
		if !strings.Contains(buf.String(), "\x1b[32mbuild") {
			t.Errorf("the block may be colored on a colored console: %q", buf.String())
		}
	})
	t.Run("empty block", func(t *testing.T) {
		t.Setenv("NO_COLOR", "1")
		var buf bytes.Buffer
		New(Options{Output: &buf}).Info("deployed", Any("steps", steps{}))
		if !strings.HasSuffix(buf.String(), " deployed steps=[]\n") {
			t.Errorf("no text, so the value as JSON: %q", buf.String())
		}
	})
	for name, o := range map[string]Options{"json": {Format: "json"}, "json with a field limit": {Format: "json", MaxFieldBytes: 1000}} {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			o.Output = &buf
			New(o).Info("deployed", Any("steps", steps{"build", "push"}))
			var rec struct{ Steps []string }
			if err := json.Unmarshal(buf.Bytes(), &rec); err != nil || len(rec.Steps) != 2 {
				t.Errorf("JSON must carry the value itself: %v: %s", err, buf.String())
			}
		})
	}
	t.Run("console with a field limit", func(t *testing.T) {
		t.Setenv("NO_COLOR", "1")
		var buf bytes.Buffer
		New(Options{Output: &buf, MaxFieldBytes: 1000}).Info("deployed", Any("steps", steps{"build", "push"}))
		if !strings.Contains(buf.String(), "    steps:\n      build\n") {
			t.Errorf("the limit must not turn the block into JSON: %q", buf.String())
		}
	})
}
