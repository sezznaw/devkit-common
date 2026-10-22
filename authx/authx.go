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
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/redis/go-redis/v9"

	"github.com/sezznaw/devkit-common/config"
)

// Realm is the kind of principal a token stands for. Each gateway accepts
// one realm; a member's token is never valid at the staff gateway. The
// three below are the usual ones; a project names more (a second back
// office with its own accounts, say "finance_admin") by using them in the
// issuing service and in a gateway's auth.realm: a realm is any name of
// lowercase letters, digits and underscores, starting with a letter, up to
// 32 characters (it goes into Redis keys and the token).
type Realm string

// realmName is the shape of a realm name.
var realmName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

// Valid says whether r is a usable realm name.
func (r Realm) Valid() bool { return realmName.MatchString(string(r)) }

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
	// PrivateKey signs the access tokens with Ed25519 (EdDSA): the base64 of
	// the 32-byte seed, from the environment ("${AUTH_JWT_PRIVATE_KEY}"), in
	// the issuing service only. A gateway holds public keys alone, so one
	// taken over cannot sign a token. `go run .../cmd/authkey` makes a pair.
	PrivateKey string `yaml:"private_key"`
	// PublicKeys verify EdDSA tokens: base64 of 32-byte Ed25519 public keys,
	// the token's `kid` header picks one; more than one while a key is
	// rotated. Not secret (deployment values or conf). The issuer always
	// accepts its own key.
	PublicKeys []string `yaml:"public_keys"`
	// Secret is the legacy shared HS256 key (at least 32 bytes, from
	// "${AUTH_JWT_SECRET}"): with it set, HS256 tokens are signed (by an
	// issuer without a private key) and accepted. Leave it empty once every
	// live token is EdDSA, so that nothing holding it can mint tokens.
	Secret string `yaml:"secret"`
	// Issuer is the `iss` claim, default "sportsbook".
	Issuer string `yaml:"issuer"`
	// Realm is the principal kind this gateway accepts: member, admin,
	// agent, or a realm of the project's own (see Realm). Required for a
	// gateway; the issuing service leaves it empty.
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
	k, err := loadKeys(c)
	if err != nil {
		return err
	}
	switch {
	case !verifier && k.private == nil && k.secret == nil:
		return fmt.Errorf("authx: the issuer needs auth.private_key (AUTH_JWT_PRIVATE_KEY, an Ed25519 seed) or the legacy auth.secret; is the variable set?")
	case verifier && len(k.public) == 0 && k.secret == nil:
		return fmt.Errorf("authx: a gateway needs auth.public_keys (AUTH_JWT_PUBLIC_KEY) or the legacy auth.secret; is the variable set?")
	}
	if verifier {
		if c.Realm == "" {
			return fmt.Errorf("authx: auth.realm is required for a gateway (member, admin, agent, or a realm of the project's own)")
		}
		if !c.Realm.Valid() {
			return fmt.Errorf("authx: auth.realm %q: lowercase letters, digits and underscores, starting with a letter, at most 32 characters", c.Realm)
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
func keyEnded(sid string) string        { return "auth:ended:" + sid } // why the session ended, kept as long as a refresh token could live

// keys sign and verify access tokens.
type keys struct {
	secret  []byte                       // HS256 (legacy); nil when not configured
	private ed25519.PrivateKey           // EdDSA signing key: the issuer only
	kid     string                       // key id of private
	public  map[string]ed25519.PublicKey // kid -> verifying key
}

// KeyID names a public key in the `kid` header: the first 8 hex digits of
// its SHA-256.
func KeyID(pub ed25519.PublicKey) string {
	h := sha256.Sum256(pub)
	return hex.EncodeToString(h[:4])
}

// GenerateKey makes an Ed25519 pair in the configuration's encoding:
// private (base64 seed, for AUTH_JWT_PRIVATE_KEY) and public (base64, for
// AUTH_JWT_PUBLIC_KEY / auth.public_keys).
func GenerateKey() (private, public string, err error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", err
	}
	return base64.StdEncoding.EncodeToString(priv.Seed()), base64.StdEncoding.EncodeToString(pub), nil
}

func loadKeys(c Config) (keys, error) {
	k := keys{public: map[string]ed25519.PublicKey{}}
	if s := strings.TrimSpace(c.Secret); s != "" {
		if len(s) < 32 {
			return k, fmt.Errorf("authx: auth.secret must be at least 32 characters, or empty")
		}
		k.secret = []byte(s)
	}
	if s := strings.TrimSpace(c.PrivateKey); s != "" {
		seed, err := base64.StdEncoding.DecodeString(s)
		if err != nil || len(seed) != ed25519.SeedSize {
			return k, fmt.Errorf("authx: auth.private_key must be the base64 of a 32-byte Ed25519 seed")
		}
		k.private = ed25519.NewKeyFromSeed(seed)
		pub := k.private.Public().(ed25519.PublicKey)
		k.kid = KeyID(pub)
		k.public[k.kid] = pub
	}
	for _, s := range c.PublicKeys {
		s = strings.TrimSpace(s)
		if s == "" {
			continue // an unset variable in a list
		}
		raw, err := base64.StdEncoding.DecodeString(s)
		if err != nil || len(raw) != ed25519.PublicKeySize {
			return k, fmt.Errorf("authx: auth.public_keys: %q is not the base64 of a 32-byte Ed25519 public key", s)
		}
		pub := ed25519.PublicKey(raw)
		k.public[KeyID(pub)] = pub
	}
	return k, nil
}

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
	cfg  Config
	keys keys
	rdb  *redis.Client
	now  func() time.Time
}

