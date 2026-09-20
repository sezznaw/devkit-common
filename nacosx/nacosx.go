// Package nacosx builds Nacos clients from a small config struct.
package nacosx

import (
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nacos-group/nacos-sdk-go/v2/clients"
	"github.com/nacos-group/nacos-sdk-go/v2/clients/config_client"
	"github.com/nacos-group/nacos-sdk-go/v2/clients/naming_client"
	"github.com/nacos-group/nacos-sdk-go/v2/common/constant"
	"github.com/nacos-group/nacos-sdk-go/v2/vo"
)

// Config is the YAML-loadable Nacos connection config.
type Config struct {
	// Addrs are "host:port" of the Nacos servers.
	Addrs []string `yaml:"addrs"`
	// Namespace is the Nacos namespace id ("" = public).
	Namespace string `yaml:"namespace"`
	// Group defaults to DEFAULT_GROUP.
	Group    string `yaml:"group"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
	// LogDir and CacheDir default to /tmp/nacos/{log,cache}.
	LogDir   string `yaml:"log_dir"`
	CacheDir string `yaml:"cache_dir"`
}

func (c Config) group() string {
	if c.Group == "" {
		return constant.DEFAULT_GROUP
	}
	return c.Group
}

func (c Config) params() (vo.NacosClientParam, error) {
	if len(c.Addrs) == 0 {
		return vo.NacosClientParam{}, fmt.Errorf("nacosx: no addrs configured")
	}
	var servers []constant.ServerConfig
	for _, a := range c.Addrs {
		host, portStr, err := net.SplitHostPort(a)
		if err != nil {
			return vo.NacosClientParam{}, fmt.Errorf("nacosx: bad addr %q: %w", a, err)
		}
		port, err := strconv.ParseUint(portStr, 10, 16)
		if err != nil {
			return vo.NacosClientParam{}, fmt.Errorf("nacosx: bad port in %q: %w", a, err)
		}
		servers = append(servers, *constant.NewServerConfig(host, port))
	}
	cc := constant.NewClientConfig(
		constant.WithNamespaceId(c.Namespace),
		constant.WithUsername(c.Username),
		constant.WithPassword(c.Password),
		constant.WithTimeoutMs(5000),
		constant.WithNotLoadCacheAtStart(true),
		constant.WithLogDir(orDefault(c.LogDir, "/tmp/nacos/log")),
		constant.WithCacheDir(orDefault(c.CacheDir, "/tmp/nacos/cache")),
		constant.WithLogLevel("warn"),
	)
	return vo.NacosClientParam{ClientConfig: cc, ServerConfigs: servers}, nil
}

var (
	sharedMu      sync.Mutex
	sharedNaming  = map[string]naming_client.INamingClient{}
	newNamingFunc = NewNamingClient // replaced in tests
	probeFunc     = Probe           // replaced in tests
)

// SharedNamingClient returns one naming client per distinct Nacos target for
// the whole process. A service registers itself and resolves every downstream
// through Nacos; without sharing, each of those would open its own connection
// and keep its own subscription cache.
func SharedNamingClient(c Config) (naming_client.INamingClient, error) {
	key := fmt.Sprintf("%v|%s|%s", c.Addrs, c.Namespace, c.Username)
	sharedMu.Lock()
	defer sharedMu.Unlock()
	if cli, ok := sharedNaming[key]; ok {
		return cli, nil
	}
	// The SDK itself does not fail here when Nacos is unreachable; it retries in
	// the background and the first real call fails with "client not connected,
	// current status:STARTING", which names neither the address nor the cause.
	if err := probeFunc(c, 2*time.Second); err != nil {
		return nil, err
	}
	cli, err := newNamingFunc(c)
	if err != nil {
		return nil, err
	}
	sharedNaming[key] = cli
	return cli, nil
}

// grpcPortOffset is fixed by Nacos 2.x: clients talk gRPC on main port + 1000
// (8848 -> 9848) in addition to HTTP on the main port.
const grpcPortOffset = 1000

// Probe checks that at least one configured server accepts TCP connections on
// both ports a Nacos 2.x client needs, and explains the failure in terms of
// the configuration when none does.
func Probe(c Config, timeout time.Duration) error {
	if len(c.Addrs) == 0 {
		return fmt.Errorf("nacosx: no addrs configured (nacos.addrs)")
	}
	type result struct {
		addr string
		err  error
	}
	ch := make(chan result, len(c.Addrs))
	for _, a := range c.Addrs {
		go func(a string) { ch <- result{a, probeOne(a, timeout)} }(a)
	}
	var failures []string
	for range c.Addrs {
		r := <-ch
		if r.err == nil {
			return nil
		}
		failures = append(failures, r.err.Error())
	}
	sort.Strings(failures)
	return fmt.Errorf("cannot reach Nacos: %s. Check nacos.addrs in the service configuration", strings.Join(failures, "; "))
}

func probeOne(addr string, timeout time.Duration) error {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("%q is not host:port", addr)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return fmt.Errorf("%q has no numeric port", addr)
	}
	if err := dial(host, port, timeout); err != nil {
		return fmt.Errorf("%s: %v", addr, err)
	}
	grpc := port + grpcPortOffset
	if err := dial(host, grpc, timeout); err != nil {
		return fmt.Errorf("%s answers, but its gRPC port %d does not (%v); Nacos 2.x clients need main port + %d as well, often a firewall or an unpublished container port", addr, grpc, err, grpcPortOffset)
	}
	return nil
}

func dial(host string, port int, timeout time.Duration) error {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), timeout)
	if err != nil {
		var op *net.OpError
		if errors.As(err, &op) && op.Err != nil {
			return op.Err // "connection refused", "i/o timeout" without the dial boilerplate
		}
		return err
	}
	return conn.Close()
}

// NewNamingClient creates a service-discovery client.
func NewNamingClient(c Config) (naming_client.INamingClient, error) {
	p, err := c.params()
	if err != nil {
		return nil, err
	}
	return clients.NewNamingClient(p)
}

// NewConfigClient creates a configuration-center client.
func NewConfigClient(c Config) (config_client.IConfigClient, error) {
	p, err := c.params()
	if err != nil {
		return nil, err
	}
	return clients.NewConfigClient(p)
}

// Group returns the configured group or DEFAULT_GROUP.
func (c Config) GroupName() string { return c.group() }

func orDefault(v, d string) string {
	if v == "" {
		return d
	}
	return v
}
