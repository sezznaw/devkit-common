package authx

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/golang-jwt/jwt/v5"
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

// EdDSA: the issuer holds the private key, a gateway only public keys; the migration from HS256 accepts both
// while the gateway still has the secret, and only EdDSA once it is gone. A token "signed" with a public key as
// an HMAC secret, a key the gateway does not list, or an unknown kid is invalid.
func TestEdDSA(t *testing.T) {
	srv := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: srv.Addr()})
	t.Cleanup(func() { rdb.Close() })
	ctx := context.Background()
	priv, pub, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	secret := "0123456789abcdef0123456789abcdef"
	legacy, err := NewIssuer(Config{Enabled: true, Secret: secret}, rdb)
	if err != nil {
		t.Fatal(err)
	}
	iss, err := NewIssuer(Config{Enabled: true, PrivateKey: priv, Secret: secret}, rdb)
	if err != nil {
		t.Fatal(err)
	}
	both, err := NewVerifier(Config{Enabled: true, Realm: RealmMember, PublicKeys: []string{pub}, Secret: secret}, rdb)
	if err != nil {
		t.Fatal(err)
	}
	only, err := NewVerifier(Config{Enabled: true, Realm: RealmMember, PublicKeys: []string{"", pub}}, rdb)
	if err != nil {
		t.Fatal(err)
	}
	old, _ := legacy.Login(ctx, RealmMember, 7)
	cur, _ := iss.Login(ctx, RealmMember, 8)
	if !strings.HasPrefix(cur.AccessToken, "eyJhbGciOiJFZERTQSIs") {
		t.Fatalf("the issuer with a private key signs EdDSA: %s", cur.AccessToken[:24])
	}
	if id, err := both.Verify(ctx, old.AccessToken); err != nil || id.UID != 7 {
		t.Fatalf("migration: HS256 still accepted with the secret: %+v %v", id, err)
	}
	for name, v := range map[string]*Verifier{"both": both, "only": only} {
		if id, err := v.Verify(ctx, cur.AccessToken); err != nil || id.UID != 8 {
			t.Fatalf("%s: EdDSA: %+v %v", name, id, err)
		}
	}
	if _, err := only.Verify(ctx, old.AccessToken); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("without the secret HS256 is refused: %v", err)
	}
	// the issuer's own Logout reads its EdDSA tokens
	if err := iss.Logout(ctx, cur.RefreshToken, cur.AccessToken); err != nil {
		t.Fatal(err)
	}
	if _, err := only.Verify(ctx, cur.AccessToken); !errors.Is(err, ErrSessionRevoked) {
		t.Fatalf("after logout: %v", err)
	}

	// alg confusion: HS256 with the public key bytes as the secret
	pubRaw, _ := base64.StdEncoding.DecodeString(pub)
	forged, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, claims{RegisteredClaims: jwt.RegisteredClaims{
		Issuer: "sportsbook", Subject: "1", ID: "x", ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))}, Realm: RealmMember, SID: "s"}).SignedString(pubRaw)
	if _, err := only.Verify(ctx, forged); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("alg confusion accepted: %v", err)
	}
	// another key, and an unknown kid
	otherPriv, _, _ := GenerateKey()
	stranger, _ := NewIssuer(Config{Enabled: true, PrivateKey: otherPriv}, rdb)
	foreign, _ := stranger.Login(ctx, RealmMember, 9)
	if _, err := only.Verify(ctx, foreign.AccessToken); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("a key the gateway does not list: %v", err)
	}
	seed, _ := base64.StdEncoding.DecodeString(otherPriv)
	tok := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims{RegisteredClaims: jwt.RegisteredClaims{
		Issuer: "sportsbook", Subject: "1", ID: "y", ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))}, Realm: RealmMember, SID: "s"})
	tok.Header["kid"] = KeyID(ed25519.PublicKey(pubRaw)) // claims the trusted key's id, signed with another
	spoofed, _ := tok.SignedString(ed25519.NewKeyFromSeed(seed))
	if _, err := only.Verify(ctx, spoofed); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("kid of a trusted key, signature of another: %v", err)
	}

	// configuration: a gateway needs keys or the secret, the issuer a private key or the secret; bad keys stop the start
	for name, c := range map[string]struct {
		cfg      Config
		verifier bool
	}{
		"gateway without keys": {Config{Enabled: true, Realm: RealmMember}, true},
		"issuer without keys":  {Config{Enabled: true, PublicKeys: []string{pub}}, false},
		"bad private key":      {Config{Enabled: true, PrivateKey: "bm90LWEta2V5"}, false},
		"bad public key":       {Config{Enabled: true, Realm: RealmMember, PublicKeys: []string{"bm90LWEta2V5"}}, true},
		"short secret":         {Config{Enabled: true, Realm: RealmMember, PublicKeys: []string{pub}, Secret: "short"}, true},
	} {
		if err := c.cfg.Validate(c.verifier); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
