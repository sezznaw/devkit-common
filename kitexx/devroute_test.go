package kitexx

import (
	"context"
	"github.com/cloudwego/kitex/pkg/loadbalance"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/kitex/client"
	"github.com/cloudwego/kitex/client/callopt"
	"github.com/cloudwego/kitex/pkg/discovery"

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
	got := mergeInstances(baseline, takeovers, nil)
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

func TestWithDebugTimeoutOnlyForTaggedRequests(t *testing.T) {
	ctx := context.Background()
	if got := WithDebugTimeout(ctx, time.Minute); len(client.CallOptionsFromCtx(got)) != 0 {
		t.Error("untagged request got call options")
	}
	tagged := devroute.WithTags(ctx, []string{"alice"})
	if got := WithDebugTimeout(tagged, time.Minute); len(client.CallOptionsFromCtx(got)) != 0 {
		t.Error("a tag nobody has a takeover for relaxed the timeout")
	}
	lb := devroute.NewBalancer().(loadbalance.Rebalancer)
	key := "debug-timeout-test"
	lb.Rebalance(discovery.Change{Result: discovery.Result{Cacheable: true, CacheKey: key, Instances: []discovery.Instance{discovery.NewInstance("tcp", "172.31.41.180:24001", 1, map[string]string{devroute.TagMetadata: "alice"})}}})
	t.Cleanup(func() { lb.Delete(discovery.Change{Result: discovery.Result{Cacheable: true, CacheKey: key}}) })
	got := WithDebugTimeout(tagged, time.Minute)
	if len(client.CallOptionsFromCtx(got)) != 1 {
		t.Fatal("tagged request has no call option")
	}
	// The handler's own call options win.
	own := client.NewCtxWithCallOptions(tagged, []callopt.Option{callopt.WithRPCTimeout(time.Second)})
	if got := WithDebugTimeout(own, time.Minute); len(client.CallOptionsFromCtx(got)) != 1 || &client.CallOptionsFromCtx(got)[0] == nil {
		t.Error("existing call options replaced")
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
	if opts := devRouteClientOptions(cfg); len(opts) != 1 {
		t.Errorf("dev got %d devroute client options, want the balancer", len(opts))
	}
	off := false
	cfg.DevRoute.On = &off
	if opts := devRouteClientOptions(cfg); len(opts) != 0 {
		t.Error("devroute.enabled: false not honoured")
	}
}

func TestInstanceGuardRefusesForeignBaselineAndOddTakeovers(t *testing.T) {
	_, pods, _ := net.ParseCIDR("10.42.0.0/16")
	g := &instanceGuard{baseline: []*net.IPNet{pods}}
	baseline := []nacosx.Instance{
		{Addr: "10.42.1.5:8100", Weight: 10},
		{Addr: "172.31.41.180:23456", Weight: 10}, // someone's reverse tunnel registered as a plain instance
		{Addr: "192.168.69.222:8100", Weight: 10},
	}
	takeovers := []nacosx.Instance{
		{Addr: "172.31.41.180:23001", Weight: 10, Metadata: map[string]string{devroute.TagMetadata: "alice"}},
		{Addr: "10.42.1.9:23001", Weight: 10, Metadata: map[string]string{devroute.TagMetadata: "bob"}},      // a Pod address claiming a tag
		{Addr: "172.31.41.180:8100", Weight: 10, Metadata: map[string]string{devroute.TagMetadata: "carol"}}, // off the tunnel port range
	}
	got := mergeInstances(baseline, takeovers, g)
	var addrs []string
	for _, in := range got {
		addrs = append(addrs, in.Address().String())
	}
	if strings.Join(addrs, " ") != "10.42.1.5:8100 172.31.41.180:23001" {
		t.Errorf("kept %v", addrs)
	}
	// No network known: baseline unchecked, the takeover port range still
	// enforced (carol on 8100 is out, bob's Pod address cannot be told apart).
	open := &instanceGuard{}
	if n := len(mergeInstances(baseline, takeovers, open)); n != 5 {
		t.Errorf("open guard kept %d, want 3 baseline + alice + bob", n)
	}
}