// NewIssuer builds the Issuer of the authentication service.
func NewIssuer(cfg Config, rdb *redis.Client) (*Issuer, error) {
	if err := cfg.Validate(false); err != nil {
		return nil, err
	}
	if rdb == nil {
		return nil, fmt.Errorf("authx: the issuer needs Redis (redis.enabled) for sessions")
	}
	k, _ := loadKeys(cfg) // Validate read them already
	return &Issuer{cfg: cfg, keys: k, rdb: rdb, now: time.Now}, nil
}

// Why a session ended. The verifier hands the reason back with
// ErrSessionRevoked (RevokeReason) and the gateway puts it in the 1005
// answer, so the front end can tell "logged in elsewhere" from "your account
// was disabled". These three are the library's own; RevokeAll takes any
// string the project defines (disabled, password_reset, ...).
//
// 会话为什么结束：校验时随 ErrSessionRevoked 带回（RevokeReason），网关放进 1005 的响应里，前端据此区分
// 「在别处登录」和「账号被停用」。这三个是库自己写的；RevokeAll 的 reason 由项目自定（disabled、password_reset……）。
const (
	ReasonReplaced        = "replaced"         // single_session：同一主体的新登录顶掉了它
	ReasonLogout          = "logout"           // 主体自己登出
	ReasonRefreshReplayed = "refresh_replayed" // 作废的 refresh 被再次使用，判定泄露，全部会话吊销
)

// RevokedError is the ErrSessionRevoked of a session whose end was recorded
// with a reason. errors.Is(err, ErrSessionRevoked) stays true.
type RevokedError struct{ Reason string }

func (e *RevokedError) Error() string {
	if e.Reason == "" {
		return ErrSessionRevoked.Error()
	}
	return ErrSessionRevoked.Error() + ": " + e.Reason
}

// Unwrap makes errors.Is(err, ErrSessionRevoked) hold.
func (e *RevokedError) Unwrap() error { return ErrSessionRevoked }

