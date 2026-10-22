package kitexx

import (
	"context"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/bytedance/gopkg/cloud/metainfo"
	"github.com/cloudwego/kitex/pkg/endpoint"
	"github.com/cloudwego/kitex/pkg/kerrors"
	"github.com/cloudwego/kitex/pkg/rpcinfo"
	"github.com/golang-jwt/jwt/v5"

	"github.com/sezznaw/devkit-common/metricsx"
	"github.com/sezznaw/devkit-common/zlog"
)

// Service identity: who is calling, proven. The caller's service name in the
// TTHeader (what callers: matches) is whatever the caller writes, so anything
// that can reach a service could call it as any other. Every pod therefore
// carries a token the cluster signs for its own ServiceAccount (a projected
// token with the audience sportsbook-rpc, an hour long, rotated by the
// kubelet); the client attaches it to every call (one hop, not passed on) and
// the server checks it locally against the cluster's public keys (OIDC JWKS):
// signature, issuer, audience, expiry, and a ServiceAccount in an allowed
// namespace whose name is the caller's service name. Once checked, callers:
// matches the proven name.
//
// 服务身份：证明是谁在调。TTHeader 里的调用方服务名（callers: 按它放行）是调用方自己写的，能连上服务的程序就能冒充别的服务。
// 所以每个 Pod 带一个集群为它自己的 ServiceAccount 签发的令牌（投射令牌，受众 sportsbook-rpc，一小时，kubelet 轮换）；
// 客户端每次调用附上它（只到下一跳，不往后传），服务端用集群公钥（OIDC JWKS）在本地核对：签名、签发方、受众、过期、
// 允许的 namespace 里名字等于调用方服务名的 ServiceAccount。核对通过后 callers: 按核对出的名字判断。

// ServiceIdentityConfig is the `service_identity:` section.
type ServiceIdentityConfig struct {
	// Mode: off (default), log (check, count and warn, let the call through)
	// or enforce (refuse an unproven caller with 1007). Empty takes the
	// environment variable SERVICE_IDENTITY.
	Mode string `yaml:"mode"`
	// Dir holds the projected files: rpc-token (the token attached to
	// calls), api-token and ca.crt (to read the cluster's public keys).
	// Default /var/run/secrets/sportsbook.
	Dir string `yaml:"dir"`
	// Audience of the rpc-token, default "sportsbook-rpc".
	Audience string `yaml:"audience"`
	// Issuer of the cluster's ServiceAccount tokens, default
	// "https://kubernetes.default.svc.cluster.local".
	Issuer string `yaml:"issuer"`
	// JWKSURL serves the cluster's public keys, default
	// "https://kubernetes.default.svc/openid/v1/jwks".
	JWKSURL string `yaml:"jwks_url"`
	// Namespaces a caller's ServiceAccount may live in; empty takes the
	// environment variable SERVICE_IDENTITY_NAMESPACES (comma separated);
	// still empty, any.
	Namespaces []string `yaml:"namespaces"`
}

// SvcTokenKey carries the caller's ServiceAccount token, one hop.
const SvcTokenKey = "SVCTOKEN"

const (
	defaultIdentityDir      = "/var/run/secrets/sportsbook"
	defaultIdentityAudience = "sportsbook-rpc"
	defaultIdentityIssuer   = "https://kubernetes.default.svc.cluster.local"
	defaultJWKSURL          = "https://kubernetes.default.svc/openid/v1/jwks"
	saSubjectPrefix         = "system:serviceaccount:"
)

func (c ServiceIdentityConfig) mode() string {
	m := strings.TrimSpace(c.Mode)
	if m == "" {
		m = strings.TrimSpace(os.Getenv("SERVICE_IDENTITY"))
	}
	if m == "" {
		return IdentityOff
	}
	return m
}

func (c ServiceIdentityConfig) dir() string {
	if c.Dir != "" {
		return c.Dir
	}
	return defaultIdentityDir
}

func (c ServiceIdentityConfig) audience() string {
	if c.Audience != "" {
		return c.Audience
	}
	return defaultIdentityAudience
}

