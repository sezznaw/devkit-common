package authx

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/sezznaw/devkit-common/config"
)

func setup(t *testing.T) (*Issuer, *Verifier, *miniredis.Miniredis) {
	srv := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: srv.Addr()})
	t.Cleanup(func() { rdb.Close() })
	cfg := Config{Enabled: true, Secret: "0123456789abcdef0123456789abcdef", AccessTTL: config.Duration(time.Hour)}
	iss, err := NewIssuer(cfg, rdb)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Realm = RealmMember
	ver, err := NewVerifier(cfg, rdb)
	if err != nil {
		t.Fatal(err)
	}
	return iss, ver, srv
}

func TestLoginVerifyRefreshLogout(t *testing.T) {
	iss, ver, _ := setup(t)
	ctx := context.Background()
	tk, err := iss.Login(ctx, RealmMember, 42)
	if err != nil {
		t.Fatal(err)
	}
	id, err := ver.Verify(ctx, tk.AccessToken)
	if err != nil || id.UID != 42 || id.Realm != RealmMember {
		t.Fatalf("verify: %+v %v", id, err)
	}
	if _, err := ver.Verify(ctx, tk.AccessToken+"x"); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("tampered: %v", err)
	}
	// Refresh rotates: the new pair works, the old refresh token is retired.
	tk2, err := iss.Refresh(ctx, tk.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ver.Verify(ctx, tk2.AccessToken); err != nil {
		t.Fatalf("new access: %v", err)
	}
	// Replay of the retired token: a leak; everything revoked.
	if _, err := iss.Refresh(ctx, tk.RefreshToken); !errors.Is(err, ErrRefreshReplayed) {
		t.Fatalf("replay: %v", err)
	}
	if _, err := ver.Verify(ctx, tk2.AccessToken); !errors.Is(err, ErrSessionRevoked) || RevokeReason(err) != ReasonRefreshReplayed {
		t.Fatalf("after replay every session is dead: %v", err)
	}
	if _, err := iss.Refresh(ctx, "nope"); !errors.Is(err, ErrRefreshInvalid) {
		t.Errorf("unknown refresh: %v", err)
	}
	// Logout blacklists the access token and ends the session.
	tk3, _ := iss.Login(ctx, RealmMember, 42)
	if err := iss.Logout(ctx, tk3.RefreshToken, tk3.AccessToken); err != nil {
		t.Fatal(err)
	}
	if _, err := ver.Verify(ctx, tk3.AccessToken); !errors.Is(err, ErrSessionRevoked) || RevokeReason(err) != ReasonLogout {
		t.Errorf("after logout: %v", err)
	}
	if _, err := iss.Refresh(ctx, tk3.RefreshToken); err == nil {
		t.Error("refresh after logout must fail")
	}
	// a refresh of a session ended by RevokeAll says why as well
	tk4, _ := iss.Login(ctx, RealmMember, 42)
	iss.RevokeAll(ctx, RealmMember, 42, "password_reset")
	if _, err := iss.Refresh(ctx, tk4.RefreshToken); !errors.Is(err, ErrSessionRevoked) || RevokeReason(err) != "password_reset" {
		t.Errorf("refresh after revoke: %v", err)
	}
	if RevokeReason(ErrInvalidToken) != "" || RevokeReason(nil) != "" {
		t.Error("RevokeReason on a non-revocation")
	}
}

func TestSingleSessionAndRealm(t *testing.T) {
	iss, ver, _ := setup(t)
	ctx := context.Background()
	a, _ := iss.Login(ctx, RealmMember, 7)
	b, _ := iss.Login(ctx, RealmMember, 7) // second device: the first session ends
	if _, err := ver.Verify(ctx, a.AccessToken); !errors.Is(err, ErrSessionRevoked) {
		t.Errorf("first login should be kicked: %v", err)
	}
	if _, err := ver.Verify(ctx, b.AccessToken); err != nil {
		t.Errorf("second login alive: %v", err)
	}
	// An admin token is not accepted by the member gateway.
	adm, _ := iss.Login(ctx, RealmAdmin, 1)
	if _, err := ver.Verify(ctx, adm.AccessToken); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("wrong realm: %v", err)
	}
	// RevokeAll ends the live session.
	iss.RevokeAll(ctx, RealmMember, 7, "disabled")
	if _, err := ver.Verify(ctx, b.AccessToken); !errors.Is(err, ErrSessionRevoked) || RevokeReason(err) != "disabled" {
		t.Errorf("after revoke all: %v (reason %q)", err, RevokeReason(err))
	}
	// the reason the first login was kicked
	if _, err := ver.Verify(ctx, a.AccessToken); RevokeReason(err) != ReasonReplaced {
		t.Errorf("replaced reason: %v", err)
	}
	if BearerToken("Bearer abc") != "abc" || BearerToken("Basic x") != "" {
		t.Error("BearerToken")
	}
	if err := (Config{Enabled: true, Secret: "short"}).Validate(false); err == nil {
		t.Error("short secret accepted")
	}
}

func TestGatewayRealm(t *testing.T) {
	c := Config{Enabled: true, Secret: strings.Repeat("s", 32)}
	for _, r := range []Realm{RealmMember, RealmAdmin, RealmAgent, "finance_admin", "ops2"} {
		c.Realm = r
		if err := c.Validate(true); err != nil {
			t.Errorf("realm %q refused: %v", r, err)
		}
	}
	for _, r := range []Realm{"", "Admin", "2fa", "a-b", "a:b", Realm(strings.Repeat("a", 33))} {
		c.Realm = r
		if err := c.Validate(true); err == nil {
			t.Errorf("realm %q accepted", r)
		}
	}
	c.Realm = ""
	if err := c.Validate(false); err != nil {
		t.Errorf("the issuing service needs no realm: %v", err)
	}
}

func TestExpiredAccessToken(t *testing.T) {
	iss, ver, _ := setup(t)
	iss.cfg.AccessTTL = config.Duration(time.Millisecond)
	tk, _ := iss.Login(context.Background(), RealmMember, 1)
	time.Sleep(5 * time.Millisecond)
	if _, err := ver.Verify(context.Background(), tk.AccessToken); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("expired: %v", err)
	}
}
