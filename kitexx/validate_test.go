package kitexx

import (
	"context"
	"errors"
	"testing"

	"github.com/cloudwego/kitex/pkg/kerrors"
	"github.com/cloudwego/kitex/pkg/rpcinfo"
)

type validReq struct{ Username string }

func (r *validReq) IsValid() error {
	if len(r.Username) < 3 {
		return errors.New("field Username min_size rule failed, current value: " + itoa(len(r.Username)))
	}
	return nil
}

type validArgs struct{ Req *validReq }

func (a *validArgs) GetFirstArgument() any { return a.Req }

func itoa(n int) string { return string(rune('0' + n)) }

func TestValidationMiddleware(t *testing.T) {
	ran := false
	ep := ValidationMiddleware()(func(ctx context.Context, req, resp any) error { ran = true; return nil })
	// without rpcinfo (a bare test) the error comes back as is
	err := ep(context.Background(), &validArgs{Req: &validReq{Username: "ab"}}, nil)
	biz, ok := kerrors.FromBizStatusError(err)
	if !ok || biz.BizStatusCode() != CodeBadParam || biz.BizMessage() != "username: min_size rule failed, current value: 2" {
		t.Fatalf("invalid: %v", err)
	}
	// with rpcinfo (a real server) it is set as the business status and nil is returned,
	// which is what the client decodes as the business error
	inv := rpcinfo.NewInvocation("svc", "Method")
	ri := rpcinfo.NewRPCInfo(nil, nil, inv, rpcinfo.NewRPCConfig(), rpcinfo.NewRPCStats())
	rctx := rpcinfo.NewCtxWithRPCInfo(context.Background(), ri)
	if err := ep(rctx, &validArgs{Req: &validReq{Username: "ab"}}, nil); err != nil {
		t.Fatalf("with rpcinfo the middleware returns nil, got %v", err)
	}
	if got := inv.BizStatusErr(); got == nil || got.BizStatusCode() != CodeBadParam {
		t.Fatalf("biz status not set: %v", got)
	}
	if ran {
		t.Fatal("handler must not run")
	}
	if err := ep(context.Background(), &validArgs{Req: &validReq{Username: "alice"}}, nil); err != nil || !ran {
		t.Fatalf("valid: %v ran=%v", err, ran)
	}
	ran = false
	if err := ep(context.Background(), &struct{ X int }{1}, nil); err != nil || !ran {
		t.Fatal("a request without IsValid passes")
	}
}

func TestFriendlyValidation(t *testing.T) {
	cases := map[string]string{
		"field Username min_size rule failed, current value: 2":     "username: min_size rule failed, current value: 2",
		"field NewPassword_ min_size rule failed, current value: 1": "new_password: min_size rule failed, current value: 1",
		"field RequestId const rule failed":                         "request_id: const rule failed",
		"something else":                                            "something else",
	}
	for in, want := range cases {
		if got := FriendlyValidation(in); got != want {
			t.Errorf("%q: got %q want %q", in, got, want)
		}
	}
}
