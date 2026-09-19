package log

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestNewJSONIncludesService(t *testing.T) {
	var buf bytes.Buffer
	l := New(Options{JSON: true, Output: &buf, Service: "svc"})
	l.Info("hello", "k", "v")
	if !strings.Contains(buf.String(), `"service":"svc"`) || !strings.Contains(buf.String(), `"k":"v"`) {
		t.Fatalf("unexpected output: %s", buf.String())
	}
}

func TestContextRoundTrip(t *testing.T) {
	l := New(Options{Output: &bytes.Buffer{}})
	ctx := WithContext(context.Background(), l)
	if FromContext(ctx) != l {
		t.Fatal("FromContext did not return the stored logger")
	}
}
