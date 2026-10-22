package kitexx

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/bytedance/gopkg/cloud/metainfo"
	"github.com/redis/go-redis/v9"

	"github.com/sezznaw/devkit-common/authx"
	"github.com/sezznaw/devkit-common/zlog/zlogtest"
)

// The login a call claims must be proven by the token it carries: log mode lets a failure through and counts
// it, enforce refuses it with 1004; a call that claims no login is not checked; off is a pass-through.
func TestIdentityCheck(t *testing.T) {
	zlogtest.Discard(t)
	priv, pub, err := authx.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	srv := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: srv.Addr()})
	t.Cleanup(func() { rdb.Close() })
	iss, err := authx.NewIssuer(authx.Config{Enabled: true, PrivateKey: priv}, rdb)
	if err != nil {
		t.Fatal(err)
	}
	ver, _ := authx.NewVerifier(authx.Config{Enabled: true, Realm: authx.RealmAdmin, PublicKeys: []string{pub}}, rdb)
	tk, _ := iss.Login(context.Background(), authx.RealmAdmin, 7)
	id, err := ver.Verify(context.Background(), tk.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	other, _ := iss.Login(context.Background(), authx.RealmAdmin, 8)
	otherPriv, _, _ := authx.GenerateKey()
	stranger, _ := authx.NewIssuer(authx.Config{Enabled: true, PrivateKey: otherPriv}, rdb)
	foreign, _ := stranger.Login(context.Background(), authx.RealmAdmin, 7)

	gateway := func(claim authx.Identity, token string) context.Context {
		ctx := callerCtx("ser-admin-api", "ListAdmins")
		ctx = WithIdentity(ctx, claim)
		if token != "" {
			ctx = WithToken(ctx, token)
		}
		return ctx
	}
	cases := []struct {
		name   string
		ctx    context.Context
		proven bool
	}{
		{"the gateway's call", gateway(id, tk.AccessToken), true},
		{"no login claimed (a job)", callerCtx("ser-admin", "ListAdmins"), true},
		{"a login claimed without a token", gateway(id, ""), false},
		{"another admin's token", gateway(id, other.AccessToken), false},
		{"uid 1 claimed with uid 7's token", gateway(authx.Identity{Realm: authx.RealmAdmin, UID: 1, SID: id.SID}, tk.AccessToken), false},
		{"realm switched", gateway(authx.Identity{Realm: "finance_admin", UID: 7, SID: id.SID}, tk.AccessToken), false},
		{"a token signed with a key not trusted", gateway(id, foreign.AccessToken), false},
		{"garbage", gateway(id, "a.b.c"), false},
	}
	for _, mode := range []string{IdentityLog, IdentityEnforce} {
		mw, err := IdentityCheck(IdentityConfig{Mode: mode, PublicKeys: []string{pub}})
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range cases {
			called := false
			err := mw(func(ctx context.Context, req, resp any) error { called = true; return nil })(c.ctx, nil, nil)
			refused := !called
			if mode == IdentityLog && refused {
				t.Fatalf("log mode refused %q: %v", c.name, err)
			}
			if mode == IdentityEnforce && refused == c.proven {
				t.Fatalf("enforce, %q: refused=%v (want %v) err=%v code=%d", c.name, refused, !c.proven, err, busyCode(c.ctx))
			}
			if mode == IdentityEnforce && refused && busyCode(c.ctx) != CodeUnauthenticated {
				t.Fatalf("enforce, %q: want 1004, got %d", c.name, busyCode(c.ctx))
			}
		}
	}

	// off (the default) is a pass-through and needs no key; a bad mode or a missing key stops the start
	if _, err := IdentityCheck(IdentityConfig{}); err != nil {
		t.Fatalf("off: %v", err)
	}
	if _, err := IdentityCheck(IdentityConfig{Mode: "strict"}); err == nil {
		t.Fatal("bad mode accepted")
	}
	t.Setenv("AUTH_JWT_PUBLIC_KEY", "")
	if _, err := IdentityCheck(IdentityConfig{Mode: IdentityLog}); err == nil {
		t.Fatal("log without a key accepted")
	}
	// the environment: IDENTITY_CHECK and AUTH_JWT_PUBLIC_KEY fill an empty section
	t.Setenv("IDENTITY_CHECK", IdentityEnforce)
	t.Setenv("AUTH_JWT_PUBLIC_KEY", pub)
	mw, err := IdentityCheck(IdentityConfig{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := gateway(id, "")
	if err := mw(func(ctx context.Context, req, resp any) error { return nil })(ctx, nil, nil); busyCode(ctx) != CodeUnauthenticated {
		t.Fatalf("enforce from the environment: %v %d", err, busyCode(ctx))
	}
	_ = metainfo.TransferForward // the persistent values travel to the next hop like the trace id
}
