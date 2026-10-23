package kitexx

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/cloudwego/kitex/pkg/endpoint"
	"github.com/cloudwego/kitex/pkg/kerrors"
	"github.com/cloudwego/kitex/pkg/rpcinfo"

	"github.com/bytedance/gopkg/cloud/metainfo"

	"github.com/sezznaw/devkit-common/authx"
	"github.com/sezznaw/devkit-common/metricsx"
	"github.com/sezznaw/devkit-common/zlog"
)

// The identity a gateway established travels to the RPC services as
// persistent metainfo (TTHeader), like the trace id: a handler anywhere in
// the call chain asks UID(ctx) and gets the member the request is for,
// without the field being in the request.
const (
	UIDKey   = "UID"
	RealmKey = "REALM"
	SIDKey   = "SID"
)

// WithIdentity puts id into ctx for this process and for every RPC made
// with ctx. The gateway's RequireLogin calls it; a gateway that has just
// logged a user in (and holds the tokens) calls it too, so that what it does
// next on that user's behalf carries the login.
func WithIdentity(ctx context.Context, id authx.Identity) context.Context {
	ctx = metainfo.WithPersistentValue(ctx, UIDKey, strconv.FormatInt(id.UID, 10))
	ctx = metainfo.WithPersistentValue(ctx, RealmKey, string(id.Realm))
	return metainfo.WithPersistentValue(ctx, SIDKey, id.SID)
}

// UID is the principal of the request (the logged-in member at the player
// gateway), or false on a request that came without a login (a public
// endpoint, a job, a consumer).
func UID(ctx context.Context) (int64, bool) {
	v, ok := metainfo.GetPersistentValue(ctx, UIDKey)
	if !ok {
		return 0, false
	}
	uid, err := strconv.ParseInt(v, 10, 64)
	return uid, err == nil && uid > 0
}

// Realm of the request's principal (member, admin, agent, or a realm of the
// project's own); "" without one.
func Realm(ctx context.Context) authx.Realm {
	v, _ := metainfo.GetPersistentValue(ctx, RealmKey)
	return authx.Realm(v)
}

// MustUID is UID for a handler whose endpoint requires a login (the
// gateway guarantees it): it panics when there is none, which means the
// endpoint was marked @public by mistake.
func MustUID(ctx context.Context) int64 {
	uid, ok := UID(ctx)
	if !ok {
		panic("kitexx: no login in the request context; the endpoint is public, or the gateway's RequireLogin is not installed")
	}
	return uid
}

// LoginUID is the principal of the request for an RPC method that works on
// the logged-in user (a profile, a binding, a password change): the uid the
// gateway's login established and identity: verified, never a field of the
// request. Without a login the business error 1004, which is what a call
// that lost its login deserves, and what a caller that was broken into and
// claims no login gets.
//
// LoginUID 给「对登录者本人办事」的 RPC 方法取 uid：来自网关建立、identity 核对过的登录态，不是请求体字段。
// 没有登录者时返回业务错误 1004。
func LoginUID(ctx context.Context) (int64, error) {
	uid, ok := UID(ctx)
	if !ok {
		// a handler returns it: Kitex turns a BizStatusError from the handler into the TTHeader status itself
		return 0, kerrors.NewBizStatusError(CodeUnauthenticated, "未登录")
	}
	return uid, nil
}

// CheckMemberUID guards a method that both a member (about itself) and the
// back office or another service (about any member) call with a uid in the
// request: a call that claims a member's login may only be about that
// member. A call without a login (a service) or with a back-office login
// passes. The error is 1007.
//
// CheckMemberUID 给玩家与后台（或别的服务）共用、请求体里带目标 uid 的方法：声称的是会员登录态时，
// 目标必须是本人，否则 1007；没有登录者（服务调用）或后台登录者放行。
func CheckMemberUID(ctx context.Context, uid int64) error {
	if Realm(ctx) != authx.RealmMember {
		return nil
	}
	if cur, ok := UID(ctx); ok && cur != uid {
		return kerrors.NewBizStatusError(CodeForbidden, "没有权限操作其他会员")
	}
	return nil
}