// RevokeReason is why the session behind err ended; "" when unknown or when
// err is not a revocation.
func RevokeReason(err error) string {
	var r *RevokedError
	if errors.As(err, &r) {
		return r.Reason
	}
	return ""
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
			if err := i.endSession(ctx, old, ReasonReplaced); err != nil {
				return Tokens{}, err
			}
			i.rdb.SRem(ctx, keySessionSet(realm, uid), old)
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
	c := claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    i.cfg.issuer(),
			Subject:   strconv.FormatInt(uid, 10),
			ID:        jti,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(accessTTL)),
		},
		Realm: realm, SID: sid,
	}
	var access string
	var err error
	if i.keys.private != nil {
		tok := jwt.NewWithClaims(jwt.SigningMethodEdDSA, c)
		tok.Header["kid"] = i.keys.kid
		access, err = tok.SignedString(i.keys.private)
	} else {
		access, err = jwt.NewWithClaims(jwt.SigningMethodHS256, c).SignedString(i.keys.secret)
	}
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
				_ = i.RevokeAll(ctx, rec.Realm, rec.UID, ReasonRefreshReplayed)
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
		return Tokens{}, revoked(ctx, i.rdb, rec.SID)
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
			if err := i.endSession(ctx, rec.SID, ReasonLogout); err != nil {
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
		if c, err := parse(i.keys, i.cfg.issuer(), accessToken); err == nil && c.ExpiresAt != nil {
			if ttl := time.Until(c.ExpiresAt.Time); ttl > 0 {
				i.rdb.Set(ctx, keyBlacklist(c.ID), "1", ttl)
			}
		}
	}
	return nil
}

// RevokeAll ends every session of a principal: password changed, account
// frozen, kicked by staff. Access tokens die at the next request, because
// the gateway checks the session. reason is the project's word for why
// (disabled, password_reset, ...); the gateway's 1005 answer carries it.
// Empty means unknown.
func (i *Issuer) RevokeAll(ctx context.Context, realm Realm, uid int64, reason string) error {
	sids, _ := i.rdb.SMembers(ctx, keySessionSet(realm, uid)).Result()
	for _, sid := range sids {
		if err := i.endSession(ctx, sid, reason); err != nil {
			return err
		}
	}
	return i.rdb.Del(ctx, keySession(realm, uid), keySessionSet(realm, uid)).Err()
}

// endSession kills the session and records why, for as long as a refresh
// token of it could still come back.
func (i *Issuer) endSession(ctx context.Context, sid, reason string) error {
	pipe := i.rdb.TxPipeline()
	pipe.Del(ctx, keySessionAlive(sid))
	if reason != "" {
		// 第一次记下的原因才是它结束的原因（被顶掉的会话之后再被整体吊销，不改写成后者）。
		pipe.SetNX(ctx, keyEnded(sid), reason, i.cfg.RefreshTTL.Or(defaultRefreshTTL))
	}
	_, err := pipe.Exec(ctx)
	return err
}

// revoked is the ErrSessionRevoked for sid, with the recorded reason when there is one.
func revoked(ctx context.Context, rdb *redis.Client, sid string) error {
	reason, _ := rdb.Get(ctx, keyEnded(sid)).Result()
	return &RevokedError{Reason: reason}
}

// parse checks a token's signature and standard claims. EdDSA is checked
// with the public key its `kid` names; HS256 only when a secret is
// configured (the legacy key). Any other algorithm, or HS256 without a
// secret, is invalid: a token "signed" with a public key as an HMAC secret
// is not accepted.
func parse(k keys, issuer, token string, opts ...jwt.ParserOption) (*claims, error) {
	var c claims
	_, err := jwt.ParseWithClaims(token, &c, func(t *jwt.Token) (any, error) {
		switch t.Method {
		case jwt.SigningMethodEdDSA:
			kid, _ := t.Header["kid"].(string)
			if pub, ok := k.public[kid]; ok {
				return pub, nil
			}
		case jwt.SigningMethodHS256:
			if k.secret != nil {
				return k.secret, nil
			}
		}
		return nil, ErrInvalidToken
	}, append([]jwt.ParserOption{jwt.WithIssuer(issuer), jwt.WithExpirationRequired(), jwt.WithValidMethods([]string{"EdDSA", "HS256"})}, opts...)...)
	if err != nil {
		return nil, ErrInvalidToken
	}
	return &c, nil
}

