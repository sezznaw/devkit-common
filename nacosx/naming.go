package nacosx

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/nacos-group/nacos-sdk-go/v2/model"
	"github.com/nacos-group/nacos-sdk-go/v2/vo"

	"github.com/sezznaw/devkit-common/zlog"
)

// cluster is the Nacos cluster name instances are registered under. It is what
// kitex-contrib/registry-nacos used, so services built on an earlier version
// of this library and services built on this one find each other.
const cluster = "DEFAULT"

// Registration describes the instance of a service that this process is.
type Registration struct {
	// Service is the name callers find the service by.
	Service string
	// Addr is "host:port" of the listener. With an empty host (":8888") the
	// first IPv4 address of this machine that is not loopback is registered.
	Addr string
	// Weight is the share of the traffic, relative to the other instances.
	// Default 10.
	Weight float64
	// Metadata travels with the instance to those who discover it.
	Metadata map[string]string
}

// Instance is one instance of a service as Nacos knows it.
type Instance struct {
	// Addr is "ip:port".
	Addr     string            `json:"addr"`
	Weight   float64           `json:"weight"`
	Healthy  bool              `json:"healthy"`
	Enabled  bool              `json:"enabled"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

// usable says whether requests may be sent to the instance.
func (i Instance) usable() bool { return i.Healthy && i.Enabled && i.Weight > 0 }

// Register makes this process an instance of a service and returns the
// function that takes it out again. The instance is ephemeral: it also
// disappears when the process dies, and after a lost connection the SDK
// registers it again by itself.
//
// One client holds one instance per service: Nacos 2.x ties an ephemeral
// instance to the connection, and registering a second one for the same
// service replaces the first. A process that is several instances of a
// service needs a client for each (New, not Shared).
//
// For an offline client it does nothing.
func (c *Client) Register(r Registration) (deregister func() error, err error) {
	if r.Service == "" {
		return nil, errors.New("nacosx: Registration.Service is empty")
	}
	if c.IsOffline() {
		zlog.Debug("nacos is switched off, not registered", zlog.Str("name", r.Service))
		return func() error { return nil }, nil
	}
	ip, port, err := instanceAddr(r.Addr)
	if err != nil {
		return nil, fmt.Errorf("nacosx: register %s: %w", r.Service, err)
	}
	if r.Weight <= 0 {
		r.Weight = 10
	}
	group := c.cfg.GroupName()
	_, err = c.naming.RegisterInstance(vo.RegisterInstanceParam{
		Ip:          ip,
		Port:        port,
		ServiceName: r.Service,
		GroupName:   group,
		ClusterName: cluster,
		Weight:      r.Weight,
		Metadata:    r.Metadata,
		Enable:      true,
		Healthy:     true,
		Ephemeral:   true,
	})
	if err != nil {
		return nil, fmt.Errorf("nacosx: register %s: %w", r.Service, err)
	}
	instance := net.JoinHostPort(ip, strconv.FormatUint(port, 10))
	zlog.Info("registered in Nacos",
		zlog.Str("name", r.Service).Green(),
		zlog.Str("instance", instance),
		zlog.Str("group", group),
		zlog.Str("namespace", c.cfg.namespaceName()),
		zlog.Float("weight", r.Weight))

	var once sync.Once
	return func() (err error) {
		once.Do(func() {
			_, err = c.naming.DeregisterInstance(vo.DeregisterInstanceParam{
				Ip:          ip,
				Port:        port,
				ServiceName: r.Service,
				GroupName:   group,
				Cluster:     cluster,
				Ephemeral:   true,
			})
			if err != nil {
				err = fmt.Errorf("nacosx: deregister %s: %w", r.Service, err)
				return
			}
			zlog.Info("deregistered from Nacos", zlog.Str("name", r.Service), zlog.Str("instance", instance))
		})
		return err
	}, nil
}

// instanceAddr splits "host:port" and fills in this machine's address for an
// empty host: a listener on all interfaces has no address of its own, and
// callers need one.
func instanceAddr(addr string) (ip string, port uint64, err error) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return "", 0, fmt.Errorf("addr %q is not host:port", addr)
	}
	port, err = strconv.ParseUint(portStr, 10, 16)
	if err != nil || port == 0 {
		return "", 0, fmt.Errorf("addr %q has no usable port", addr)
	}
	if host == "" || host == "::" || host == "0.0.0.0" {
		if host, err = localIPv4(); err != nil {
			return "", 0, err
		}
	}
	return host, port, nil
}

func localIPv4() (string, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "", err
	}
	for _, a := range addrs {
		if ipNet, ok := a.(*net.IPNet); ok && !ipNet.IP.IsLoopback() {
			if v4 := ipNet.IP.To4(); v4 != nil {
				return v4.String(), nil
			}
		}
	}
	return "", errors.New("this machine has no IPv4 address besides loopback; give the address to register in full, host:port")
}

// service is what the client knows about one service it discovers.
type service struct {
	name string

	// subMu guards param only. It is not mu: the SDK calls the callback from
	// inside Subscribe, on the same goroutine, and the callback takes mu.
	subMu sync.Mutex
	param *vo.SubscribeParam

	mu    sync.Mutex
	seen  bool
	known map[string]Instance // by address, as of the last record
	subs  []func([]Instance)
}

// Instances returns the instances of a service that requests may be sent to:
// healthy, enabled, with a weight. There being none is an error.
//
// The first call for a service subscribes to it. From then on the answer comes
// from memory, Nacos pushes what changes, and every change of the list is a
// record, "instances changed".
func (c *Client) Instances(name string) ([]Instance, error) {
	if c.IsOffline() {
		return nil, fmt.Errorf("nacosx: cannot look up %s: nacos is switched off. Say where the service is instead, for Kitex with client.WithHostPorts", name)
	}
	s, err := c.subscribe(name)
	if err != nil {
		return nil, err
	}
	all, err := c.naming.SelectAllInstances(vo.SelectAllInstancesParam{
		ServiceName: name,
		GroupName:   c.cfg.GroupName(),
	})
	if err != nil {
		return nil, fmt.Errorf("nacosx: look up %s: %w", name, err)
	}
	list := convert(all)
	s.observe(list, false)
	usable := list[:0:0]
	for _, in := range list {
		if in.usable() {
			usable = append(usable, in)
		}
	}
	if len(usable) == 0 {
		return nil, fmt.Errorf("nacosx: no instance of %s can take requests (group %s, namespace %s; %d registered)",
			name, c.cfg.GroupName(), c.cfg.namespaceName(), len(list))
	}
	return usable, nil
}

// Pick returns one usable instance, chosen at random in proportion to the
// weights. It is for callers without a load balancer of their own; Kitex has
// one and takes the whole list.
func (c *Client) Pick(name string) (Instance, error) {
	list, err := c.Instances(name)
	if err != nil {
		return Instance{}, err
	}
	total := 0.0
	for _, in := range list {
		total += in.Weight
	}
	at := rand.Float64() * total
	for _, in := range list {
		if at -= in.Weight; at < 0 {
			return in, nil
		}
	}
	return list[len(list)-1], nil
}

// Subscribe calls fn with all instances of a service, the unusable ones
// included, whenever the list changes, and once with the list of now. The
// calls come one at a time; a panic in fn is logged.
func (c *Client) Subscribe(name string, fn func([]Instance)) error {
	if c.IsOffline() {
		return fmt.Errorf("nacosx: cannot subscribe to %s: nacos is switched off", name)
	}
	s, err := c.subscribe(name)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.subs = append(s.subs, fn)
	s.call(fn, s.list())
	return nil
}

// subscribe makes sure that Nacos pushes the changes of a service.
func (c *Client) subscribe(name string) (*service, error) {
	if name == "" {
		return nil, errors.New("nacosx: the service name is empty")
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, errors.New("nacosx: the client is closed")
	}
	s, ok := c.services[name]
	if !ok {
		s = &service{name: name, known: map[string]Instance{}}
		c.services[name] = s
	}
	c.mu.Unlock()

	s.subMu.Lock()
	defer s.subMu.Unlock()
	if s.param != nil {
		return s, nil
	}
	param := &vo.SubscribeParam{
		ServiceName: name,
		GroupName:   c.cfg.GroupName(),
		SubscribeCallback: func(instances []model.Instance, err error) {
			if err == nil {
				s.observe(convert(instances), true)
			}
		},
	}
	if err := c.naming.Subscribe(param); err != nil {
		return nil, fmt.Errorf("nacosx: subscribe to %s: %w", name, err)
	}
	s.param = param
	return s, nil
}

// observe compares a list with the one of the last record and, when they
// differ, writes the next record and tells the subscribers. Both the pushes of
// Nacos and the look-ups come through here, so that the first look-up of a
// service is on record whichever of the two is first.
//
// A look-up that finds nothing is not taken as "all gone": the SDK keeps the
// last list when Nacos pushes an empty one (its protection against a Nacos
// that lost its data), and the record should say what callers really use.
func (s *service) observe(list []Instance, pushed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(list) == 0 && !pushed && s.seen {
		return
	}
	cur := make(map[string]Instance, len(list))
	for _, in := range list {
		cur[in.Addr] = in
	}
	changes := instanceDiff(s.known, cur)
	first := !s.seen
	s.seen, s.known = true, cur
	if len(changes) == 0 && !first {
		return
	}
	usable := 0
	for _, in := range list {
		if in.usable() {
			usable++
		}
	}
	fields := []zlog.Field{zlog.Str("name", s.name).Cyan(), zlog.Int("usable", usable), zlog.Int("total", len(list))}
	if len(changes) > 0 {
		fields = append(fields, zlog.Any("instances", changes))
	}
	switch {
	case usable == 0:
		zlog.Warn("instances changed: none can take requests", fields...)
	case first:
		zlog.Info("service discovered", fields...)
	default:
		zlog.Info("instances changed", fields...)
	}
	for _, fn := range s.subs {
		s.call(fn, s.list())
	}
}

func (s *service) list() []Instance {
	out := make([]Instance, 0, len(s.known))
	for _, in := range s.known {
		out = append(out, in)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Addr < out[j].Addr })
	return out
}

func (s *service) call(fn func([]Instance), list []Instance) {
	defer func() {
		if r := recover(); r != nil {
			zlog.Error("instance subscriber panicked", zlog.Str("name", s.name),
				zlog.Any("panic", r), zlog.Str("panic_stack", string(debug.Stack())))
		}
	}()
	fn(list)
}

func convert(in []model.Instance) []Instance {
	out := make([]Instance, 0, len(in))
	for _, m := range in {
		out = append(out, Instance{
			Addr:     net.JoinHostPort(m.Ip, strconv.FormatUint(m.Port, 10)),
			Weight:   m.Weight,
			Healthy:  m.Healthy,
			Enabled:  m.Enable,
			Metadata: m.Metadata,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Addr < out[j].Addr })
	return out
}

// InstanceChange is one instance that came, went, or is not what it was.
type InstanceChange struct {
	// Op is "added", "removed" or "changed".
	Op       string `json:"op"`
	Instance        // as it is now; for "removed", as it was
	Was      string `json:"was,omitempty"` // for "changed": what it was, in words
}

// InstanceChanges is what a record of a changed instance list carries: an
// array in JSON, and on the console a line per instance below the record:
//
//   - 10.0.0.7:8888  weight=10
//   - 10.0.0.6:8888
//     ~ 10.0.0.5:8888  weight=10 unhealthy (was healthy)
type InstanceChanges []InstanceChange

func instanceDiff(old, cur map[string]Instance) InstanceChanges {
	var out InstanceChanges
	for addr, in := range cur {
		was, ok := old[addr]
		switch {
		case !ok:
			out = append(out, InstanceChange{Op: "added", Instance: in})
		case was.state() != in.state():
			out = append(out, InstanceChange{Op: "changed", Instance: in, Was: was.state()})
		}
	}
	for addr, in := range old {
		if _, still := cur[addr]; !still {
			out = append(out, InstanceChange{Op: "removed", Instance: in})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Addr < out[j].Addr })
	return out
}

// state is what about an instance matters to a caller, in words. Metadata is
// not part of it: it does not decide whether requests go there.
func (i Instance) state() string {
	s := "weight=" + strconv.FormatFloat(i.Weight, 'f', -1, 64)
	if !i.Healthy {
		s += " unhealthy"
	}
	if !i.Enabled {
		s += " disabled"
	}
	return s
}

func (c InstanceChanges) LogBlock(colored bool) string {
	width := 0
	for _, ch := range c {
		width = max(width, len(ch.Addr))
	}
	var b strings.Builder
	for _, ch := range c {
		var line string
		paint := zlog.Yellow
		switch ch.Op {
		case "added":
			line, paint = fmt.Sprintf("+ %-*s  %s", width, ch.Addr, ch.state()), zlog.Green
		case "removed":
			line, paint = "- "+ch.Addr, zlog.Red
		default:
			line = fmt.Sprintf("~ %-*s  %s (was %s)", width, ch.Addr, ch.state(), ch.Was)
		}
		if colored {
			line = fmt.Sprint(paint(line))
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}
