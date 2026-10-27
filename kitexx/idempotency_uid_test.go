package kitexx

import (
	"context"
	"testing"

	"github.com/cloudwego/kitex/pkg/rpcinfo"
	"github.com/sezznaw/devkit-common/authx"
)

// The idempotency key of a request made by a login carries the login's uid:
// another member replaying the same request_id gets their own handler run,
// not this member's cached answer.
// 带登录者的幂等键带上登录者的 uid：别的会员拿同一个 request_id 重放，跑的是自己的 handler，拿不到这个会员的缓存结果。
func TestIdempotencyKeyIsPerLogin(t *testing.T) {
	ctx := rpcinfo.NewCtxWithRPCInfo(context.Background(), rpcinfo.NewRPCInfo(nil, rpcinfo.NewEndpointInfo("svc", "", nil, nil), rpcinfo.NewInvocation("svc", "DoIt"), nil, nil))
	plain := idempotencyKey(ctx, "svc", "r1")
	a := idempotencyKey(WithIdentity(ctx, authx.Identity{UID: 7, Realm: authx.RealmMember}), "svc", "r1")
	b := idempotencyKey(WithIdentity(ctx, authx.Identity{UID: 8, Realm: authx.RealmMember}), "svc", "r1")
	if plain != "idem:svc:DoIt:r1" || a != "idem:svc:DoIt:r1:u7" || b != "idem:svc:DoIt:r1:u8" {
		t.Fatalf("plain=%s a=%s b=%s", plain, a, b)
	}
}
