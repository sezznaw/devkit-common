package kitexx

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sezznaw/devkit-common/log"
)

func TestOptionsRequiresName(t *testing.T) {
	if _, err := Options(Config{}); err == nil {
		t.Fatal("expected error for empty service name")
	}
}

func TestOptionsWithoutRegistry(t *testing.T) {
	var cfg Config
	cfg.Service.Name = "order"
	cfg.RegistryDisabled = true
	cfg.Log.Output = &bytes.Buffer{}
	opts, err := Options(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// basic info, listen address, logging middleware, drain timeout; no registry.
	if len(opts) != 4 {
		t.Fatalf("expected 4 options without a registry, got %d", len(opts))
	}
}

func TestLoggingMiddleware(t *testing.T) {
	var buf bytes.Buffer
	l := log.New(log.Options{Output: &buf, JSON: true})
	mw := LoggingMiddleware(l)
	boom := errors.New("boom")
	ep := mw(func(ctx context.Context, req, resp any) error {
		if log.FromContext(ctx) == l {
			t.Error("handler must receive a request-scoped logger")
		}
		return boom
	})
	if err := ep(context.Background(), nil, nil); err != boom {
		t.Fatalf("error not propagated: %v", err)
	}
	if !strings.Contains(buf.String(), `"level":"ERROR"`) || !strings.Contains(buf.String(), "boom") {
		t.Errorf("unexpected log: %s", buf.String())
	}
}
