package kitexx

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/kitex/pkg/rpcinfo"

	"github.com/sezznaw/devkit-common/zlog/zlogtest"
)

func limitedCtx() context.Context {
	ri := rpcinfo.NewRPCInfo(nil, rpcinfo.NewEndpointInfo("svc", "Ping", nil, nil), rpcinfo.NewInvocation("svc", "Ping"), rpcinfo.NewRPCConfig(), rpcinfo.NewRPCStats())
	return rpcinfo.NewCtxWithRPCInfo(context.Background(), ri)
}

func busyCode(ctx context.Context) int32 {
	if ri := rpcinfo.GetRPCInfo(ctx); ri != nil {
		if be := ri.Invocation().BizStatusErr(); be != nil {
			return be.BizStatusCode()
		}
	}
	return 0
}

func TestLimitsInFlight(t *testing.T) {
	zlogtest.Discard(t)
	release := make(chan struct{})
	var entered atomic.Int32
	slow := func(ctx context.Context, req, resp interface{}) error {
		entered.Add(1)
		<-release
		return nil
	}
	ep := Limits(LimitsConfig{MaxInFlight: 2})(slow)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = ep(limitedCtx(), nil, nil) }()
	}
	for entered.Load() < 2 {
		time.Sleep(time.Millisecond)
	}
	ctx := limitedCtx()
	if err := ep(ctx, nil, nil); err != nil || busyCode(ctx) != CodeBusy {
		t.Fatalf("third in flight: err=%v code=%d", err, busyCode(ctx))
	}
	close(release)
	wg.Wait()
	ctx = limitedCtx()
	if err := ep(ctx, nil, nil); err != nil || busyCode(ctx) != 0 {
		t.Fatalf("after release: err=%v code=%d", err, busyCode(ctx))
	}
}

func TestLimitsQPS(t *testing.T) {
	zlogtest.Discard(t)
	ok := func(ctx context.Context, req, resp interface{}) error { return nil }
	ep := Limits(LimitsConfig{MaxQPS: 50, MaxInFlight: -1})(ok) // 5 per 100ms window
	refused := 0
	for i := 0; i < 20; i++ {
		ctx := limitedCtx()
		_ = ep(ctx, nil, nil)
		if busyCode(ctx) == CodeBusy {
			refused++
		}
	}
	if refused < 10 || refused > 15 {
		t.Fatalf("refused %d of 20 within one window (want about 15)", refused)
	}
	time.Sleep(110 * time.Millisecond)
	ctx := limitedCtx()
	_ = ep(ctx, nil, nil)
	if busyCode(ctx) != 0 {
		t.Fatal("next window refused")
	}
}

func TestLimitsDefaults(t *testing.T) {
	var c LimitsConfig
	if c.inFlight() != 1000 || c.connections() != 10000 || c.MaxQPS != 0 {
		t.Fatalf("%+v", c)
	}
	c.MaxInFlight = -1
	if c.inFlight() != 0 {
		t.Fatal("-1 is off")
	}
}
