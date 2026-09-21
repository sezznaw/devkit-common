// Package nacosx is how a service talks to Nacos: it registers itself, finds
// the services it calls, and reads configuration that follows Nacos while the
// program runs. Everything it has to say goes through zlog.
//
//	nc, err := nacosx.New(cfg.Nacos)
//
//	// Configuration that refreshes itself.
//	dyn, err := nacosx.Watch[Dynamic](nc, "ser-user.yaml")
//	dyn.Get().Battle.MaxLevel // always the current value
//
//	// Registration and discovery.
//	deregister, err := nc.Register(nacosx.Registration{Service: "gateway", Addr: ":8080"})
//	ins, err := nc.Pick("ser-user")
//
// A Kitex service does not call Register or Pick itself: kitexx does, see
// kitexx.Options, kitexx.ClientOptions and kitexx.WatchConfig.
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

	"github.com/sezznaw/devkit-common/config"
	"github.com/sezznaw/devkit-common/zlog"
)

// Config is the YAML-loadable Nacos connection config.
type Config struct {
	// Addrs are "host:port" of the Nacos servers.
	Addrs []string `yaml:"addrs"`
	// Namespace is the Nacos namespace id ("" = public).
	Namespace string `yaml:"namespace"`
	// Group defaults to DEFAULT_GROUP. It applies to services and to
	// configurations alike.
	Group    string `yaml:"group"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
	// Register says whether this process announces itself. false keeps
	// everything else: the services it calls are found and its configuration
	// is read, it just does not become an instance that others are sent to.
	// That is how a developer's machine works against the Nacos of the
	// development server. Default true.
	Register *bool `yaml:"register"`
	// CacheDir defaults to /tmp/nacos/cache.
	CacheDir string `yaml:"cache_dir"`
	// SDKLogLevel is the level from which the Nacos SDK's own records are let
	// through to zlog: debug, info, warn (the default) or error. Mind that at
	// info the SDK logs the content of every configuration it receives,
	// passwords included.
	SDKLogLevel string `yaml:"sdk_log_level"`
	// LogDir is no longer used: the SDK logs through zlog instead of into a
	// file. Still read so that an existing configuration file stays valid.
	LogDir string `yaml:"log_dir"`
}

// GroupName returns the configured group or DEFAULT_GROUP.
func (c Config) GroupName() string {
	if c.Group == "" {
		return constant.DEFAULT_GROUP
	}
	return c.Group
}

// Registers reports whether Register announces this process (nacos.register).
func (c Config) Registers() bool { return c.Register == nil || *c.Register }

// CheckRegistration refuses the one registration that hurts other people: a
// developer's machine (the local environment) announcing itself in a Nacos
// that is not on that machine. Everybody who uses that Nacos would have a
// share of their requests sent to a laptop, to code that is half written or
// standing on a breakpoint.
func (c Config) CheckRegistration() error {
	if !c.Registers() || !config.IsLocal() {
		return nil
	}
	for _, a := range c.Addrs {
		if !isLoopback(a) {
			return fmt.Errorf("nacosx: this is a developer's machine (%s is %q) and Nacos at %s is not on it: "+
				"registered there, this machine would receive requests of everybody who uses that Nacos. "+
				"To work against it, set nacos.register: false, which finds the other services and reads the configuration without announcing this one",
				config.EnvVar, config.Env(), a)
		}
	}
	return nil
}

// isLoopback reports whether the Nacos address "host:port" is this machine.
func isLoopback(addr string) bool {
	host := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

func (c Config) namespaceName() string {
	if c.Namespace == "" {
		return "public"
	}
	return c.Namespace
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
		// Without this, GetConfig answers from the snapshot in the cache
		// directory when the server does not, and says nothing about it: a
		// service would start on a configuration of unknown age.
		constant.WithDisableUseSnapShot(true),
		constant.WithCacheDir(orDefault(c.CacheDir, "/tmp/nacos/cache")),
	)
	return vo.NacosClientParam{ClientConfig: cc, ServerConfigs: servers}, nil
}

// Client is one connection to Nacos, or, made by Offline, a stand-in that
// works without a server. It is safe for concurrent use.
type Client struct {
	cfg Config
	// offlineDir is set for a client without a server, see Offline.
	offlineDir string

	naming naming_client.INamingClient

	mu       sync.Mutex
	config   config_client.IConfigClient // created when first needed
	watches  map[string]*watch           // by group + data id
	services map[string]*service         // by service name
	closed   bool
	stop     chan struct{}
}

var (
	newNamingFunc = newNamingClient // replaced in tests
	newConfigFunc = newConfigClient // replaced in tests
	probeFunc     = Probe           // replaced in tests
)

// New connects to Nacos. It fails when no server can be reached, with an error
// that names the address and the likely cause.
func New(c Config) (*Client, error) {
	start := time.Now()
	// The SDK itself does not fail when Nacos is unreachable; it retries in
	// the background and the first real call fails with "client not connected,
	// current status:STARTING", which names neither the address nor the cause.
	if err := probeFunc(c, 2*time.Second); err != nil {
		return nil, err
	}
	// Before the first SDK client exists: that is when the SDK would otherwise
	// install its own logger, which writes to a file.
	bridgeSDKLog(c.SDKLogLevel)
	naming, err := newNamingFunc(c)
	if err != nil {
		return nil, fmt.Errorf("nacosx: %w", err)
	}
	cli := NewFrom(c, naming, nil)
	zlog.Info("nacos connected",
		zlog.Any("addrs", c.Addrs),
		zlog.Str("namespace", c.namespaceName()),
		zlog.Str("group", c.GroupName()),
		zlog.Dur("took", time.Since(start).Round(time.Millisecond)))
	go cli.watchConnection(healthInterval)
	return cli, nil
}

// NewFrom makes a client of SDK clients that exist already, which is how a
// test puts fakes underneath. config may be nil: it is then created from c
// when first needed. Nothing is probed, announced or watched.
func NewFrom(c Config, naming naming_client.INamingClient, config config_client.IConfigClient) *Client {
	return &Client{
		cfg:      c,
		naming:   naming,
		config:   config,
		watches:  map[string]*watch{},
		services: map[string]*service{},
		stop:     make(chan struct{}),
	}
}

// Offline returns a client for running without Nacos, which is what a
// developer's machine usually does. Configurations are files, <dir>/<data id>,
// and are followed like the ones in Nacos: saving the file is a change.
// Register does nothing, and no service is found: say where a service is
// instead, for Kitex with client.WithHostPorts.
func Offline(dir string) *Client {
	zlog.Info("nacos is switched off: nothing is registered or discovered, configurations are files", zlog.Str("dir", dir))
	return &Client{
		offlineDir: dir,
		watches:    map[string]*watch{},
		services:   map[string]*service{},
		stop:       make(chan struct{}),
	}
}

// IsOffline reports whether the client was made by Offline.
func (c *Client) IsOffline() bool { return c.offlineDir != "" }

// Naming returns the SDK's naming client, for what this package has no method
// for. It is nil for an offline client.
func (c *Client) Naming() naming_client.INamingClient { return c.naming }

// ConfigClient returns the SDK's configuration client, created on first use:
// a service that reads no configuration from Nacos does not pay for the second
// connection. It is nil, with an error, for an offline client.
func (c *Client) ConfigClient() (config_client.IConfigClient, error) {
	if c.IsOffline() {
		return nil, errors.New("nacosx: the client is offline")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, errors.New("nacosx: the client is closed")
	}
	if c.config == nil {
		cc, err := newConfigFunc(c.cfg)
		if err != nil {
			return nil, fmt.Errorf("nacosx: %w", err)
		}
		c.config = cc
	}
	return c.config, nil
}

// Close stops following configurations and services and closes the
// connections. Instances registered through this client disappear from Nacos
// with the connection; deregister them first where callers must notice in
// time.
func (c *Client) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	close(c.stop)
	config := c.config
	c.mu.Unlock()

	if config != nil {
		config.CloseClient()
	}
	if c.naming != nil {
		c.naming.CloseClient()
	}
	return nil
}

var (
	sharedMu sync.Mutex
	shared   = map[string]*Client{}
)

// Shared returns one client per distinct Nacos target for the whole process.
// A service registers itself, resolves every downstream and reads its
// configuration through Nacos; without sharing, each of those would open its
// own connections and keep its own caches.
func Shared(c Config) (*Client, error) {
	key := fmt.Sprintf("%v|%s|%s|%s", c.Addrs, c.Namespace, c.GroupName(), c.Username)
	sharedMu.Lock()
	defer sharedMu.Unlock()
	if cli, ok := shared[key]; ok {
		return cli, nil
	}
	cli, err := New(c)
	if err != nil {
		return nil, err
	}
	shared[key] = cli
	return cli, nil
}

// CloseShared closes every client Shared has made.
func CloseShared() {
	sharedMu.Lock()
	list := shared
	shared = map[string]*Client{}
	sharedMu.Unlock()
	for _, cli := range list {
		_ = cli.Close()
	}
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

func newNamingClient(c Config) (naming_client.INamingClient, error) {
	p, err := c.params()
	if err != nil {
		return nil, err
	}
	return clients.NewNamingClient(p)
}

func newConfigClient(c Config) (config_client.IConfigClient, error) {
	p, err := c.params()
	if err != nil {
		return nil, err
	}
	return clients.NewConfigClient(p)
}

// healthInterval is how often the connection is looked at.
const healthInterval = 2 * time.Second

// watchConnection keeps the process informed of whether the connection to
// Nacos is up; what is made of it is in outage.go. The SDK keeps its state to
// itself: listeners can only be registered inside it, so it is asked. Nothing
// has to be done about a lost connection, the SDK reconnects and then
// registers and subscribes again; but whoever reads the log of an incident
// wants to know that, for those twelve seconds, instance lists and
// configurations were not following Nacos.
func (c *Client) watchConnection(every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	defer outage.forget(c)
	for {
		select {
		case <-c.stop:
			return
		case <-t.C:
		}
		outage.report(c, c.naming.ServerHealthy(), 2*every)
	}
}

func orDefault(v, d string) string {
	if v == "" {
		return d
	}
	return v
}