// RequireRealm refuses a call that does not carry a login of one of realms:
// 1004 without a login, 1007 with a login of another realm. For a method
// only a back office may call about a member.
//
// RequireRealm 要求调用带着指定 realm 之一的登录者：没有登录者 1004，别的 realm 1007。
func RequireRealm(ctx context.Context, realms ...authx.Realm) error {
	if _, ok := UID(ctx); !ok {
		return kerrors.NewBizStatusError(CodeUnauthenticated, "未登录")
	}
	cur := Realm(ctx)
	for _, r := range realms {
		if cur == r {
			return nil
		}
	}
	return kerrors.NewBizStatusError(CodeForbidden, fmt.Sprintf("此操作不对 %s 开放", cur))
}

// TokenKey carries the user's access token beside the identity, so that a
// service down the chain can check the identity instead of believing it.
const TokenKey = "UTOKEN"

// WithToken puts the user's access token into ctx for every RPC made with it.
// The gateway's RequireLogin calls it after WithIdentity.
func WithToken(ctx context.Context, token string) context.Context {
	return metainfo.WithPersistentValue(ctx, TokenKey, token)
}

// Token is the user's access token the call carries; "" without one.
func Token(ctx context.Context) string {
	v, _ := metainfo.GetPersistentValue(ctx, TokenKey)
	return v
}

// IdentityConfig is the `identity:` section of an RPC service. The login a
// call claims (UID / REALM / SID in metainfo) is written by the caller, so
// anything that can reach the service could claim to be anyone; the gateway
// therefore passes the user's access token along (WithToken) and this check
// verifies it with the public keys: the token must be valid (signature,
// issuer, not expired by more than authx.CheckLeeway) and be for the claimed
// uid, realm and session. A call that claims no login (a job, a consumer, a
// public endpoint) is not checked by Mode.
//
// RequireLogin closes the other door: a gateway that was broken into could
// simply claim no login and name any uid in the request. With it on, a call
// that comes from a gateway (Gateways, matched against the caller
// service_identity proved) must claim a login unless the method is one the
// gateway legitimately calls without a user (Anonymous: registration, the
// steps of a login, password recovery, public lookups), and the login it
// claims must be of the realm that gateway logs in. Anything else is 1004.
// Handlers then take the user from LoginUID(ctx), or guard a uid in the
// request with CheckMemberUID.
//
// IdentityConfig 是 RPC 服务的 `identity:` 段。调用声称的登录者（metainfo 里的 UID / REALM / SID）是调用方自己写的，
// 能连上服务的程序都能冒充任何人；所以网关把用户的访问令牌一起传下来（WithToken），这里用公钥核对：令牌有效
// （签名、签发方、过期不超过 authx.CheckLeeway），并且就是声称的 uid、realm、会话。没声称登录者的调用（任务、消费者、
// 公开接口）mode 不查。
//
// require_login 关上另一扇门：被攻破的网关可以干脆不声称登录者、在请求体里写任意 uid。打开后，来自网关
// （gateways，按服务身份核对出的调用方名字匹配）的调用必须声称登录者——除非方法在 anonymous 名单里（注册、登录各步、
// 找回密码、公开查询，网关本来就没有用户时调的）——而且声称的 realm 必须是该网关登录的 realm；否则 1004。
// handler 再用 LoginUID(ctx) 取登录者，或用 CheckMemberUID 守住请求体里的目标 uid。
type IdentityConfig struct {
	// Mode: off (default), log (check, count and log a failure, let the
	// call through) or enforce (refuse a failure with 1004). Empty takes the
	// environment variable IDENTITY_CHECK, so a deployment can switch every
	// service without editing each conf.
	Mode string `yaml:"mode"`
	// PublicKeys verify the tokens (base64 Ed25519, as auth.public_keys).
	// Empty takes the environment variable AUTH_JWT_PUBLIC_KEY.
	PublicKeys []string `yaml:"public_keys"`
	// Issuer is the tokens' `iss`, default "sportsbook".
	Issuer string `yaml:"issuer"`
	// RequireLogin: off (default), log or enforce, for calls from a gateway
	// without a login or with a login of another realm. Needs Mode on: log
	// with log or enforce, enforce with enforce (a claim that is not
	// verified proves nothing). Empty takes IDENTITY_REQUIRE_LOGIN.
	RequireLogin string `yaml:"require_login"`
	// Gateways: the API services and the realm each one logs in, e.g.
	// {ser-api: member, ser-admin-api: admin}. Empty takes IDENTITY_GATEWAYS
	// ("ser-api=member,ser-admin-api=admin"). Required when RequireLogin is on.
	Gateways map[string]string `yaml:"gateways"`
	// Anonymous: the methods a gateway calls without a login. "*" means every
	// method (a service with nothing a user calls through a gateway writes
	// its own name in callers instead; this is for an interim).
	Anonymous []string `yaml:"anonymous"`
}

