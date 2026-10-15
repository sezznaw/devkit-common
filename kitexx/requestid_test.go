package kitexx

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cloudwego/kitex/pkg/rpcinfo"

	"github.com/sezznaw/devkit-common/zlog/zlogtest"
)

// clientCtx is ctx as a client middleware sees it on a call to wallet.Debit.
func clientCtx(ctx context.Context) context.Context {
	ri := rpcinfo.NewRPCInfo(nil, rpcinfo.NewEndpointInfo("wallet", "Debit", nil, nil), rpcinfo.NewInvocation("wallet", "Debit"), rpcinfo.NewRPCConfig(), rpcinfo.NewRPCStats())
	return rpcinfo.NewCtxWithRPCInfo(ctx, ri)
}

func debit(id string) *transferArgs { return &transferArgs{Req: &transferReq{RequestId: id}} }

func TestClientRequestID(t *testing.T) {
	zlogtest.Discard(t)
	sent := 0
	var fail error
	call := ClientRequestID()(func(ctx context.Context, req, resp any) error { sent++; return fail })
	refused := func(err error, want string) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("want refusal %q, got %v", want, err)
		}
	}

	// outside a request: only the empty id is refused
	ctx := clientCtx(context.Background())
	refused(call(ctx, debit(""), nil), "wallet.Debit takes a request_id and the call has none")
	if err := call(ctx, debit("job-1"), nil); err != nil {
		t.Fatal(err)
	}
	if err := call(ctx, &plainArgs{}, nil); err != nil {
		t.Fatal(err)
	}

	// inside request r1
	ctx = clientCtx(WithRequestID(context.Background(), "r1"))
	if RequestID(ctx) != "r1" || SubRequestID(ctx, "fee") != "r1:fee" {
		t.Fatalf("RequestID %q, SubRequestID %q", RequestID(ctx), SubRequestID(ctx, "fee"))
	}
	refused(call(ctx, debit("3f2a-new-uuid"), nil), "not derived from the request being handled (r1)")
	refused(call(ctx, debit("r10"), nil), "not derived")
	if err := call(ctx, debit("r1"), nil); err != nil {
		t.Fatal(err)
	}
	refused(call(ctx, debit("r1"), nil), "a second time with the same request_id")
	if err := call(ctx, debit(SubRequestID(ctx, "fee")), nil); err != nil {
		t.Fatal(err)
	}

	// a failed call may be tried again with its id
	fail = errors.New("timeout")
	if err := call(ctx, debit("r1:stake"), nil); err != fail {
		t.Fatal(err)
	}
	fail = nil
	if err := call(ctx, debit("r1:stake"), nil); err != nil {
		t.Fatalf("retry after failure: %v", err)
	}
	if sent != 6 {
		t.Fatalf("%d calls sent, want 6", sent)
	}

	// another request has its own record
	other := clientCtx(WithRequestID(context.Background(), "r1"))
	if err := call(other, debit("r1"), nil); err != nil {
		t.Fatal(err)
	}
	if RequestID(context.Background()) != "" || SubRequestID(context.Background(), "x") != "" || RequestID(WithRequestID(context.Background(), "")) != "" {
		t.Fatal("no request, no id")
	}
}

// The server middleware hands the incoming id to the handler's ctx.
func TestIdempotencyScopesTheHandler(t *testing.T) {
	zlogtest.Discard(t)
	rt := runtimeWithRedis(t)
	var seen string
	h := rt.Idempotency()(func(ctx context.Context, req, resp any) error {
		seen = RequestID(ctx)
		return nil
	})
	if err := h(withRPCInfo(context.Background(), "wallet", "Transfer", "gateway"), &transferArgs{Req: &transferReq{RequestId: "abc"}}, &transferResult{}); err != nil {
		t.Fatal(err)
	}
	if seen != "abc" {
		t.Fatalf("handler saw request_id %q", seen)
	}
}
