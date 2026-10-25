package kitexx

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cloudwego/kitex/pkg/rpcinfo"
	"github.com/cloudwego/kitex/pkg/serviceinfo"

	"github.com/alicebob/miniredis/v2"
	"github.com/cloudwego/kitex/pkg/kerrors"

	"github.com/sezznaw/devkit-common/redisx"
	"github.com/sezznaw/devkit-common/zlog/zlogtest"
)

// The shapes Kitex generates: a request with a request_id, the *Args wrapper
// and the *Result wrapper of a method.
type transferReq struct {
	RequestId string `json:"request_id"`
	Amount    int64  `json:"amount"`
}

func (r *transferReq) GetRequestId() string { return r.RequestId }

type transferResp struct {
	TxID string `json:"tx_id"`
}

type transferArgs struct{ Req *transferReq }

func (a *transferArgs) GetFirstArgument() any { return a.Req }

type transferResult struct{ Success *transferResp }

func (r *transferResult) GetResult() any { return r.Success }

type plainArgs struct{ Req *struct{ X int } }

func (a *plainArgs) GetFirstArgument() any { return a.Req }

func runtimeWithRedis(t *testing.T) *Runtime {
	t.Helper()
	srv := miniredis.RunT(t)
	cli, err := redisx.Open(context.Background(), redisx.Target{Addr: srv.Addr()}, redisx.Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cli.Close() })
	rt := &Runtime{Redis: cli}
	rt.Config.Service.Name = "wallet"
	return rt
}

func TestIdempotencyReplaysTheKeptResult(t *testing.T) {
	zlogtest.Discard(t)
	rt := runtimeWithRedis(t)
	runs := 0
	ep := rt.Idempotency()(func(ctx context.Context, req, resp any) error {
		runs++
		resp.(*transferResult).Success = &transferResp{TxID: "tx-1"}
		return nil
	})
	ctx := withRPCInfo(context.Background(), "wallet", "Transfer", "gateway")
	call := func() *transferResult {
		res := &transferResult{}
		if err := ep(ctx, &transferArgs{Req: &transferReq{RequestId: "r1", Amount: 5}}, res); err != nil {
			t.Fatal(err)
		}
		return res
	}
	if r := call(); r.Success.TxID != "tx-1" || runs != 1 {
		t.Fatalf("first call runs: %+v runs=%d", r.Success, runs)
	}
	if r := call(); r.Success.TxID != "tx-1" || runs != 1 {
		t.Fatalf("second call replays without running: %+v runs=%d", r.Success, runs)
	}
	// A different id is a different request.
	res := &transferResult{}
	if err := ep(ctx, &transferArgs{Req: &transferReq{RequestId: "r2"}}, res); err != nil || runs != 2 {
		t.Fatalf("new id runs: %v runs=%d", err, runs)
	}
}

// A handler that answers with a business error does not get its (empty)
// result kept: Kitex puts the error into the RPCInfo and returns nil to the
// middlewares, which looked like a success, so a retry with the same id got an
// empty success without running the handler (the 3004 below came back as a
// GrantBonus "success" with txn 0). The id is dropped and the retry runs.
func TestIdempotencyDoesNotKeepABusinessError(t *testing.T) {
	zlogtest.Discard(t)
	rt := runtimeWithRedis(t)
	runs := 0
	ep := rt.Idempotency()(func(ctx context.Context, req, resp any) error {
		runs++
		if runs == 1 {
			// what kitex's invokeHandleEndpoint does with a BizStatusError
			rpcinfo.GetRPCInfo(ctx).Invocation().(rpcinfo.InvocationSetter).SetBizStatusErr(kerrors.NewBizStatusError(3004, "conflict"))
			return nil
		}
		resp.(*transferResult).Success = &transferResp{TxID: "tx-2"}
		return nil
	})
	call := func() (*transferResult, rpcinfo.RPCInfo) {
		ctx := withRPCInfo(context.Background(), "wallet", "Transfer", "gateway") // a call has its own RPCInfo
		res := &transferResult{}
		if err := ep(ctx, &transferArgs{Req: &transferReq{RequestId: "r-biz", Amount: 5}}, res); err != nil {
			t.Fatal(err)
		}
		return res, rpcinfo.GetRPCInfo(ctx)
	}
	if res, ri := call(); res.Success != nil || ri.Invocation().BizStatusErr() == nil || runs != 1 {
		t.Fatalf("first call is the business error: %+v runs=%d", res.Success, runs)
	}
	if n, _ := rt.Redis.Exists(context.Background(), "idem:wallet:Transfer:r-biz").Result(); n != 0 {
		t.Fatal("a business error must not be kept")
	}
	if res, ri := call(); res.Success == nil || res.Success.TxID != "tx-2" || ri.Invocation().BizStatusErr() != nil || runs != 2 {
		t.Fatalf("the retry runs the handler again: %+v runs=%d", res.Success, runs)
	}
	if res, _ := call(); res.Success == nil || res.Success.TxID != "tx-2" || runs != 2 {
		t.Fatalf("the success is kept and replayed: %+v runs=%d", res.Success, runs)
	}
}

