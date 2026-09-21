package kitexx

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bytedance/gopkg/cloud/metainfo"
	"github.com/cloudwego/kitex/pkg/registry"
	"github.com/cloudwego/kitex/pkg/utils"
	"github.com/nacos-group/nacos-sdk-go/v2/clients/naming_client"
	"github.com/nacos-group/nacos-sdk-go/v2/model"
	"github.com/nacos-group/nacos-sdk-go/v2/vo"
	"gopkg.in/yaml.v3"

	"github.com/sezznaw/devkit-common/nacosx"
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

// service.addr is written as a port in conf/*.yaml (`addr: 8888`); the older
// ":8888" and a full "host:port" keep working.
func TestListenAddr(t *testing.T) {
	for in, want := range map[string]string{
		"":               ":8888",
		"8888":           ":8888",
		" 9001 ":         ":9001",
		":8888":          ":8888",
		"127.0.0.1:9001": "127.0.0.1:9001",
		"[::1]:9001":     "[::1]:9001",
	} {
		got, err := ListenAddr(in)
		if err != nil {
			t.Errorf("ListenAddr(%q): %v", in, err)
			continue
		}
		if got.String() != want {
			t.Errorf("ListenAddr(%q) = %s, want %s", in, got, want)
		}
	}
	for _, in := range []string{"0", "70000", "abc", "8888:", "localhost", "-1"} {
		_, err := ListenAddr(in)
		if err == nil {
			t.Errorf("ListenAddr(%q) must fail", in)
			continue
		}
		if !strings.Contains(err.Error(), "service.addr") || !strings.Contains(err.Error(), "a port such as 8888") {
			t.Errorf("ListenAddr(%q): the error must name the setting and what it accepts: %v", in, err)
		}
	}
}

