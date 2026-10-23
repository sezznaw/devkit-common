package kitexx

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/cloudwego/kitex/pkg/kerrors"
	"github.com/redis/go-redis/v9"

	"github.com/sezznaw/devkit-common/authx"
	"github.com/sezznaw/devkit-common/zlog/zlogtest"
)

// identity.require_login: a call from a gateway must claim a login unless the method is anonymous, and the
// login must be of the gateway's realm; services and jobs are not gateways. log counts, enforce refuses (1004).
func TestIdentityRequireLogin(t *testing.T) {
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
	bg := context.Background()
	memberVer, _ := authx.NewVerifier(authx.Config{Enabled: true, Realm: authx.RealmMember, PublicKeys: []string{pub}}, rdb)
	adminVer, _ := authx.NewVerifier(authx.Config{Enabled: true, Realm: authx.RealmAdmin, PublicKeys: []string{pub}}, rdb)
	mtk, _ := iss.Login(bg, authx.RealmMember, 7)
	member, err := memberVer.Verify(bg, mtk.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	atk, _ := iss.Login(bg, authx.RealmAdmin, 9)
	admin, err := adminVer.Verify(bg, atk.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	call := func(from, method string, id *authx.Identity, token string) context.Context {
		ctx := callerCtx(from, method)
		if id != nil {
			ctx = WithIdentity(ctx, *id)
			ctx = WithToken(ctx, token)
		}
		return ctx
	}
	// the TTHeader says ser-promo, the ServiceAccount token proved ser-api: the proven name counts
	proven := context.WithValue(callerCtx("ser-promo", "GetProfile"), verifiedCallerKey{}, "ser-api")
	cases := []struct {
		name string
		ctx  context.Context
		pass bool
	}{
		{"a gateway's call without a login", call("ser-api", "GetProfile", nil, ""), false},
		{"an anonymous method without a login", call("ser-api", "Register", nil, ""), true},
		{"a service's call without a login", call("ser-promo", "GetProfile", nil, ""), true},
		{"a job's call (no caller name)", call("", "GetProfile", nil, ""), true},
		{"the member's own login", call("ser-api", "GetProfile", &member, mtk.AccessToken), true},
		{"a member's login through the back office", call("ser-admin-api", "GetProfile", &member, mtk.AccessToken), false},
		{"an admin's login through the player gateway", call("ser-api", "GetProfile", &admin, atk.AccessToken), false},
		{"the admin's login through the back office", call("ser-admin-api", "AdminSetPassword", &admin, atk.AccessToken), true},
		{"the proven caller is a gateway", proven, false},
		{"a claim without a token is still refused", call("ser-api", "GetProfile", &member, ""), false},
	}
	gateways := map[string]string{"ser-api": "member", "ser-admin-api": "admin"}
	for _, rl := range []string{IdentityLog, IdentityEnforce} {
		mw, err := IdentityCheck(IdentityConfig{Mode: IdentityEnforce, RequireLogin: rl, Gateways: gateways, Anonymous: []string{"Register"}, PublicKeys: []string{pub}})
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range cases {
			called := false
			err := mw(func(ctx context.Context, req, resp any) error { called = true; return nil })(c.ctx, nil, nil)
			refused := !called
			switch {
			case rl == IdentityLog && refused && c.name != "a claim without a token is still refused":
				t.Fatalf("require_login log refused %q: %v", c.name, err)
			case rl == IdentityEnforce && refused == c.pass:
				t.Fatalf("require_login enforce, %q: refused=%v (want %v) err=%v code=%d", c.name, refused, !c.pass, err, busyCode(c.ctx))
			case rl == IdentityEnforce && refused && busyCode(c.ctx) != CodeUnauthenticated:
				t.Fatalf("enforce, %q: want 1004, got %d", c.name, busyCode(c.ctx))
			}
		}
	}

	// the configuration: require_login needs mode on and a gateway list; "*" in anonymous covers every method
	if _, err := IdentityCheck(IdentityConfig{Mode: IdentityLog, RequireLogin: IdentityEnforce, Gateways: gateways, PublicKeys: []string{pub}}); err == nil {
		t.Fatal("require_login enforce with mode log accepted")
	}
	if _, err := IdentityCheck(IdentityConfig{RequireLogin: IdentityLog, Gateways: gateways, PublicKeys: []string{pub}}); err == nil {
		t.Fatal("require_login without mode accepted")
	}
	t.Setenv("IDENTITY_GATEWAYS", "")
	if _, err := IdentityCheck(IdentityConfig{Mode: IdentityEnforce, RequireLogin: IdentityEnforce, PublicKeys: []string{pub}}); err == nil {
		t.Fatal("require_login without gateways accepted")
	}
	if _, err := IdentityCheck(IdentityConfig{Mode: IdentityEnforce, RequireLogin: IdentityEnforce, Gateways: map[string]string{"ser-api": "Member!"}, PublicKeys: []string{pub}}); err == nil {
		t.Fatal("a bad realm name accepted")
	}
	mw, err := IdentityCheck(IdentityConfig{Mode: IdentityEnforce, RequireLogin: IdentityEnforce, Gateways: gateways, Anonymous: []string{"*"}, PublicKeys: []string{pub}})
	if err != nil {
		t.Fatal(err)
	}
	called := false
	_ = mw(func(ctx context.Context, req, resp any) error { called = true; return nil })(call("ser-api", "GetProfile", nil, ""), nil, nil)
	if !called {
		t.Fatal(`anonymous "*" did not cover GetProfile`)
	}
	// the environment fills an empty section: IDENTITY_REQUIRE_LOGIN and IDENTITY_GATEWAYS
	t.Setenv("IDENTITY_CHECK", IdentityEnforce)
	t.Setenv("IDENTITY_REQUIRE_LOGIN", IdentityEnforce)
	t.Setenv("IDENTITY_GATEWAYS", "ser-api=member, ser-admin-api=admin,ser-finance-admin-api=finance_admin")
	t.Setenv("AUTH_JWT_PUBLIC_KEY", pub)
	mw, err = IdentityCheck(IdentityConfig{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := call("ser-finance-admin-api", "GetAdmin", nil, "")
	called = false
	_ = mw(func(ctx context.Context, req, resp any) error { called = true; return nil })(ctx, nil, nil)
	if called || busyCode(ctx) != CodeUnauthenticated {
		t.Fatalf("enforce from the environment: called=%v code=%d", called, busyCode(ctx))
	}
	t.Setenv("IDENTITY_GATEWAYS", "ser-api")
	if _, err := IdentityCheck(IdentityConfig{}); err == nil {
		t.Fatal("IDENTITY_GATEWAYS without =realm accepted")
	}
}

// LoginUID, CheckMemberUID and RequireRealm are what a handler asks instead of trusting a uid in the request.
func TestLoginHelpers(t *testing.T) {
	code := func(err error) int32 {
		if be, ok := kerrors.FromBizStatusError(err); ok {
			return be.BizStatusCode()
		}
		return 0
	}
	none := callerCtx("ser-api", "GetProfile")
	member := WithIdentity(callerCtx("ser-api", "GetProfile"), authx.Identity{Realm: authx.RealmMember, UID: 7, SID: "s"})
	admin := WithIdentity(callerCtx("ser-admin-api", "GetWallet"), authx.Identity{Realm: authx.RealmAdmin, UID: 9, SID: "a"})

	if _, err := LoginUID(none); code(err) != CodeUnauthenticated {
		t.Fatalf("LoginUID without a login: %v", err)
	}
	if uid, err := LoginUID(member); err != nil || uid != 7 {
		t.Fatalf("LoginUID: %d %v", uid, err)
	}

	if err := CheckMemberUID(member, 7); err != nil {
		t.Fatalf("a member about itself: %v", err)
	}
	if err := CheckMemberUID(member, 8); code(err) != CodeForbidden {
		t.Fatalf("a member about another: %v", err)
	}
	if err := CheckMemberUID(none, 8); err != nil {
		t.Fatalf("a service about any member: %v", err)
	}
	if err := CheckMemberUID(admin, 8); err != nil {
		t.Fatalf("the back office about any member: %v", err)
	}

	if err := RequireRealm(none, authx.RealmAdmin); code(err) != CodeUnauthenticated {
		t.Fatalf("RequireRealm without a login: %v", err)
	}
	if err := RequireRealm(member, authx.RealmAdmin, "finance_admin"); code(err) != CodeForbidden {
		t.Fatalf("RequireRealm with a member: %v", err)
	}
	if err := RequireRealm(admin, authx.RealmAdmin); err != nil {
		t.Fatalf("RequireRealm with an admin: %v", err)
	}
}
