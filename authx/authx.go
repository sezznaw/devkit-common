// Package authx is the login state of the platform: who issued a token, how
// a gateway checks it, and the session keys in Redis both share. One
// authentication service (ser-auth) issues tokens for every kind of
// principal (realm: member, admin, agent); each gateway verifies the tokens
// of its own realm locally (JWT signature) plus one Redis read (is the
// session still alive), and puts the identity into the context, from where
// kitexx carries it to the RPC services (kitexx.UID). Authorization (what a
// principal may do) is not here: it belongs to the domain that owns the
// resource.
package authx

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/redis/go-redis/v9"

	"github.com/sezznaw/devkit-common/config"
)

// Realm is the kind of principal a token stands for. Each gateway accepts
// one realm; a member's token is never valid at the staff gateway.
type Realm string

const (
	RealmMember Realm = "member"
	RealmAdmin  Realm = "admin"
	RealmAgent  Realm = "agent"
)

// Config is the `auth:` section. The issuing service and every gateway
// share secret and issuer; a gateway also says which realm it serves.
type Config struct {
	// Enabled switches the section on (the gateway's RequireLogin, the
	// issuing service's Issuer).
	Enabled bool `yaml:"enabled"`
	// Secret signs the access tokens (HS256), at least 32 bytes, from the
	// environment: "${AUTH_JWT_SECRET}". The same value in ser-auth and in
	// every gateway.
	Secret string `yaml:"secret"`
	// Issuer is the `iss` claim, default "sportsbook".
	Issuer string `yaml:"issuer"`
	// Realm is the principal kind this gateway accepts: member, admin or
	// agent. Required for a gateway; the issuing service leaves it empty.
	Realm Realm `yaml:"realm"`
	// AccessTTL is the life of an access token, default 15m.
	AccessTTL config.Duration `yaml:"access_ttl"`
	// RefreshTTL is the life of a refresh token, default 30 days (720h).
	RefreshTTL config.Duration `yaml:"refresh_ttl"`
	// SingleSession: one live session per principal of this realm; a new
	// login ends the previous one. Default true (the owner's rule for
	// members).
	SingleSession *bool `yaml:"single_session"`
}

const (
	defaultIssuer     = "sportsbook"
	defaultAccessTTL  = 15 * time.Minute
	defaultRefreshTTL = 30 * 24 * time.Hour
)

func (c Config) issuer() string {
	if c.Issuer == "" {
		return defaultIssuer
	}
	return c.Issuer
}

func (c Config) singleSession() bool { return c.SingleSession == nil || *c.SingleSession }

// Validate checks the section; verifier says whether a realm is required.
func (c Config) Validate(verifier bool) error {
	if !c.Enabled {
		return nil
	}
	if len(c.Secret) < 32 {
		return fmt.Errorf("authx: auth.secret must be at least 32 characters (it comes from AUTH_JWT_SECRET; is the variable set?)")
	}
	if verifier {
		switch c.Realm {
		case RealmMember, RealmAdmin, RealmAgent:
		default:
			return fmt.Errorf("authx: auth.realm %q: a gateway serves one of member, admin, agent", c.Realm)
		}
	}
	return nil
}

// Identity is who a request is from, as the gateway established it.
type Identity struct {
	Realm Realm
	UID   int64
	SID   string // session id
	JTI   string // access token id
	Exp   time.Time
}

// Tokens is what a login or refresh returns.
type Tokens struct {
	AccessToken      string
	RefreshToken     string
	AccessExpiresIn  int64 // seconds
	RefreshExpiresIn int64 // seconds
}

var (
	// ErrInvalidToken: the access token does not parse, is not ours, is
	// expired or is for another realm. Code 1004.
	ErrInvalidToken = errors.New("authx: invalid or expired token")
	// ErrSessionRevoked: the token is fine but its session has ended (logout,
	// a newer login, or a revocation). Code 1005.
	ErrSessionRevoked = errors.New("authx: session revoked")
	// ErrRefreshInvalid: the refresh token is unknown or expired. Code 1006.
	ErrRefreshInvalid = errors.New("authx: invalid refresh token")
	// ErrRefreshReplayed: a refresh token that was already rotated was used
	// again: a leak. Every session of the principal was revoked. Code 1006.
	ErrRefreshReplayed = errors.New("authx: refresh token replayed; all sessions revoked")
)

// Keys in Redis. They are ser-auth's data; the gateways read them through
// this package only.
func keySession(realm Realm, uid int64) string {
	return "auth:sess:" + string(realm) + ":" + strconv.FormatInt(uid, 10)
}
func keySessionSet(realm Realm, uid int64) string {
	return "auth:sessions:" + string(realm) + ":" + strconv.FormatInt(uid, 10)
}
func keyRefresh(hash string) string     { return "auth:rt:" + hash }
func keyUsed(hash string) string        { return "auth:rtused:" + hash }
func keyBlacklist(jti string) string    { return "auth:bl:" + jti }
func keySessionAlive(sid string) string { return "auth:sid:" + sid }

