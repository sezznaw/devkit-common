package kitexx

import (
	"context"
	"testing"
	"time"

	"github.com/cloudwego/kitex/pkg/discovery"
	"github.com/cloudwego/kitex/pkg/rpcinfo"

	"github.com/sezznaw/devkit-common/devroute"
	"github.com/sezznaw/devkit-common/nacosx"
)

func TestMergeInstancesDropsTaggedBaselineAndBadTags(t *testing.T) {
	baseline := []nacosx.Instance{
		{Addr: "10.0.0.1:8100", Weight: 10},
		{Addr: "10.0.0.9:8100", Weight: 10, Metadata: map[string]string{devroute.TagMetadata: "sneaky"}},
	}
	takeovers := []nacosx.Instance{
		{Addr: "172.31.41.180:23001", Weight: 10, Metadata: map[string]string{devroute.TagMetadata: "alice"}},
		{Addr: "172.31.41.180:23002", Weight: 10, Metadata: map[string]string{devroute.TagMetadata: "Bad Tag"}},
		{Addr: "172.31.41.180:23003", Weight: 10},
	}
	got := mergeInstances(baseline, takeovers)
	if len(got) != 2 {
		t.Fatalf("want baseline 10.0.0.1 and alice only, got %d: %v", len(got), got)
	}
	if a := got[0].Address().String(); a != "10.0.0.1:8100" {
		t.Errorf("baseline: %s", a)
	}
	if tag, _ := got[1].Tag(devroute.TagMetadata); tag != "alice" {
		t.Errorf("takeover tag: %q", tag)
	}
}

func TestDiffSeesTagChange(t *testing.T) {
	a := discovery.Result{Instances: []discovery.Instance{discovery.NewInstance("tcp", "172.31.41.180:23001", 10, map[string]string{devroute.TagMetadata: "alice"})}}
	b := discovery.Result{Instances: []discovery.Instance{discovery.NewInstance("tcp", "172.31.41.180:23001", 10, map[string]string{devroute.TagMetadata: "bob"})}}
	if _, changed := discovery.DefaultDiff("k", a, b); changed {
		t.Fatal("precondition: Kitex's default diff is blind to metadata")
	}
	if _, changed := diffWithTags("k", a, b); !changed {
		t.Error("tag change not seen")
	}
	if _, changed := diffWithTags("k", a, a); changed {
		t.Error("no change reported as change")
	}
}

func TestDevRouteTimeoutOnlyForTaggedRequests(t *testing.T) {
	mw := devRouteTimeout(devroute.Config{})
	seen := time.Duration(0)
	ep := mw(func(ctx context.Context, req, resp any) error {
		seen = rpcinfo.GetRPCInfo(ctx).Config().RPCTimeout()
		return nil
	})
	cfg := rpcinfo.NewRPCConfig()
	rpcinfo.AsMutableRPCConfig(cfg).SetRPCTimeout(3 * time.Second)
	ctx := rpcinfo.NewCtxWithRPCInfo(context.Background(), rpcinfo.NewRPCInfo(nil, nil, nil, cfg, rpcinfo.NewRPCStats()))
	_ = ep(ctx, nil, nil)
	if seen != 3*time.Second {
		t.Errorf("untagged request timeout changed to %v", seen)
	}
	_ = ep(devroute.WithTags(ctx, []string{"alice"}), nil, nil)
	if seen != devroute.DefaultDebugTimeout {
		t.Errorf("tagged request timeout %v, want %v", seen, devroute.DefaultDebugTimeout)
	}
}

func TestTakeoverResolver(t *testing.T) {
	r := &takeoverResolver{services: map[string]string{"ser-auth": "127.0.0.1:9120"}}
	res, err := r.Resolve(context.Background(), "ser-auth")
	if err != nil || len(res.Instances) != 1 || res.Instances[0].Address().String() != "127.0.0.1:9120" {
		t.Errorf("resolve: %v %v", res, err)
	}
	if _, err := r.Resolve(context.Background(), "ser-wallet"); err == nil {
		t.Error("unknown service resolved")
	}
}

func TestDevRouteClientOptionsOffByDefault(t *testing.T) {
	t.Setenv("APP_ENV", "prod")
	var cfg Config
	if opts := devRouteClientOptions(cfg); len(opts) != 0 {
		t.Errorf("prod got %d devroute client options", len(opts))
	}
	t.Setenv("APP_ENV", "dev")
	if opts := devRouteClientOptions(cfg); len(opts) != 2 {
		t.Errorf("dev got %d devroute client options, want balancer + timeout", len(opts))
	}
	off := false
	cfg.DevRoute.On = &off
	if opts := devRouteClientOptions(cfg); len(opts) != 0 {
		t.Error("devroute.enabled: false not honoured")
	}
}
