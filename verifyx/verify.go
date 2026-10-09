// Package verifyx is the post-deploy verification of a service: a list of
// checks run against the deployment that just went live (`<binary>
// --verify`, which the deployment runs as an ArgoCD PostSync Job), so a
// release that starts but does not work is caught in minutes and raises
// the failed-Job alert instead of waiting for a user. The framework
// supplies the checks every service needs (its connections, Nacos
// registration, migrations, the outbox backlog, the gateway's /ping); a
// service adds its own few in app/verify.go: log in, read something,
// call one RPC.
//
// A check is a function that returns an error; the runner gives each a
// timeout, logs the result, runs them all and fails if any failed.
// Checks must be safe to run against a live system at any time: read
// only, or a write that is harmless and idempotent (a test account's
// own data).
//
// verifyx 是服务的部署后验证：一组检查，对刚上线的部署跑一遍（`<二进制> --verify`，部署里是
// ArgoCD 的 PostSync Job），起得来但不能用的版本几分钟内就被发现并触发 Job 失败告警。
// 框架提供每个服务都需要的检查（连接、Nacos 注册、迁移、outbox 积压、网关 /ping），服务在
// app/verify.go 里加自己的几条：登录、读一条、调一个 RPC。检查必须对线上无害：只读，或者
// 幂等且无害的写（测试账号自己的数据）。
package verifyx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/sezznaw/devkit-common/zlog"
)

// Check is one verification: a short name (lowercase, dashes) and what to
// run. Run gets a context with the check's timeout.
type Check struct {
	Name        string
	Description string
	Run         func(ctx context.Context) error
}

// Options of the runner.
type Options struct {
	// Timeout per check. Default 30s.
	Timeout time.Duration
}

// Run runs every check in order and returns an error naming the failed
// ones (nil when all passed). Each result is one log record with the
// check's name, outcome and duration, so the Job's log reads as a report.
func Run(ctx context.Context, checks []Check, opts Options) error {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	var failed []string
	for _, c := range checks {
		if c.Name == "" || c.Run == nil {
			return errors.New("verifyx: a check needs a name and a function")
		}
		start := time.Now()
		cctx, cancel := context.WithTimeout(ctx, timeout)
		err := safely(cctx, c.Run)
		cancel()
		if err != nil {
			failed = append(failed, c.Name)
			zlog.Error("verify failed", zlog.Str("check", c.Name), zlog.Str("description", c.Description), zlog.Dur("took", time.Since(start)), zlog.Err(err))
			continue
		}
		zlog.Info("verify ok", zlog.Str("check", c.Name), zlog.Dur("took", time.Since(start)))
	}
	if len(failed) > 0 {
		return fmt.Errorf("verifyx: %d of %d checks failed: %s", len(failed), len(checks), strings.Join(failed, ", "))
	}
	zlog.Info("verify passed", zlog.Int("checks", int64(len(checks))))
	return nil
}

func safely(ctx context.Context, fn func(context.Context) error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return fn(ctx)
}

// BaseURLEnv names the environment variable with the gateway's own URL
// (the deployment sets it to the Service: http://<name>:<port>); on a
// developer's machine it defaults to the configured listen port.
const BaseURLEnv = "VERIFY_BASE_URL"

// Gateway is an HTTP client for a gateway's own endpoints: the base URL
// from VERIFY_BASE_URL or the fallback, JSON in and the {code, msg, data}
// envelope out.
type Gateway struct {
	BaseURL string
	Token   string // Authorization: Bearer, after a login check sets it
	client  *http.Client
}

// NewGateway reads VERIFY_BASE_URL, else uses fallback (e.g.
// "http://127.0.0.1:8080").
func NewGateway(fallback string) *Gateway {
	base := strings.TrimRight(os.Getenv(BaseURLEnv), "/")
	if base == "" {
		base = strings.TrimRight(fallback, "/")
	}
	return &Gateway{BaseURL: base, client: &http.Client{Timeout: 15 * time.Second}}
}

// Envelope is a gateway answer.
type Envelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// Get does a GET and wants 200.
func (g *Gateway) Get(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.BaseURL+path, nil)
	if err != nil {
		return nil, err
	}
	return g.do(req)
}

// Post sends JSON (with the bearer token when set) and returns the
// envelope; a non-200 status or a non-zero code is an error that says so.
func (g *Gateway) Post(ctx context.Context, path string, body any) (*Envelope, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.BaseURL+path, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if g.Token != "" {
		req.Header.Set("Authorization", "Bearer "+g.Token)
	}
	raw, err := g.do(req)
	if err != nil {
		return nil, err
	}
	var env Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("%s: not an envelope: %s", path, truncate(raw))
	}
	if env.Code != 0 {
		return &env, fmt.Errorf("%s: code %d: %s", path, env.Code, env.Msg)
	}
	return &env, nil
}

func (g *Gateway) do(req *http.Request) ([]byte, error) {
	res, err := g.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode != http.StatusOK {
		return raw, fmt.Errorf("%s %s: HTTP %d: %s", req.Method, req.URL.Path, res.StatusCode, truncate(raw))
	}
	return raw, nil
}

func truncate(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}