// Verifier is what a gateway uses on every request: signature and claims
// locally, then one Redis read to see that the session is alive and the
// token not blacklisted.
type Verifier struct {
	cfg  Config
	keys keys
	rdb  *redis.Client
}

// NewVerifier builds a gateway's Verifier for cfg.Realm.
func NewVerifier(cfg Config, rdb *redis.Client) (*Verifier, error) {
	if err := cfg.Validate(true); err != nil {
		return nil, err
	}
	if rdb == nil {
		return nil, fmt.Errorf("authx: the verifier needs Redis (redis.enabled) to check sessions")
	}
	k, _ := loadKeys(cfg) // Validate read them already
	return &Verifier{cfg: cfg, keys: k, rdb: rdb}, nil
}

// Realm the verifier accepts.
func (v *Verifier) Realm() Realm { return v.cfg.Realm }

// Verify checks an access token and returns who it is.
func (v *Verifier) Verify(ctx context.Context, accessToken string) (Identity, error) {
	c, err := parse(v.keys, v.cfg.issuer(), accessToken)
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
	if bl.Val() > 0 {
		return Identity{}, &RevokedError{Reason: ReasonLogout}
	}
	if alive.Val() == 0 {
		return Identity{}, revoked(ctx, v.rdb, c.SID)
	}
	return Identity{Realm: c.Realm, UID: uid, SID: c.SID, JTI: c.ID, Exp: c.ExpiresAt.Time}, nil
}

// TokenChecker checks an access token's signature and claims alone, with no
// session lookup: what an RPC service behind a gateway uses to see that the
// login a call claims is real (the gateway already checked the session). It
// holds public keys only and accepts EdDSA tokens only; a token that expired
// less than CheckLeeway ago still passes, for a call chain that was running
// when it did.
//
// TokenChecker 只验访问令牌的签名和声明、不查会话：网关后面的 RPC 服务用它确认调用声称的登录者是真的（会话网关已经查过）。
// 只持公钥、只认 EdDSA；过期不到 CheckLeeway 的仍放行（调用链途中到期）。
type TokenChecker struct {
	keys   keys
	issuer string
}

// CheckLeeway is how long after its expiry a token still passes TokenChecker.
const CheckLeeway = 30 * time.Second

// NewTokenChecker builds a checker from public keys (base64 Ed25519, as
// auth.public_keys) and the issuer ("" = the default).
func NewTokenChecker(publicKeys []string, issuer string) (*TokenChecker, error) {
	k, err := loadKeys(Config{PublicKeys: publicKeys})
	if err != nil {
		return nil, err
	}
	if len(k.public) == 0 {
		return nil, fmt.Errorf("authx: a token checker needs at least one public key (AUTH_JWT_PUBLIC_KEY)")
	}
	return &TokenChecker{keys: k, issuer: Config{Issuer: issuer}.issuer()}, nil
}

// Check returns who the token is for, or ErrInvalidToken.
func (c *TokenChecker) Check(token string) (Identity, error) {
	cl, err := parse(c.keys, c.issuer, token, jwt.WithLeeway(CheckLeeway))
	if err != nil {
		return Identity{}, err
	}
	uid, err := strconv.ParseInt(cl.Subject, 10, 64)
	if err != nil || uid <= 0 {
		return Identity{}, ErrInvalidToken
	}
	return Identity{Realm: cl.Realm, UID: uid, SID: cl.SID, JTI: cl.ID, Exp: cl.ExpiresAt.Time}, nil
}

// BearerToken extracts the token of an "Authorization: Bearer <token>" value.
func BearerToken(header string) string {
	if len(header) > 7 && strings.EqualFold(header[:7], "bearer ") {
		return strings.TrimSpace(header[7:])
	}
	return ""
}
