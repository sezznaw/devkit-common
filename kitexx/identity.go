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
// with ctx. The gateway's RequireLogin calls it.
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
// public endpoint) is not checked.
//
// IdentityConfig 是 RPC 服务的 `identity:` 段。调用声称的登录者（metainfo 里的 UID / REALM / SID）是调用方自己写的，
// 能连上服务的程序都能冒充任何人；所以网关把用户的访问令牌一起传下来（WithToken），这里用公钥核对：令牌有效
// （签名、签发方、过期不超过 authx.CheckLeeway），并且就是声称的 uid、realm、会话。没声称登录者的调用（任务、消费者、
// 公开接口）不查。
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
}

// Identity check modes.
const (
	IdentityOff     = "off"
	IdentityLog     = "log"
	IdentityEnforce = "enforce"
)

// CodeUnauthenticated: the call claims a login its token does not prove (enforce).
const CodeUnauthenticated int32 = 1004

func (c IdentityConfig) mode() string {
	m := strings.TrimSpace(c.Mode)
	if m == "" {
		m = strings.TrimSpace(os.Getenv("IDENTITY_CHECK"))
	}
	if m == "" {
		return IdentityOff
	}
	return m
}

func (c IdentityConfig) publicKeys() []string {
	if len(c.PublicKeys) > 0 {
		return c.PublicKeys
	}
	if v := strings.TrimSpace(os.Getenv("AUTH_JWT_PUBLIC_KEY")); v != "" {
		return []string{v}
	}
	return nil
}

// IdentityCheck is the middleware of `identity:`; a pass-through when off.
func IdentityCheck(cfg IdentityConfig) (endpoint.Middleware, error) {
	mode := cfg.mode()
	switch mode {
	case IdentityOff:
		return func(next endpoint.Endpoint) endpoint.Endpoint { return next }, nil
	case IdentityLog, IdentityEnforce:
	default:
		return nil, fmt.Errorf("kitexx: identity.mode %q: off, log or enforce", mode)
	}
	checker, err := authx.NewTokenChecker(cfg.publicKeys(), cfg.Issuer)
	if err != nil {
		return nil, fmt.Errorf("kitexx: identity.mode %s: %w", mode, err)
	}
	zlog.Info("identity check on", zlog.Str("mode", mode))
	return func(next endpoint.Endpoint) endpoint.Endpoint {
		return func(ctx context.Context, req, resp any) error {
			claimed, ok := metainfo.GetPersistentValue(ctx, UIDKey)
			if !ok || claimed == "" {
				return next(ctx, req, resp)
			}
			result := "ok"
			if tok := Token(ctx); tok == "" {
				result = "no_token"
			} else if id, err := checker.Check(tok); err != nil {
				result = "bad_token"
			} else if strconv.FormatInt(id.UID, 10) != claimed || string(id.Realm) != string(Realm(ctx)) || id.SID != sessionID(ctx) {
				result = "mismatch"
			}
			method, from := "", ""
			if ri := rpcinfo.GetRPCInfo(ctx); ri != nil {
				if ri.Invocation() != nil {
					method = ri.Invocation().MethodName()
				}
				if ri.From() != nil {
					from = ri.From().ServiceName()
				}
			}
			metricsx.RPCIdentityChecks.WithLabelValues(method, result, mode).Inc()
			if result == "ok" {
				return next(ctx, req, resp)
			}
			zlog.Ctx(ctx).Warn("claimed login not proven", zlog.Str("result", result), zlog.Str("method", method), zlog.Str("from", from),
				zlog.Str("claimed_uid", claimed), zlog.Str("realm", string(Realm(ctx))), zlog.Str("mode", mode))
			if mode == IdentityEnforce {
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
