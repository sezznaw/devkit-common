package kitexx

import (
	"context"
	"testing"

	"github.com/cloudwego/kitex/pkg/rpcinfo"

	"github.com/sezznaw/devkit-common/zlog/zlogtest"
)

func callerCtx(from, method string) context.Context {
	var fromInfo rpcinfo.EndpointInfo
	if from != "" {
		fromInfo = rpcinfo.NewEndpointInfo(from, "", nil, nil)
	}
	ri := rpcinfo.NewRPCInfo(fromInfo, rpcinfo.NewEndpointInfo("ser-wallet", method, nil, nil), rpcinfo.NewInvocation("ser-wallet", method), rpcinfo.NewRPCConfig(), rpcinfo.NewRPCStats())
	return rpcinfo.NewCtxWithRPCInfo(context.Background(), ri)
}

func TestCallers(t *testing.T) {
	zlogtest.Discard(t)
	cfg := CallersConfig{Rules: []CallerRule{
		{Methods: []string{"DebitBet", "CreditSettle"}, Allow: []string{"ser-bet"}},
		{Methods: []string{"ManualAdjust"}, Allow: []string{"ser-admin-api"}},
	}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	called := 0
	ep := Callers("ser-wallet", cfg)(func(ctx context.Context, req, resp interface{}) error { called++; return nil })
	cases := []struct {
		from, method string
		ok           bool
	}{
		{"ser-bet", "DebitBet", true},
		{"ser-bet", "CreditSettle", true},
		{"ser-admin-api", "DebitBet", false},
		{"", "DebitBet", false},
		{"ser-wallet", "DebitBet", true}, // the service itself (post-deploy checks)
		{"anyone", "GetWallet", true},    // not guarded
		{"ser-admin-api", "ManualAdjust", true},
		{"ser-bet", "ManualAdjust", false},
	}
	for _, c := range cases {
		ctx := callerCtx(c.from, c.method)
		err := ep(ctx, nil, nil)
		if c.ok && err != nil {
			t.Fatalf("%s -> %s refused: %v", c.from, c.method, err)
		}
		if !c.ok && busyCode(ctx) != CodeForbidden {
			t.Fatalf("%s -> %s: want 1007, got %v / %d", c.from, c.method, err, busyCode(ctx))
		}
	}
	if called != 5 {
		t.Fatalf("called %d", called)
	}
	// no rules: a pass-through
	ep = Callers("x", CallersConfig{})(func(ctx context.Context, req, resp interface{}) error { called++; return nil })
	if err := ep(callerCtx("", "Anything"), nil, nil); err != nil || called != 6 {
		t.Fatal(err)
	}
	if (CallersConfig{Rules: []CallerRule{{Methods: nil, Allow: []string{"a"}}}}).Validate() == nil {
		t.Fatal("empty methods accepted")
	}
}

// default deny: an unlisted method takes only this service; "*" covers every
// method or any caller; log lets through; the default comes from the
// environment when the conf has none.
func TestCallersDefault(t *testing.T) {
	zlogtest.Discard(t)
	rules := []CallerRule{
		{Methods: []string{"DebitBet"}, Allow: []string{"ser-bet"}},
		{Methods: []string{"Ping"}, Allow: []string{"*"}},
		{Methods: []string{"*"}, Allow: []string{"ser-admin"}},
	}
	run := func(cfg CallersConfig, from, method string) bool {
		ctx := callerCtx(from, method)
		_ = Callers("ser-wallet", cfg)(func(ctx context.Context, req, resp interface{}) error { return nil })(ctx, nil, nil)
		return busyCode(ctx) == 0
	}
	deny := CallersConfig{Default: CallersDeny, Rules: rules}
	for _, c := range []struct {
		from, method string
		ok           bool
	}{
		{"ser-bet", "DebitBet", true},
		{"ser-admin", "DebitBet", true},   // "*" methods
		{"ser-admin", "GetWallet", true},  // "*" methods, unlisted otherwise
		{"ser-bet", "GetWallet", false},   // unlisted
		{"", "GetWallet", false},          // unlisted, unnamed
		{"ser-wallet", "GetWallet", true}, // itself
		{"anyone", "Ping", true},          // "*" caller
		{"ser-api", "DebitBet", false},
	} {
		if got := run(deny, c.from, c.method); got != c.ok {
			t.Fatalf("deny: %s -> %s: allowed=%v", c.from, c.method, got)
		}
	}
	if !run(CallersConfig{Default: CallersLog}, "ser-bet", "GetWallet") {
		t.Fatal("log refused an unlisted method")
	}
	if !run(CallersConfig{}, "ser-bet", "GetWallet") {
		t.Fatal("allow refused")
	}
	t.Setenv("CALLERS_DEFAULT", "deny")
	if run(CallersConfig{}, "ser-bet", "GetWallet") {
		t.Fatal("CALLERS_DEFAULT=deny let an unlisted method through")
	}
	if !run(CallersConfig{Default: CallersAllow}, "ser-bet", "GetWallet") {
		t.Fatal("conf allow did not win over the environment")
	}
	if (CallersConfig{Default: "strict"}).Validate() == nil {
		t.Fatal("bad default accepted")
	}
	if !deny.Covers("DebitBet") || !deny.Covers("Anything") || (CallersConfig{Rules: rules[:1]}).Covers("GetWallet") {
		t.Fatal("Covers")
	}
}
