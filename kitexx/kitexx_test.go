package kitexx

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bytedance/gopkg/cloud/metainfo"
	"github.com/nacos-group/nacos-sdk-go/v2/clients/config_client"
	"github.com/nacos-group/nacos-sdk-go/v2/vo"

	"github.com/sezznaw/devkit-common/zlog"
	"github.com/sezznaw/devkit-common/zlog/zlogtest"
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
	// basic info, listen address, logging middleware, meta handler, drain
	// timeout; no registry.
	if len(opts) != 5 {
		t.Fatalf("expected 5 options without a registry, got %d", len(opts))
	}
}

func TestLoggingMiddleware(t *testing.T) {
	logs := zlogtest.Capture(t)
	mw := LoggingMiddleware()
	boom := errors.New("boom")

	// A request that starts here gets a trace_id; the handler logs with it,
	// and the context carries it on to the services the handler calls.
	var inHandler string
	ep := mw(func(ctx context.Context, req, resp any) error {
		inHandler = TraceID(ctx)
		if v, _ := metainfo.GetPersistentValue(ctx, TraceIDKey); v != inHandler {
			t.Errorf("the context must carry the trace_id on: %q", v)
		}
		zlog.Ctx(ctx).Info("in handler", zlog.Int("uid", 1))
		return boom
	})
	if err := ep(context.Background(), nil, nil); err != boom {
		t.Fatalf("error not propagated: %v", err)
	}
	if len(inHandler) != 32 {
		t.Fatalf("trace_id = %q, want 32 hex characters", inHandler)
	}

	// A request that comes with a trace_id keeps it.
	ok := mw(func(ctx context.Context, req, resp any) error { return nil })
	if err := ok(metainfo.WithPersistentValue(context.Background(), TraceIDKey, "from-caller"), nil, nil); err != nil {
		t.Fatal(err)
	}

	recs := logs.Records()
	if len(recs) != 3 {
		t.Fatalf("want 3 records, got %d:\n%s", len(recs), logs)
	}
	if r := recs[0]; r.Msg != "in handler" || r.Fields["trace_id"] != inHandler || r.Fields["method"] != "unknown" || r.Fields["uid"] != float64(1) {
		t.Errorf("handler record: %+v", r)
	}
	if r := recs[1]; r.Level != "ERROR" || r.Msg != "rpc" || r.Fields["err"] != "boom" || r.Fields["trace_id"] != inHandler || r.Fields["latency"] == nil {
		t.Errorf("rpc record of the failed call: %+v", r)
	}
	if r := recs[2]; r.Level != "INFO" || r.Fields["trace_id"] != "from-caller" {
		t.Errorf("rpc record of the call with a trace_id: %+v", r)
	}
}

func TestClientOptions(t *testing.T) {
	var cfg Config
	cfg.RegistryDisabled = true
	opts, err := ClientOptions(cfg)
	if err != nil {
		t.Fatalf("without a registry no Nacos is needed: %v", err)
	}
	// TTHeader and its meta handler, which carry the trace_id; no resolver.
	if len(opts) != 2 {
		t.Fatalf("expected 2 options without a registry, got %d", len(opts))
	}
	cfg.Service.Name = "order"
	if opts, _ = ClientOptions(cfg); len(opts) != 3 {
		t.Fatalf("expected the caller's name as a third option, got %d", len(opts))
	}
}

type fakeConfigClient struct {
	config_client.IConfigClient
	content  string
	err      error
	onChange func(namespace, group, dataId, data string)
}

func (f *fakeConfigClient) GetConfig(p vo.ConfigParam) (string, error) { return f.content, f.err }
func (f *fakeConfigClient) ListenConfig(p vo.ConfigParam) error {
	f.onChange = p.OnChange
	return nil
}

func TestLogLevelFollowsNacos(t *testing.T) {
	zlogtest.Capture(t) // level debug
	zlog.SetLevel("info")

	cc := &fakeConfigClient{content: " Debug\n"}
	if err := watchLogLevel(cc, "ser-auth.log-level", "DEFAULT_GROUP"); err != nil {
		t.Fatal(err)
	}
	if !zlog.Enabled(zlog.LevelDebug) {
		t.Error("the level in Nacos at start must be applied")
	}
	cc.onChange("", "DEFAULT_GROUP", "ser-auth.log-level", "error")
	if zlog.Enabled(zlog.LevelWarn) || !zlog.Enabled(zlog.LevelError) {
		t.Error("a change in Nacos must be applied")
	}
	// Neither an empty configuration nor nonsense changes the level.
	cc.onChange("", "", "", "")
	cc.onChange("", "", "", "verbose")
	if zlog.Enabled(zlog.LevelWarn) || !zlog.Enabled(zlog.LevelError) {
		t.Error("an empty or unknown level must leave the level alone")
	}

	if err := watchLogLevel(&fakeConfigClient{err: errors.New("nacos down")}, "x", "g"); err == nil {
		t.Error("a failure to read must be reported")
	}
}

// Options fills in log.service and log.env when the configuration leaves them
// out, and the "logger configured" record has to say so.
func TestLoggerSettingsFilledInByOptionsSayWhereTheyAreFrom(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	announced := func(t *testing.T, env string) string {
		t.Helper()
		t.Cleanup(zlog.SetDefault(zlog.Default())) // Options installs a logger
		var buf bytes.Buffer
		var cfg Config
		cfg.Service.Name, cfg.RegistryDisabled = "order", true
		cfg.Log.Env, cfg.Log.Output = env, &buf
		if _, err := Options(cfg); err != nil {
			t.Fatal(err)
		}
		return buf.String()
	}
	for _, c := range []struct {
		name, appEnv, logEnv string
		want                 []string
	}{
		{"APP_ENV not set", "", "", []string{"      service: order (from service.name)\n", "      env: dev (APP_ENV is not set)\n"}},
		{"APP_ENV set", "prod", "", []string{"      env: prod (from APP_ENV)\n"}},
		{"log.env written", "prod", "prod-cn", []string{"      env: prod-cn\n"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("APP_ENV", c.appEnv)
			got := announced(t, c.logEnv)
			for _, w := range c.want {
				if !strings.Contains(got, w) {
					t.Errorf("want %q in:\n%s", w, got)
				}
			}
		})
	}
}