func (c ServiceIdentityConfig) issuer() string {
	if c.Issuer != "" {
		return c.Issuer
	}
	return defaultIdentityIssuer
}

func (c ServiceIdentityConfig) jwksURL() string {
	if c.JWKSURL != "" {
		return c.JWKSURL
	}
	return defaultJWKSURL
}

func (c ServiceIdentityConfig) namespaces() map[string]bool {
	list := c.Namespaces
	if len(list) == 0 {
		for _, s := range strings.Split(os.Getenv("SERVICE_IDENTITY_NAMESPACES"), ",") {
			if s = strings.TrimSpace(s); s != "" {
				list = append(list, s)
			}
		}
	}
	if len(list) == 0 {
		return nil
	}
	out := map[string]bool{}
	for _, s := range list {
		out[s] = true
	}
	return out
}

// ---- client: attach this pod's token ----

// svcTokenFile reads the projected token, again once a minute (the kubelet
// rotates it well before it expires).
type svcTokenFile struct {
	path string
	mu   sync.Mutex
	tok  string
	read time.Time
}

func (f *svcTokenFile) get() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if time.Since(f.read) < time.Minute && f.tok != "" {
		return f.tok
	}
	if b, err := os.ReadFile(f.path); err == nil {
		f.tok, f.read = strings.TrimSpace(string(b)), time.Now()
	}
	return f.tok
}

// ClientServiceToken attaches this pod's ServiceAccount token to every call
// (one hop). Without the file (a laptop, a pod without the projected
// volume) it attaches nothing.
func ClientServiceToken(cfg ServiceIdentityConfig) endpoint.Middleware {
	f := &svcTokenFile{path: filepath.Join(cfg.dir(), "rpc-token")}
	return func(next endpoint.Endpoint) endpoint.Endpoint {
		return func(ctx context.Context, req, resp any) error {
			if tok := f.get(); tok != "" {
				ctx = metainfo.WithValue(ctx, SvcTokenKey, tok)
			}
			return next(ctx, req, resp)
		}
	}
}

// ---- server: check the caller's token ----

type verifiedCallerKey struct{}

// VerifiedCaller is the caller's service name as its ServiceAccount token
// proved it; "" when unproven or not checked.
func VerifiedCaller(ctx context.Context) string {
	v, _ := ctx.Value(verifiedCallerKey{}).(string)
	return v
}

// jwks holds the cluster's public keys, refreshed every ten minutes and on
// an unknown kid (at most every 30 seconds).
type jwks struct {
	url     string
	dir     string
	client  *http.Client
	mu      sync.Mutex
	keys    map[string]*rsa.PublicKey
	fetched time.Time
	tried   time.Time
}

func newJWKS(url, dir string) (*jwks, error) {
	pool := x509.NewCertPool()
	if ca, err := os.ReadFile(filepath.Join(dir, "ca.crt")); err == nil {
		pool.AppendCertsFromPEM(ca)
	} else if strings.HasPrefix(url, "https://kubernetes.default") {
		return nil, fmt.Errorf("kitexx: service_identity: %s: %w (is the projected volume mounted?)", filepath.Join(dir, "ca.crt"), err)
	}
	return &jwks{url: url, dir: dir, keys: map[string]*rsa.PublicKey{},
		client: &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}}, nil
}