type claims struct {
	jwt.RegisteredClaims
	Realm Realm  `json:"typ"`
	SID   string `json:"sid"`
}

func randomID(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func hashToken(t string) string {
	h := sha256.Sum256([]byte(t))
	return hex.EncodeToString(h[:])
}

// Issuer is what the authentication service uses: it creates and ends
// sessions and signs tokens.
type Issuer struct {
	cfg Config
	rdb *redis.Client
	now func() time.Time
}

// NewIssuer builds the Issuer of the authentication service.
func NewIssuer(cfg Config, rdb *redis.Client) (*Issuer, error) {
	if err := cfg.Validate(false); err != nil {
		return nil, err
	}
	if rdb == nil {
		return nil, fmt.Errorf("authx: the issuer needs Redis (redis.enabled) for sessions")
	}
	return &Issuer{cfg: cfg, rdb: rdb, now: time.Now}, nil
}

type refreshRecord struct {
	Realm Realm  `json:"realm"`
	UID   int64  `json:"uid"`
	SID   string `json:"sid"`
}

// Login starts a session for a principal whose credentials the caller has
// already verified, and returns its tokens. With single_session the
// previous session of the principal ends.
func (i *Issuer) Login(ctx context.Context, realm Realm, uid int64) (Tokens, error) {
	sid := randomID(16)
	if i.cfg.singleSession() {
		if old, err := i.rdb.Get(ctx, keySession(realm, uid)).Result(); err == nil && old != "" {
			if err := i.endSession(ctx, old); err != nil {
				return Tokens{}, err
			}
		}
	}
	pipe := i.rdb.TxPipeline()
	pipe.Set(ctx, keySession(realm, uid), sid, i.cfg.RefreshTTL.Or(defaultRefreshTTL))
	pipe.SAdd(ctx, keySessionSet(realm, uid), sid)
	pipe.Expire(ctx, keySessionSet(realm, uid), i.cfg.RefreshTTL.Or(defaultRefreshTTL))
	pipe.Set(ctx, keySessionAlive(sid), strconv.FormatInt(uid, 10), i.cfg.RefreshTTL.Or(defaultRefreshTTL))
	if _, err := pipe.Exec(ctx); err != nil {
		return Tokens{}, fmt.Errorf("authx: start session: %w", err)
	}
	return i.issue(ctx, realm, uid, sid)
}

func (i *Issuer) issue(ctx context.Context, realm Realm, uid int64, sid string) (Tokens, error) {
	now := i.now()
	accessTTL, refreshTTL := i.cfg.AccessTTL.Or(defaultAccessTTL), i.cfg.RefreshTTL.Or(defaultRefreshTTL)
	jti := randomID(12)
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    i.cfg.issuer(),
			Subject:   strconv.FormatInt(uid, 10),
			ID:        jti,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(accessTTL)),
		},
		Realm: realm, SID: sid,
	})
	access, err := tok.SignedString([]byte(i.cfg.Secret))
	if err != nil {
		return Tokens{}, fmt.Errorf("authx: sign: %w", err)
	}
	refresh := randomID(32)
	rec, _ := json.Marshal(refreshRecord{Realm: realm, UID: uid, SID: sid})
	if err := i.rdb.Set(ctx, keyRefresh(hashToken(refresh)), rec, refreshTTL).Err(); err != nil {
		return Tokens{}, fmt.Errorf("authx: store refresh token: %w", err)
	}
	return Tokens{AccessToken: access, RefreshToken: refresh, AccessExpiresIn: int64(accessTTL.Seconds()), RefreshExpiresIn: int64(refreshTTL.Seconds())}, nil
}

// Refresh exchanges a refresh token for a new pair and retires the old one.
// A retired token presented again means it leaked: every session of the
// principal is revoked and ErrRefreshReplayed returned.
func (i *Issuer) Refresh(ctx context.Context, refreshToken string) (Tokens, error) {
	h := hashToken(refreshToken)
	raw, err := i.rdb.Get(ctx, keyRefresh(h)).Result()
	if errors.Is(err, redis.Nil) {
		if used, _ := i.rdb.Get(ctx, keyUsed(h)).Result(); used != "" {
			var rec refreshRecord
			if json.Unmarshal([]byte(used), &rec) == nil {
				_ = i.RevokeAll(ctx, rec.Realm, rec.UID)
			}
			return Tokens{}, ErrRefreshReplayed
		}
		return Tokens{}, ErrRefreshInvalid
	}
	if err != nil {
		return Tokens{}, fmt.Errorf("authx: refresh lookup: %w", err)
	}
	var rec refreshRecord
	if err := json.Unmarshal([]byte(raw), &rec); err != nil {
		return Tokens{}, ErrRefreshInvalid
	}
	// The session must still be alive (not logged out, not replaced).
	if alive, _ := i.rdb.Exists(ctx, keySessionAlive(rec.SID)).Result(); alive == 0 {
		i.rdb.Del(ctx, keyRefresh(h))
		return Tokens{}, ErrSessionRevoked
	}
	pipe := i.rdb.TxPipeline()
	pipe.Del(ctx, keyRefresh(h))
	pipe.Set(ctx, keyUsed(h), raw, i.cfg.RefreshTTL.Or(defaultRefreshTTL))
	if _, err := pipe.Exec(ctx); err != nil {
		return Tokens{}, fmt.Errorf("authx: rotate: %w", err)
	}
	return i.issue(ctx, rec.Realm, rec.UID, rec.SID)
}