// Identity check modes.
const (
	IdentityOff     = "off"
	IdentityLog     = "log"
	IdentityEnforce = "enforce"
)

// CodeUnauthenticated: the call claims a login its token does not prove, or
// comes from a gateway without one (enforce).
const CodeUnauthenticated int32 = 1004

// Environment variables an empty section takes its values from.
const (
	identityModeEnv         = "IDENTITY_CHECK"
	identityRequireLoginEnv = "IDENTITY_REQUIRE_LOGIN"
	identityGatewaysEnv     = "IDENTITY_GATEWAYS"
)

func modeOf(conf, env string) string {
	m := strings.TrimSpace(conf)
	if m == "" {
		m = strings.TrimSpace(os.Getenv(env))
	}
	if m == "" {
		return IdentityOff
	}
	return m
}

func (c IdentityConfig) mode() string         { return modeOf(c.Mode, identityModeEnv) }
func (c IdentityConfig) requireLogin() string { return modeOf(c.RequireLogin, identityRequireLoginEnv) }

func (c IdentityConfig) publicKeys() []string {
	if len(c.PublicKeys) > 0 {
		return c.PublicKeys
	}
	if v := strings.TrimSpace(os.Getenv("AUTH_JWT_PUBLIC_KEY")); v != "" {
		return []string{v}
	}
	return nil
}