func (j *jwks) fetch() error {
	req, err := http.NewRequest(http.MethodGet, j.url, nil)
	if err != nil {
		return err
	}
	if tok, err := os.ReadFile(filepath.Join(j.dir, "api-token")); err == nil {
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(tok)))
	}
	resp, err := j.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("jwks: HTTP %d", resp.StatusCode)
	}
	var doc struct {
		Keys []struct {
			Kty, Kid, N, E string
		} `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return err
	}
	keys := map[string]*rsa.PublicKey{}
	for _, k := range doc.Keys {
		if k.Kty != "RSA" {
			continue
		}
		n, err1 := base64.RawURLEncoding.DecodeString(k.N)
		e, err2 := base64.RawURLEncoding.DecodeString(k.E)
		if err1 != nil || err2 != nil {
			continue
		}
		keys[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
	}
	if len(keys) == 0 {
		return errors.New("jwks: no RSA key")
	}
	j.keys, j.fetched = keys, time.Now()
	return nil
}

func (j *jwks) key(kid string) (*rsa.PublicKey, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	k, ok := j.keys[kid]
	stale := time.Since(j.fetched) > 10*time.Minute
	if (!ok || stale) && time.Since(j.tried) > 30*time.Second {
		j.tried = time.Now()
		if err := j.fetch(); err != nil {
			zlog.Warn("service identity: cannot read the cluster's public keys; keeping the last ones", zlog.Err(err))
		}
		k, ok = j.keys[kid]
	}
	if !ok {
		return nil, errors.New("unknown key")
	}
	return k, nil
}

// serviceTokenChecker checks a ServiceAccount token and returns its
// namespace and name.
type serviceTokenChecker struct {
	keys     *jwks
	issuer   string
	audience string
}

func (c *serviceTokenChecker) check(token string) (ns, name string, err error) {
	var cl jwt.RegisteredClaims
	_, err = jwt.ParseWithClaims(token, &cl, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		return c.keys.key(kid)
	}, jwt.WithValidMethods([]string{"RS256"}), jwt.WithIssuer(c.issuer), jwt.WithAudience(c.audience),
		jwt.WithExpirationRequired(), jwt.WithLeeway(30*time.Second))
	if err != nil {
		return "", "", err
	}
	rest, ok := strings.CutPrefix(cl.Subject, saSubjectPrefix)
	ns, name, ok2 := strings.Cut(rest, ":")
	if !ok || !ok2 || ns == "" || name == "" {
		return "", "", fmt.Errorf("subject %q is not a ServiceAccount", cl.Subject)
	}
	return ns, name, nil
}

// ServiceIdentityCheck is the server middleware of `service_identity:`; a
// pass-through when off. self is this service's name: a call from itself
// (its own post-deploy check or job) is checked like any other.
func ServiceIdentityCheck(cfg ServiceIdentityConfig) (endpoint.Middleware, error) {
	mode := cfg.mode()
	switch mode {
	case IdentityOff:
		return func(next endpoint.Endpoint) endpoint.Endpoint { return next }, nil
	case IdentityLog, IdentityEnforce:
	default:
		return nil, fmt.Errorf("kitexx: service_identity.mode %q: off, log or enforce", mode)
	}
	keys, err := newJWKS(cfg.jwksURL(), cfg.dir())
	if err != nil {
		return nil, err
	}
	if err := keys.fetch(); err != nil {
		if mode == IdentityEnforce {
			return nil, fmt.Errorf("kitexx: service_identity enforce: cannot read the cluster's public keys from %s: %w", cfg.jwksURL(), err)
		}
		zlog.Warn("service identity: cannot read the cluster's public keys yet; will retry", zlog.Err(err))
	}
	checker := &serviceTokenChecker{keys: keys, issuer: cfg.issuer(), audience: cfg.audience()}
	allowedNS := cfg.namespaces()
	zlog.Info("service identity check on", zlog.Str("mode", mode))
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
			result, name := "ok", ""
			if tok, _ := metainfo.GetValue(ctx, SvcTokenKey); tok == "" {
				result = "no_token"
			} else if ns, n, err := checker.check(tok); err != nil {
				result = "bad_token"
			} else if allowedNS != nil && !allowedNS[ns] {
				result = "wrong_namespace"
			} else if n != from {
				result, name = "mismatch", n
			} else {
				name = n
			}
			metricsx.RPCServiceIdentity.WithLabelValues(method, result, mode).Inc()
			if result == "ok" {
				return next(context.WithValue(ctx, verifiedCallerKey{}, name), req, resp)
			}
			zlog.Ctx(ctx).Warn("caller not proven", zlog.Str("result", result), zlog.Str("method", method), zlog.Str("from", from),
				zlog.Str("token_name", name), zlog.Str("mode", mode))
			if mode == IdentityEnforce {
				return BizError(ctx, kerrors.NewBizStatusError(CodeForbidden, "调用方身份无法验证"))
			}
			return next(ctx, req, resp)
		}
	}, nil
}