// Logout ends the session the refresh token belongs to and blacklists the
// access token (jti) until it would have expired. Idempotent.
func (i *Issuer) Logout(ctx context.Context, refreshToken, accessToken string) error {
	h := hashToken(refreshToken)
	raw, err := i.rdb.Get(ctx, keyRefresh(h)).Result()
	if err == nil {
		var rec refreshRecord
		if json.Unmarshal([]byte(raw), &rec) == nil {
			if err := i.endSession(ctx, rec.SID); err != nil {
				return err
			}
			cur, _ := i.rdb.Get(ctx, keySession(rec.Realm, rec.UID)).Result()
			if cur == rec.SID {
				i.rdb.Del(ctx, keySession(rec.Realm, rec.UID))
			}
			i.rdb.SRem(ctx, keySessionSet(rec.Realm, rec.UID), rec.SID)
		}
		i.rdb.Del(ctx, keyRefresh(h))
	}
	if accessToken != "" {
		if c, err := parse(i.cfg, accessToken); err == nil && c.ExpiresAt != nil {
			if ttl := time.Until(c.ExpiresAt.Time); ttl > 0 {
				i.rdb.Set(ctx, keyBlacklist(c.ID), "1", ttl)
			}
		}
	}
	return nil
}

// RevokeAll ends every session of a principal: password changed, account
// frozen, kicked by staff. Access tokens die at the next request, because
// the gateway checks the session.
func (i *Issuer) RevokeAll(ctx context.Context, realm Realm, uid int64) error {
	sids, _ := i.rdb.SMembers(ctx, keySessionSet(realm, uid)).Result()
	for _, sid := range sids {
		if err := i.endSession(ctx, sid); err != nil {
			return err
		}
	}
	return i.rdb.Del(ctx, keySession(realm, uid), keySessionSet(realm, uid)).Err()
}

func (i *Issuer) endSession(ctx context.Context, sid string) error {
	return i.rdb.Del(ctx, keySessionAlive(sid)).Err()
}

func parse(cfg Config, token string) (*claims, error) {
	var c claims
	_, err := jwt.ParseWithClaims(token, &c, func(t *jwt.Token) (any, error) {
		if t.Method != jwt.SigningMethodHS256 {
			return nil, ErrInvalidToken
		}
		return []byte(cfg.Secret), nil
	}, jwt.WithIssuer(cfg.issuer()), jwt.WithExpirationRequired())
	if err != nil {
		return nil, ErrInvalidToken
	}
	return &c, nil
}

// Verifier is what a gateway uses on every request: signature and claims
// locally, then one Redis read to see that the session is alive and the
// token not blacklisted.
type Verifier struct {
	cfg Config
	rdb *redis.Client
}

// NewVerifier builds a gateway's Verifier for cfg.Realm.
func NewVerifier(cfg Config, rdb *redis.Client) (*Verifier, error) {
	if err := cfg.Validate(true); err != nil {
		return nil, err
	}
	if rdb == nil {
		return nil, fmt.Errorf("authx: the verifier needs Redis (redis.enabled) to check sessions")
	}
	return &Verifier{cfg: cfg, rdb: rdb}, nil
}

// Realm the verifier accepts.
func (v *Verifier) Realm() Realm { return v.cfg.Realm }

// Verify checks an access token and returns who it is.
func (v *Verifier) Verify(ctx context.Context, accessToken string) (Identity, error) {
	c, err := parse(v.cfg, accessToken)
	if err != nil {
		return Identity{}, err
	}
	if c.Realm != v.cfg.Realm {
		return Identity{}, ErrInvalidToken
	}
	uid, err := strconv.ParseInt(c.Subject, 10, 64)
	if err != nil || uid <= 0 {
		return Identity{}, ErrInvalidToken
	}
	pipe := v.rdb.Pipeline()
	bl := pipe.Exists(ctx, keyBlacklist(c.ID))
	alive := pipe.Exists(ctx, keySessionAlive(c.SID))
	if _, err := pipe.Exec(ctx); err != nil {
		return Identity{}, fmt.Errorf("authx: session check: %w", err)
	}
	if bl.Val() > 0 || alive.Val() == 0 {
		return Identity{}, ErrSessionRevoked
	}
	return Identity{Realm: c.Realm, UID: uid, SID: c.SID, JTI: c.ID, Exp: c.ExpiresAt.Time}, nil
}

// BearerToken extracts the token of an "Authorization: Bearer <token>" value.
func BearerToken(header string) string {
	if len(header) > 7 && strings.EqualFold(header[:7], "bearer ") {
		return strings.TrimSpace(header[7:])
	}
	return ""
}