// gateways is Gateways, else IDENTITY_GATEWAYS parsed ("name=realm,...").
func (c IdentityConfig) gateways() (map[string]authx.Realm, error) {
	out := map[string]authx.Realm{}
	add := func(name, realm string) error {
		name, realm = strings.TrimSpace(name), strings.TrimSpace(realm)
		if name == "" || !authx.Realm(realm).Valid() {
			return fmt.Errorf("kitexx: identity.gateways: %q: %q is not a realm name (member, admin, agent, or a-z 0-9 _)", name, realm)
		}
		out[name] = authx.Realm(realm)
		return nil
	}
	if len(c.Gateways) > 0 {
		for name, realm := range c.Gateways {
			if err := add(name, realm); err != nil {
				return nil, err
			}
		}
		return out, nil
	}
	for _, pair := range strings.Split(os.Getenv(identityGatewaysEnv), ",") {
		if strings.TrimSpace(pair) == "" {
			continue
		}
		name, realm, ok := strings.Cut(pair, "=")
		if !ok {
			return nil, fmt.Errorf("kitexx: %s: %q is not name=realm", identityGatewaysEnv, pair)
		}
		if err := add(name, realm); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func validMode(m string) bool { return m == IdentityOff || m == IdentityLog || m == IdentityEnforce }

// IdentityCheck is the middleware of `identity:`; a pass-through when off.
func IdentityCheck(cfg IdentityConfig) (endpoint.Middleware, error) {
	mode, requireLogin := cfg.mode(), cfg.requireLogin()
	if !validMode(mode) {
		return nil, fmt.Errorf("kitexx: identity.mode %q: off, log or enforce", mode)
	}
	if !validMode(requireLogin) {
		return nil, fmt.Errorf("kitexx: identity.require_login %q: off, log or enforce", requireLogin)
	}
	if mode == IdentityOff && requireLogin == IdentityOff {
		return func(next endpoint.Endpoint) endpoint.Endpoint { return next }, nil
	}
	var gateways map[string]authx.Realm
	if requireLogin != IdentityOff {
		if mode == IdentityOff || (requireLogin == IdentityEnforce && mode != IdentityEnforce) {
			return nil, fmt.Errorf("kitexx: identity.require_login %s needs identity.mode %s: a login that is not verified proves nothing", requireLogin, requireLogin)
		}
		var err error
		if gateways, err = cfg.gateways(); err != nil {
			return nil, err
		}
		if len(gateways) == 0 {
			return nil, fmt.Errorf("kitexx: identity.require_login %s needs identity.gateways (or %s): the services whose calls must carry a login", requireLogin, identityGatewaysEnv)
		}
	}
	anonymous := map[string]bool{}
	for _, m := range cfg.Anonymous {
		m = strings.TrimSpace(m)
		if m == "" {
			return nil, fmt.Errorf("kitexx: identity.anonymous: empty method name")
		}
		anonymous[m] = true
	}
	checker, err := authx.NewTokenChecker(cfg.publicKeys(), cfg.Issuer)
	if err != nil {
		return nil, fmt.Errorf("kitexx: identity.mode %s: %w", mode, err)
	}
	zlog.Info("identity check on", zlog.Str("mode", mode), zlog.Str("require_login", requireLogin), zlog.Int("gateways", int64(len(gateways))), zlog.Int("anonymous_methods", int64(len(anonymous))))
	return func(next endpoint.Endpoint) endpoint.Endpoint {
		return func(ctx context.Context, req, resp any) error {
			method, from := "", ""
			if ri := rpcinfo.GetRPCInfo(ctx); ri != nil {
				if ri.Invocation() != nil {
					method = ri.Invocation().MethodName()
				}
				if ri.From() != nil {
					from = ri.From().ServiceName()
				}
			}
			if proven := VerifiedCaller(ctx); proven != "" {
				from = proven // service_identity: proven by the caller's ServiceAccount token
			}
			gatewayRealm, fromGateway := gateways[from]
			claimed, ok := metainfo.GetPersistentValue(ctx, UIDKey)
			if !ok || claimed == "" {
				if !fromGateway || anonymous[method] || anonymous["*"] {
					return next(ctx, req, resp)
				}
				// a gateway calling a method that works on a user, without a user
				metricsx.RPCIdentityChecks.WithLabelValues(method, "no_login", requireLogin).Inc()
				zlog.Ctx(ctx).Warn("call from a gateway claims no login", zlog.Str("result", "no_login"), zlog.Str("method", method), zlog.Str("from", from), zlog.Str("mode", requireLogin))
				if requireLogin == IdentityEnforce {
					return BizError(ctx, kerrors.NewBizStatusError(CodeUnauthenticated, "未登录"))
				}
				return next(ctx, req, resp)
			}
			result, decides := "ok", mode
			if tok := Token(ctx); tok == "" {
				result = "no_token"
			} else if id, err := checker.Check(tok); err != nil {
				result = "bad_token"
			} else if strconv.FormatInt(id.UID, 10) != claimed || string(id.Realm) != string(Realm(ctx)) || id.SID != sessionID(ctx) {
				result = "mismatch"
			} else if fromGateway && Realm(ctx) != gatewayRealm {
				// a valid login, but not one this gateway logs in (a member's token through the back office, or the reverse)
				result, decides = "realm_mismatch", requireLogin
			}
			metricsx.RPCIdentityChecks.WithLabelValues(method, result, decides).Inc()
			if result == "ok" {
				return next(ctx, req, resp)
			}
			zlog.Ctx(ctx).Warn("claimed login not proven", zlog.Str("result", result), zlog.Str("method", method), zlog.Str("from", from),
				zlog.Str("claimed_uid", claimed), zlog.Str("realm", string(Realm(ctx))), zlog.Str("mode", decides))
			if decides == IdentityEnforce {
				return BizError(ctx, kerrors.NewBizStatusError(CodeUnauthenticated, "登录已过期，请重新登录"))
			}
			return next(ctx, req, resp)
		}
	}, nil
}

func sessionID(ctx context.Context) string {
	v, _ := metainfo.GetPersistentValue(ctx, SIDKey)
	return v
}