func TestIdempotencyRefusals(t *testing.T) {
	zlogtest.Discard(t)
	rt := runtimeWithRedis(t)
	ctx := withRPCInfo(context.Background(), "wallet", "Transfer", "")

	// Empty id.
	ep := rt.Idempotency()(func(ctx context.Context, req, resp any) error { return nil })
	if err := ep(ctx, &transferArgs{Req: &transferReq{}}, &transferResult{}); bizOf(ctx, err) != CodeRequestIDRequired {
		t.Errorf("empty request_id: %v", err)
	}

	// In progress: the first call has claimed the id and is still running.
	started := make(chan struct{})
	release := make(chan struct{})
	slow := rt.Idempotency()(func(ctx context.Context, req, resp any) error {
		close(started)
		<-release
		resp.(*transferResult).Success = &transferResp{TxID: "slow"}
		return nil
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = slow(ctx, &transferArgs{Req: &transferReq{RequestId: "r3"}}, &transferResult{})
	}()
	<-started
	if err := slow(ctx, &transferArgs{Req: &transferReq{RequestId: "r3"}}, &transferResult{}); bizOf(ctx, err) != CodeRequestInProgress {
		t.Errorf("while running: %v", err)
	}
	close(release)
	<-done // or its tail (a Set on the closed redis, logged) lands on the next test's logger

	// A handler error drops the id: the retry runs.
	runs := 0
	failing := rt.Idempotency()(func(ctx context.Context, req, resp any) error {
		runs++
		if runs == 1 {
			return errors.New("db down")
		}
		resp.(*transferResult).Success = &transferResp{TxID: "ok"}
		return nil
	})
	if err := failing(ctx, &transferArgs{Req: &transferReq{RequestId: "r4"}}, &transferResult{}); err == nil {
		t.Fatal("first attempt fails")
	}
	res := &transferResult{}
	if err := failing(ctx, &transferArgs{Req: &transferReq{RequestId: "r4"}}, res); err != nil || res.Success.TxID != "ok" || runs != 2 {
		t.Errorf("retry after an error runs again: %v %+v runs=%d", err, res.Success, runs)
	}

	// A method without request_id passes through untouched.
	ran := false
	plain := rt.Idempotency()(func(ctx context.Context, req, resp any) error { ran = true; return nil })
	if err := plain(ctx, &plainArgs{Req: &struct{ X int }{1}}, &transferResult{}); err != nil || !ran {
		t.Errorf("no request_id: %v ran=%v", err, ran)
	}

	// No Redis: an idempotent request is refused, a plain one is not.
	none := (&Runtime{}).Idempotency()(func(ctx context.Context, req, resp any) error { return nil })
	if err := none(ctx, &transferArgs{Req: &transferReq{RequestId: "r5"}}, &transferResult{}); bizOf(ctx, err) != CodeIdempotencyUnavailable {
		t.Errorf("without redis: %v", err)
	}
	if err := none(ctx, &plainArgs{Req: &struct{ X int }{1}}, &transferResult{}); err != nil {
		t.Errorf("without redis, plain request: %v", err)
	}
}

// bizOf reads the business code a middleware answered with: set on the
// RPC info (BizError) when there is one, else the returned error.
func bizOf(ctx context.Context, err error) int32 {
	if ri := rpcinfo.GetRPCInfo(ctx); ri != nil {
		if biz := ri.Invocation().BizStatusErr(); biz != nil {
			return biz.BizStatusCode()
		}
	}
	if biz, ok := kerrors.FromBizStatusError(err); ok {
		return biz.BizStatusCode()
	}
	return 0
}

func TestCheckIdempotencyStore(t *testing.T) {
	newResult := func() any { return &transferResult{} }
	svcs := map[string]*serviceinfo.ServiceInfo{"Wallet": {Methods: map[string]serviceinfo.MethodInfo{
		"Transfer": serviceinfo.NewMethodInfo(nil, func() any { return &transferArgs{} }, newResult, false),
		"Refund":   serviceinfo.NewMethodInfo(nil, func() any { return &transferArgs{} }, newResult, false),
		"Balance":  serviceinfo.NewMethodInfo(nil, func() any { return &plainArgs{} }, newResult, false),
	}}}

	err := (&Runtime{}).checkIdempotencyStore(svcs)
	if err == nil || !strings.Contains(err.Error(), "Refund, Transfer take a request_id") {
		t.Fatalf("no redis, idempotent methods: %v", err)
	}
	delete(svcs["Wallet"].Methods, "Transfer")
	delete(svcs["Wallet"].Methods, "Refund")
	if err := (&Runtime{}).checkIdempotencyStore(svcs); err != nil {
		t.Fatalf("no idempotent methods: %v", err)
	}
	svcs["Wallet"].Methods["Transfer"] = serviceinfo.NewMethodInfo(nil, func() any { return &transferArgs{} }, newResult, false)
	if err := runtimeWithRedis(t).checkIdempotencyStore(svcs); err != nil {
		t.Fatalf("with redis: %v", err)
	}
}
