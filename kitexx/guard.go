package kitexx

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/sezznaw/devkit-common/config"
)

// GuardConfig is the `guard:` section of an API service: what the gateway
// refuses before any handler runs. Every value has a default, so a service
// without the section is still protected; the section tunes it.
//
// GuardConfig 是 API 服务的 `guard:` 段：网关在 handler 之前就拒绝的东西。每项都有默认值，
// 没写这一段也受保护；写了是调参。
type GuardConfig struct {
	// MaxBodyBytes is the largest request body accepted; bigger is 413.
	// Default 1 MiB (1048576). An upload endpoint uses presigned URLs, not
	// the gateway.
	MaxBodyBytes int `yaml:"max_body_bytes"`
	// Timeout is the deadline of one request: the handler's ctx ends then,
	// so the RPC calls it makes give up, and a request that produced nothing
	// by then is answered 504 with code 1010. Default 10s.
	Timeout config.Duration `yaml:"timeout"`
	// Rate is the request-rate limit.
	Rate struct {
		// PerIP caps the requests one client IP may make to this service in
		// a window, over every route: "600/m" (the default), "20/s", "5000/h",
		// or "off". A route with its own `// @limit` in the IDL has that
		// limit in addition (per IP too). Over the limit is 429, code 1008,
		// with a Retry-After header.
		PerIP Rate `yaml:"per_ip"`
	} `yaml:"rate"`
}

// MaxBody is max_body_bytes or its default.
func (g GuardConfig) MaxBody() int {
	if g.MaxBodyBytes > 0 {
		return g.MaxBodyBytes
	}
	return 1 << 20
}

// RequestTimeout is timeout or its default.
func (g GuardConfig) RequestTimeout() time.Duration { return g.Timeout.Or(10 * time.Second) }

// PerIP is rate.per_ip or its default (600 a minute); Off when "off".
func (g GuardConfig) PerIP() Rate {
	if g.Rate.PerIP.set {
		return g.Rate.PerIP
	}
	return Rate{N: 600, Window: time.Minute, set: true}
}

// Rate is "N/s", "N/m" or "N/h": N requests per second, minute or hour;
// "off" is no limit.
type Rate struct {
	N      int
	Window time.Duration
	set    bool
}

// Off reports a rate that limits nothing.
func (r Rate) Off() bool { return r.N <= 0 || r.Window <= 0 }

func (r Rate) String() string {
	if r.Off() {
		return "off"
	}
	u := map[time.Duration]string{time.Second: "s", time.Minute: "m", time.Hour: "h"}[r.Window]
	if u == "" {
		return fmt.Sprintf("%d/%s", r.N, r.Window)
	}
	return fmt.Sprintf("%d/%s", r.N, u)
}

// ParseRate reads "20/s", "600/m", "5000/h" or "off".
func ParseRate(s string) (Rate, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" || s == "off" || s == "0" {
		return Rate{set: true}, nil
	}
	n, unit, ok := strings.Cut(s, "/")
	if !ok {
		return Rate{}, fmt.Errorf("rate %q: want \"<count>/s\", \"<count>/m\", \"<count>/h\" or \"off\"", s)
	}
	count, err := strconv.Atoi(strings.TrimSpace(n))
	if err != nil || count <= 0 {
		return Rate{}, fmt.Errorf("rate %q: the count must be a positive integer", s)
	}
	var w time.Duration
	switch strings.TrimSpace(unit) {
	case "s", "sec", "second":
		w = time.Second
	case "m", "min", "minute":
		w = time.Minute
	case "h", "hour":
		w = time.Hour
	default:
		return Rate{}, fmt.Errorf("rate %q: the unit is s, m or h", s)
	}
	return Rate{N: count, Window: w, set: true}, nil
}

// UnmarshalYAML accepts the string form.
func (r *Rate) UnmarshalYAML(n *yaml.Node) error {
	parsed, err := ParseRate(n.Value)
	if err != nil {
		return err
	}
	*r = parsed
	return nil
}

// Codes the guard answers with; rows of the idl repository's errors.md.
const (
	CodeTooManyRequests = 1008 // HTTP 429
	CodeRequestTimeout  = 1010 // HTTP 504
)