// What matters to the developer: `addr: 8888` without quotes loads.
func TestAddrFromYAML(t *testing.T) {
	for _, doc := range []string{"service:\n  addr: 8888\n", "service:\n  addr: \":8888\"\n", "service:\n  addr: \"8888\"\n"} {
		var cfg Config
		if err := yaml.Unmarshal([]byte(doc), &cfg); err != nil {
			t.Fatalf("%q: %v", doc, err)
		}
		got, err := ListenAddr(cfg.Service.Addr)
		if err != nil || got.String() != ":8888" {
			t.Errorf("%q: got %v, %v", doc, got, err)
		}
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

type fakeLevelSource struct {
	content  string
	err      error
	onChange func(content string)
}

func (f *fakeLevelSource) Get(string, ...nacosx.Option) (string, error) { return f.content, f.err }
func (f *fakeLevelSource) OnChange(_ string, fn func(string), _ ...nacosx.Option) error {
	f.onChange = fn
	return nil
}

func TestLogLevelFollowsNacos(t *testing.T) {
	zlogtest.Capture(t) // level debug
	zlog.SetLevel("info")

	src := &fakeLevelSource{content: " Debug\n"}
	if err := watchLogLevel(src, "ser-auth.log-level"); err != nil {
		t.Fatal(err)
	}
	if !zlog.Enabled(zlog.LevelDebug) {
		t.Error("the level in Nacos at start must be applied")
	}
	src.onChange("error")
	if zlog.Enabled(zlog.LevelWarn) || !zlog.Enabled(zlog.LevelError) {
		t.Error("a change in Nacos must be applied")
	}
	// Neither an empty configuration nor nonsense changes the level.
	src.onChange("")
	src.onChange("verbose")
	if zlog.Enabled(zlog.LevelWarn) || !zlog.Enabled(zlog.LevelError) {
		t.Error("an empty or unknown level must leave the level alone")
	}

	if err := watchLogLevel(&fakeLevelSource{err: errors.New("nacos down")}, "x"); err == nil {
		t.Error("a failure to read must be reported")
	}
}

type fakeNaming struct {
	naming_client.INamingClient
	registered   []vo.RegisterInstanceParam
	deregistered []vo.DeregisterInstanceParam
	instances    []model.Instance
}

func (f *fakeNaming) RegisterInstance(p vo.RegisterInstanceParam) (bool, error) {
	f.registered = append(f.registered, p)
	return true, nil
}
func (f *fakeNaming) DeregisterInstance(p vo.DeregisterInstanceParam) (bool, error) {
	f.deregistered = append(f.deregistered, p)
	return true, nil
}
func (f *fakeNaming) SelectAllInstances(vo.SelectAllInstancesParam) ([]model.Instance, error) {
	return f.instances, nil
}
func (f *fakeNaming) Subscribe(*vo.SubscribeParam) error { return nil }

func TestRegistryAndResolverGoThroughNacosx(t *testing.T) {
	logs := zlogtest.Capture(t)
	t.Setenv("APP_ENV", "test") // not a developer's machine, which registers in its own Nacos only
	naming := &fakeNaming{}
	cli := nacosx.NewFrom(nacosx.Config{Addrs: []string{"n1:8848"}}, naming, nil)

	reg := newNacosRegistry(cli, "")
	info := &registry.Info{ServiceName: "order", Addr: utils.NewNetAddr("tcp", "10.0.0.5:8888"), Weight: 20, Tags: map[string]string{"zone": "a"}}
	if err := reg.Register(info); err != nil {
		t.Fatal(err)
	}
	if p := naming.registered[0]; p.ServiceName != "order" || p.Ip != "10.0.0.5" || p.Port != 8888 || p.Weight != 20 || p.Metadata["zone"] != "a" {
		t.Errorf("registered as %+v", p)
	}
	if err := reg.Deregister(info); err != nil || len(naming.deregistered) != 1 {
		t.Fatalf("deregister: %v %d", err, len(naming.deregistered))
	}
	if !logs.Has("INFO", "registered in Nacos") || !logs.Has("INFO", "deregistered from Nacos") {
		t.Errorf("records: %s", logs)
	}

	res := newNacosResolver(cli, nacosx.Config{})
	if _, err := res.Resolve(context.Background(), "pay"); err == nil {
		t.Error("no instance must be an error, or Kitex caches an empty list")
	}
	naming.instances = []model.Instance{
		{Ip: "10.0.0.7", Port: 8888, Weight: 10, Healthy: true, Enable: true, Metadata: map[string]string{"zone": "b"}},
		{Ip: "10.0.0.8", Port: 8888, Weight: 10, Healthy: false, Enable: true},
	}
	got, err := res.Resolve(context.Background(), "pay")
	if err != nil || len(got.Instances) != 1 || !got.Cacheable || got.CacheKey != "pay" {
		t.Fatalf("resolve: %+v %v", got, err)
	}
	in := got.Instances[0]
	if zone, _ := in.Tag("zone"); in.Address().String() != "10.0.0.7:8888" || in.Weight() != 10 || zone != "b" {
		t.Errorf("instance: %v", in)
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
		{"APP_ENV not set", "", "", []string{"      service: order (from service.name)\n", "      env: local (APP_ENV is not set)\n"}},
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

// A container is reached through the host's address and a mapped port: what is
// registered then is service.advertise, not what the process listens on.
func TestAdvertisedAddressIsWhatGetsRegistered(t *testing.T) {
	zlogtest.Discard(t)
	t.Setenv("APP_ENV", "dev")
	for in, want := range map[string]string{
		"":                "",
		"10.0.0.5":        "10.0.0.5:8888",
		"10.0.0.5:30888":  "10.0.0.5:30888",
		"host.dev:30888":  "host.dev:30888",
		" 10.0.0.5 ":      "10.0.0.5:8888",
		"[fd00::5]:30888": "[fd00::5]:30888",
	} {
		if got, err := advertiseAddr(in, 8888); err != nil || got != want {
			t.Errorf("advertiseAddr(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{":30888", "0.0.0.0:30888", "10.0.0.5:0", "10.0.0.5:x", "a:b:c"} {
		if _, err := advertiseAddr(in, 8888); err == nil || !strings.Contains(err.Error(), "service.advertise") {
			t.Errorf("advertiseAddr(%q) must fail and name the setting: %v", in, err)
		}
	}

	naming := &fakeNaming{}
	cli := nacosx.NewFrom(nacosx.Config{Addrs: []string{"n1:8848"}}, naming, nil)
	reg := newNacosRegistry(cli, "10.0.0.5:30888")
	info := &registry.Info{ServiceName: "order", Addr: utils.NewNetAddr("tcp", "[::]:8888")}
	if err := reg.Register(info); err != nil {
		t.Fatal(err)
	}
	if p := naming.registered[0]; p.Ip != "10.0.0.5" || p.Port != 30888 {
		t.Errorf("registered as %s:%d", p.Ip, p.Port)
	}
	if err := reg.Deregister(info); err != nil || len(naming.deregistered) != 1 || naming.deregistered[0].Port != 30888 {
		t.Errorf("deregister: %v %+v", err, naming.deregistered)
	}
}

// Options refuses before anything is started.
func TestOptionsRefusesALaptopInASharedNacos(t *testing.T) {
	t.Setenv("APP_ENV", "")
	var cfg Config
	cfg.Service.Name = "order"
	cfg.Log.Output = &bytes.Buffer{}
	cfg.Nacos.Addrs = []string{"10.0.0.5:8848"}
	_, err := Options(cfg)
	if err == nil || !strings.Contains(err.Error(), "nacos.register: false") {
		t.Fatalf("err = %v", err)
	}
}
