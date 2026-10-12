package config

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Takeover mode: `devkit takeover <service>` runs on a developer's machine
// and, while it runs, keeps <service>/.devkit/takeover.json fresh. A service
// started on that machine (APP_ENV unset, i.e. local) that finds a fresh
// marker for itself runs against the development environment the tool has
// tunnelled: it loads conf/<marker.env>.yaml instead of local.yaml, gets the
// environment variables the deployment would have, and kitexx rewrites every
// cluster address to its tunnel. Without the marker nothing changes. [takeover]

// TakeoverFile is the marker, relative to the service directory (the parent
// of the conf directory).
const TakeoverFile = ".devkit/takeover.json"

// TakeoverFresh is how recent the marker's heartbeat must be. The tool
// refreshes it every 5 seconds; a marker left behind by a crashed tool is
// ignored after this.
const TakeoverFresh = 15 * time.Second

// TakeoverMarker is what the tool writes.
type TakeoverMarker struct {
	// Service is the service the tool took over; the marker is only for it.
	Service string `json:"service"`
	// Tag is the developer's tag; requests carrying it reach this machine.
	Tag string `json:"tag"`
	// Env is the configuration profile to load (conf/<env>.yaml), "dev".
	Env string `json:"env"`
	// PID of the tool, Heartbeat its last refresh (RFC3339).
	PID       int       `json:"pid"`
	Heartbeat time.Time `json:"heartbeat"`
	// EnvVars are the deployment's environment variables, addresses already
	// pointing at the tunnels; set into this process when not already set.
	EnvVars map[string]string `json:"env_vars"`
	// Rewrite maps cluster addresses ("host:port", host as the cluster
	// names or IPs them) to the local tunnel ("127.0.0.1:port").
	Rewrite map[string]string `json:"rewrite"`
	// Services maps a service name to where its instances are reached from
	// this machine ("127.0.0.1:port"): the tunnel to its ClusterIP, or the
	// local instance when this machine took that service over too.
	Services map[string]string `json:"services"`
	// Path the marker was read from.
	Path string `json:"-"`
}

var (
	takeoverOnce sync.Once
	takeover     *TakeoverMarker
	takeoverOff  atomic.Bool
	takeoverNote string // why a marker that exists was ignored, for the start-up log
)

// TakeoverIgnored says why a marker found next to this service was not used
// (APP_ENV set to an environment, another service's marker, a stale
// heartbeat), or "" when there was no marker or it was used. The service
// logs it at start so that "the tool says not registered" has a reason.
func TakeoverIgnored() string {
	Takeover()
	return takeoverNote
}

// DisableTakeover makes Takeover report nothing, whatever is on disk and
// whether or not it was read before. Tests (testx) call it: a test must
// never pick up a developer's live takeover.
func DisableTakeover() { takeoverOff.Store(true) }

// TakeoverAckFile is written next to the marker when the service has entered
// takeover mode: the tool registers the local instance only once it is there,
// so a service that is up but running against its local middleware (the
// marker ignored or unreadable) never receives the environment's traffic.
// The name extends the marker's so the same .gitignore line covers it.
const TakeoverAckFile = TakeoverFile + ".ack"

// TakeoverAck is the content of TakeoverAckFile.
type TakeoverAck struct {
	Service string    `json:"service"`
	Tag     string    `json:"tag"`
	PID     int       `json:"pid"`
	Since   time.Time `json:"since"`
}

// Takeover returns the fresh marker of this service, or nil. It is read once;
// the service decides at start whether it is in takeover mode and stays
// there. Only a process on a developer's machine looks: APP_ENV unset or
// "local" (make run sets local; an IDE run configuration sets nothing). The
// marker must name the service whose directory it is in (<service>/.devkit),
// so a marker left for another service is not one.
func Takeover() *TakeoverMarker {
	if takeoverOff.Load() {
		return nil
	}
	takeoverOnce.Do(func() {
		dir, err := Dir()
		if err != nil {
			return
		}
		if abs, err := filepath.Abs(dir); err == nil {
			dir = abs // make run and go run find "conf" relative to the working directory
		}
		serviceDir := filepath.Dir(dir)
		path := filepath.Join(serviceDir, filepath.FromSlash(TakeoverFile))
		b, err := os.ReadFile(path)
		if err != nil {
			return // no marker: the usual case, nothing to say
		}
		if env := os.Getenv(EnvVar); env != "" && env != LocalEnv {
			takeoverNote = fmt.Sprintf("%s=%s (takeover mode needs it unset or local)", EnvVar, env)
			return
		}
		var m TakeoverMarker
		if err := json.Unmarshal(b, &m); err != nil || m.Service == "" || m.Env == "" {
			takeoverNote = "marker unreadable or incomplete: " + path
			return
		}
		if m.Service != filepath.Base(serviceDir) {
			takeoverNote = fmt.Sprintf("marker names service %s, this directory is %s", m.Service, filepath.Base(serviceDir))
			return
		}
		if age := time.Since(m.Heartbeat); age > TakeoverFresh {
			takeoverNote = fmt.Sprintf("marker heartbeat is %s old (is devkit takeover still running?)", age.Truncate(time.Second))
			return
		}
		m.Path = path
		for k, v := range m.EnvVars {
			if os.Getenv(k) == "" {
				os.Setenv(k, v)
			}
		}
		takeover = &m
		if b, err := json.Marshal(TakeoverAck{Service: m.Service, Tag: m.Tag, PID: os.Getpid(), Since: time.Now().UTC()}); err == nil {
			_ = os.WriteFile(path+".ack", b, 0o600) // best effort; the tool waits for it
		}
	})
	return takeover
}

// RewriteAddr maps a cluster "host:port" to its tunnel while in takeover
// mode; otherwise, and for an address the marker does not list, it returns
// addr unchanged. Hosts compare case-insensitively and without a trailing
// dot (Redpanda advertises "redpanda-0.redpanda.ns.svc.cluster.local.").
func RewriteAddr(addr string) string {
	m := Takeover()
	if m == nil || len(m.Rewrite) == 0 {
		return addr
	}
	if to, ok := m.Rewrite[normalizeAddr(addr)]; ok {
		return to
	}
	return addr
}

// RewriteURL is RewriteAddr for a URL ("scheme://host:port/path?query").
func RewriteURL(u string) string {
	m := Takeover()
	if m == nil || len(m.Rewrite) == 0 {
		return u
	}
	parsed, err := url.Parse(u)
	if err != nil || parsed.Host == "" {
		return RewriteAddr(u)
	}
	to := RewriteAddr(parsed.Host)
	if to == parsed.Host {
		return u
	}
	parsed.Host = to
	return parsed.String()
}

func normalizeAddr(addr string) string {
	addr = strings.TrimSpace(addr)
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return strings.ToLower(strings.TrimSuffix(addr, "."))
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	return net.JoinHostPort(host, port)
}

// takeoverProfile is the file Load reads in takeover mode, else "".
func takeoverProfile() string {
	if m := Takeover(); m != nil {
		return m.Env
	}
	return ""
}
