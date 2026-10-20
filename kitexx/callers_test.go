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
