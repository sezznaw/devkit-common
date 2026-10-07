package httpx

import (
	"fmt"
	"net"
	"strings"

	"github.com/sezznaw/devkit-common/config"
)

// Callback is the `callback:` entry of a provider: the rules for the
// requests the provider sends us.
type Callback struct {
	// Verify is how a callback proves it comes from the provider. Type
	// "hmac-sha256": Header carries encode(HMAC-SHA256(Secret, Payload)),
	// Payload a template of {method} {path} {query} {timestamp} {body}
	// (default "{body}"); with {timestamp} the value is read from
	// TimestampHeader and rejected when older than MaxSkew (default 5m).
	// Type "basic": the provider sends Authorization: Basic with Username /
	// Password. Type "custom": a Verifier given to webhookx.Handle. Type ""
	// means no verification, allowed only together with AllowCIDRs.
	Verify CallbackVerify `yaml:"verify"`
	// AllowCIDRs: the provider's published source addresses; a callback
	// from elsewhere is 403. Optional, in addition to Verify.
	AllowCIDRs []string `yaml:"allow_cidrs"`
	// EventID says where the provider's unique id of the notification is:
	// a header, or a JSON field of the body ("id", "data.event_id"). It is
	// what makes a redelivered callback a no-op. Without it every callback
	// runs the handler (a warning at start).
	EventID EventIDSource `yaml:"event_id"`
	// DedupeTTL is how long an event id is remembered. Default 7 days.
	DedupeTTL config.Duration `yaml:"dedupe_ttl"`
	// ArchiveTopic is the Kafka topic the raw callback (headers and body)
	// is copied to before the handler runs, so a dispute can be settled
	// from what the provider actually sent. Default "callbacks.<provider>";
	// "-" switches the copy off. Needs kafka.enabled.
	ArchiveTopic string `yaml:"archive_topic"`
	// MaxBody is the largest body accepted, bytes. Default 1 MiB.
	MaxBody int64 `yaml:"max_body"`
}

type CallbackVerify struct {
	Type            string          `yaml:"type"`
	Secret          string          `yaml:"secret"`
	Header          string          `yaml:"header"`
	Payload         string          `yaml:"payload"`
	Encoding        string          `yaml:"encoding"`
	TimestampHeader string          `yaml:"timestamp_header"`
	MaxSkew         config.Duration `yaml:"max_skew"`
	Username        string          `yaml:"username"`
	Password        string          `yaml:"password"`
}

type EventIDSource struct {
	Header string `yaml:"header"`
	JSON   string `yaml:"json"`
}

// Configured says whether the provider has a callback entry at all.
func (c Callback) Configured() bool {
	return c.Verify.Type != "" || len(c.AllowCIDRs) > 0 || c.EventID.Header != "" || c.EventID.JSON != ""
}

// Validate checks the callback entry.
func (c Callback) Validate(name string) error {
	if !c.Configured() {
		return nil
	}
	v := c.Verify
	switch v.Type {
	case "":
		if len(c.AllowCIDRs) == 0 {
			return fmt.Errorf("httpx: providers.%s.callback: verify.type is required (hmac-sha256, basic, custom), or at least allow_cidrs", name)
		}
	case "hmac-sha256":
		if strings.TrimSpace(v.Secret) == "" {
			return fmt.Errorf("httpx: providers.%s.callback.verify.secret is required (is its environment variable set?)", name)
		}
		if v.Encoding != "" && v.Encoding != "hex" && v.Encoding != "base64" {
			return fmt.Errorf("httpx: providers.%s.callback.verify.encoding %q: hex or base64", name, v.Encoding)
		}
	case "basic":
		if v.Username == "" || v.Password == "" {
			return fmt.Errorf("httpx: providers.%s.callback.verify: username and password are required for basic", name)
		}
	case "custom":
	default:
		return fmt.Errorf("httpx: providers.%s.callback.verify.type %q: hmac-sha256, basic or custom", name, v.Type)
	}
	for _, c := range c.AllowCIDRs {
		if _, _, err := net.ParseCIDR(c); err != nil {
			return fmt.Errorf("httpx: providers.%s.callback.allow_cidrs %q: not a CIDR (use 203.0.113.7/32 for one address)", name, c)
		}
	}
	if c.EventID.Header != "" && c.EventID.JSON != "" {
		return fmt.Errorf("httpx: providers.%s.callback.event_id: header or json, not both", name)
	}
	return nil
}
