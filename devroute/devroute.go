// Package devroute lets a developer's machine take over one service of the
// development environment for the requests that carry that developer's tag,
// and nothing else: a request with X-Dev-Tag: alice reaches alice's local
// instance of the service she took over, every other request goes where it
// always went. [takeover]
//
// The pieces: the tag travels as a persistent metainfo value (DEV_TAG), so
// it crosses every hop exactly like the trace id; takeover instances are
// registered by `devkit takeover` in the Nacos group DEV_TAKEOVER with a
// dev-tag metadata entry; the balancer sends tagged requests to the instance
// with the same tag and lets Kitex's default balancer pick among baseline
// instances for everything else; a tagged request gets the debug timeout
// instead of the usual one, so a breakpoint does not fail the chain.
//
// Enabled is decided from APP_ENV: dev yes, uat and prod no, local only
// while the service runs in takeover mode (a fresh .devkit/takeover.json
// written by the tool). A conf section `devroute: {enabled, debug_timeout}`
// may override. Off, nothing here is on the request path: no second Nacos
// client, no custom balancer, the header is ignored, the timeout untouched.
package devroute

import (
	"context"
	"regexp"
	"strings"
	"time"

	"github.com/bytedance/gopkg/cloud/metainfo"

	"github.com/sezznaw/devkit-common/config"
)

const (
	// HeaderKey is the HTTP header a client sets.
	HeaderKey = "X-Dev-Tag"
	// MetaKey is the persistent metainfo key the tags travel under.
	MetaKey = "DEV_TAG"
	// TakeoverGroup is the Nacos group takeover instances live in.
	TakeoverGroup = "DEV_TAKEOVER"
	// TagMetadata is the instance metadata key holding one tag.
	TagMetadata = "dev-tag"
	// DefaultDebugTimeout is how long a tagged request may take.
	DefaultDebugTimeout = 30 * time.Minute
	// DevEnv is the APP_ENV in which routing is on by default.
	DevEnv = "dev"
	// MaxTags is how many tags one request may carry (a chain of takeovers).
	MaxTags = 3
	// TakeoverPortMin / Max is the port range devkit takeover binds on the
	// node for the reverse tunnel; a takeover instance outside it is refused.
	TakeoverPortMin, TakeoverPortMax = 20000, 32000
)

// Config is the optional `devroute:` section of a service configuration.
type Config struct {
	// On overrides the APP_ENV rule (`enabled:` in YAML). Leave it out.
	On *bool `yaml:"enabled"`
	// DebugTimeout replaces the RPC and gateway timeouts of a tagged
	// request. Default 30m.
	DebugTimeout config.Duration `yaml:"debug_timeout"`
	// BaselineCIDRs are the networks a baseline (non-takeover) instance may
	// have; an instance elsewhere is ignored. Default: this process's own
	// /16, which is the Pod network in a cluster.
	BaselineCIDRs []string `yaml:"baseline_cidrs"`
}

// Enabled says whether tagged routing is on for this process.
func (c Config) Enabled() bool {
	if c.On != nil {
		return *c.On
	}
	return config.Env() == DevEnv || config.Takeover() != nil
}

// Timeout is the debug timeout.
func (c Config) Timeout() time.Duration { return c.DebugTimeout.Or(DefaultDebugTimeout) }

var tagRe = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)

// Parse validates a header value: one or more tags, comma separated. It
// returns the tags in order and ok=false when any of them is malformed (a
// malformed header counts as no header at all: nothing is routed on it).
func Parse(header string) (tags []string, ok bool) {
	header = strings.TrimSpace(header)
	if header == "" {
		return nil, false
	}
	parts := strings.Split(header, ",")
	if len(parts) > MaxTags {
		return nil, false
	}
	for _, t := range parts {
		t = strings.TrimSpace(t)
		if !tagRe.MatchString(t) {
			return nil, false
		}
		tags = append(tags, t)
	}
	return tags, true
}

// ValidTag says whether one tag is well formed.
func ValidTag(tag string) bool { return tagRe.MatchString(tag) }

// WithTags puts the tags into ctx as the persistent metainfo value every
// RPC made with ctx carries to the service it calls.
func WithTags(ctx context.Context, tags []string) context.Context {
	if len(tags) == 0 {
		return ctx
	}
	return metainfo.WithPersistentValue(ctx, MetaKey, strings.Join(tags, ","))
}

// Tags returns the tags of ctx, in order; nil when there are none.
func Tags(ctx context.Context) []string {
	v, ok := metainfo.GetPersistentValue(ctx, MetaKey)
	if !ok || v == "" {
		return nil
	}
	tags, ok := Parse(v)
	if !ok {
		return nil
	}
	return tags
}

// TagString is Tags joined, for log fields; "" when there are none.
func TagString(ctx context.Context) string {
	v, _ := metainfo.GetPersistentValue(ctx, MetaKey)
	return v
}
