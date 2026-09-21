package nacosx

import (
	"strings"
	"testing"

	"github.com/sezznaw/devkit-common/zlog/zlogtest"
)

func TestRegister(t *testing.T) {
	logs := zlogtest.Capture(t)
	c, naming, _ := newTestClient(t)
	dereg, err := c.Register(Registration{Service: "gateway", Addr: "10.0.0.5:8080", Metadata: map[string]string{"zone": "a"}})
	if err != nil {
		t.Fatal(err)
	}
	p := naming.registered[0]
	if p.Ip != "10.0.0.5" || p.Port != 8080 || p.Weight != 10 || p.ClusterName != "DEFAULT" || p.GroupName != "DEFAULT_GROUP" || !p.Ephemeral || !p.Enable || !p.Healthy || p.Metadata["zone"] != "a" {
		t.Errorf("registered as %+v", p)
	}
	if !logs.Has("INFO", "registered in Nacos") || !strings.Contains(logs.String(), `"instance":"10.0.0.5:8080"`) {
		t.Errorf("records: %s", logs)
	}
	if dereg() != nil || dereg() != nil || len(naming.deregistered) != 1 {
		t.Errorf("deregistering twice must reach Nacos once: %d", len(naming.deregistered))
	}
	if d := naming.deregistered[0]; d.Ip != "10.0.0.5" || d.Port != 8080 || d.Cluster != "DEFAULT" {
		t.Errorf("deregistered as %+v", d)
	}
	if !logs.Has("INFO", "deregistered from Nacos") {
		t.Errorf("records: %s", logs)
	}
}

func TestRegisterFillsInTheAddressOfThisMachine(t *testing.T) {
	zlogtest.Discard(t)
	c, naming, _ := newTestClient(t)
	if _, err := localIPv4(); err != nil {
		t.Skip("this machine has no IPv4 address besides loopback")
	}
	if _, err := c.Register(Registration{Service: "gateway", Addr: ":8080"}); err != nil {
		t.Fatal(err)
	}
	if ip := naming.registered[0].Ip; ip == "" || strings.HasPrefix(ip, "127.") {
		t.Errorf("registered with ip %q", ip)
	}
	for _, bad := range []string{"", "no-port", "h:0", "h:x"} {
		if _, err := c.Register(Registration{Service: "gateway", Addr: bad}); err == nil {
			t.Errorf("addr %q must be rejected", bad)
		}
	}
}

func TestInstances(t *testing.T) {
	logs := zlogtest.Capture(t)
	c, naming, _ := newTestClient(t)

	if _, err := c.Instances("ser-auth"); err == nil || !strings.Contains(err.Error(), "no instance of ser-auth") {
		t.Fatalf("no instances: %v", err)
	}
	if !logs.Has("WARN", "instances changed: none can take requests") {
		t.Errorf("the first look-up is on record even when it finds nothing: %s", logs)
	}

	disabled := inst("10.0.0.9", true)
	disabled.Enable = false
	naming.push("ser-auth", inst("10.0.0.7", true), inst("10.0.0.8", false), disabled)
	list, err := c.Instances("ser-auth")
	if err != nil || len(list) != 1 || list[0].Addr != "10.0.0.7:8888" {
		t.Fatalf("only what can take requests: %v %v", list, err)
	}
	in, err := c.Pick("ser-auth")
	if err != nil || in.Addr != "10.0.0.7:8888" {
		t.Fatalf("Pick: %v %v", in, err)
	}

	before := len(logs.Records())
	c.Instances("ser-auth")
	naming.push("ser-auth", inst("10.0.0.7", true), inst("10.0.0.8", false), disabled)
	if len(logs.Records()) != before {
		t.Errorf("no change, no record: %s", logs)
	}

	naming.push("ser-auth", inst("10.0.0.8", true), inst("10.0.0.6", true))
	recs := logs.Records()
	last := recs[len(recs)-1]
	changes, _ := last.Fields["instances"].([]any)
	if last.Msg != "instances changed" || last.Fields["usable"] != float64(2) || len(changes) != 4 {
		t.Fatalf("last record: %+v", last)
	}
	ops := ""
	for _, ch := range changes {
		m := ch.(map[string]any)
		ops += m["op"].(string) + " " + m["addr"].(string) + ","
	}
	if ops != "added 10.0.0.6:8888,removed 10.0.0.7:8888,changed 10.0.0.8:8888,removed 10.0.0.9:8888," {
		t.Errorf("changes: %s", ops)
	}
}

func TestServiceDiscoveredIsOnRecordOnce(t *testing.T) {
	logs := zlogtest.Capture(t)
	c, naming, _ := newTestClient(t)
	naming.push("ser-auth", inst("10.0.0.7", true))
	c.Instances("ser-auth")
	c.Instances("ser-auth")
	n := 0
	for _, r := range logs.Records() {
		if r.Msg == "service discovered" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("want one record, got %d: %s", n, logs)
	}
}

func TestSubscribe(t *testing.T) {
	zlogtest.Discard(t)
	c, naming, _ := newTestClient(t)
	naming.push("ser-auth", inst("10.0.0.7", true))
	var got [][]Instance
	if err := c.Subscribe("ser-auth", func(l []Instance) { got = append(got, l) }); err != nil {
		t.Fatal(err)
	}
	c.Subscribe("ser-auth", func([]Instance) { panic("boom") })
	naming.push("ser-auth", inst("10.0.0.7", true), inst("10.0.0.8", false))
	if len(got) != 2 || len(got[0]) != 1 || len(got[1]) != 2 {
		t.Fatalf("want the list of now and the change, unusable instances included: %v", got)
	}
}

func TestInstanceChangesOnTheConsole(t *testing.T) {
	down := Instance{Addr: "10.0.0.5:8888", Weight: 10, Enabled: true}
	c := InstanceChanges{
		{Op: "added", Instance: Instance{Addr: "10.0.0.17:8888", Weight: 10, Healthy: true, Enabled: true}},
		{Op: "changed", Instance: down, Was: "weight=10"},
		{Op: "removed", Instance: Instance{Addr: "10.0.0.6:8888"}},
	}
	want := "+ 10.0.0.17:8888  weight=10\n" +
		"~ 10.0.0.5:8888   weight=10 unhealthy (was weight=10)\n" +
		"- 10.0.0.6:8888\n"
	if got := c.LogBlock(false); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

// nacos.register: false is how a laptop works against the Nacos of the
// development server: everything but the announcement.
func TestRegisterSwitchedOff(t *testing.T) {
	logs := zlogtest.Capture(t)
	t.Setenv("APP_ENV", "") // a developer's machine
	naming, _, _ := fakeTarget(t)
	no := false
	c, err := New(Config{Addrs: []string{"10.0.0.5:8848"}, Register: &no})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	dereg, err := c.Register(Registration{Service: "order", Addr: ":8888"})
	if err != nil || dereg() != nil {
		t.Fatalf("Register: %v", err)
	}
	if len(naming.registered) != 0 || len(naming.deregistered) != 0 {
		t.Errorf("nothing may reach Nacos: %+v", naming.registered)
	}
	if !logs.Has("INFO", "not registered in Nacos: nacos.register is false") {
		t.Errorf("records: %s", logs)
	}
	naming.push("user", inst("10.0.0.7", true))
	if list, err := c.Instances("user"); err != nil || len(list) != 1 {
		t.Errorf("discovery must keep working: %v %v", list, err)
	}
}

// A laptop announces itself only in a Nacos on the laptop.
func TestALaptopDoesNotRegisterInASharedNacos(t *testing.T) {
	zlogtest.Discard(t)
	for _, c := range []struct {
		env   string
		addrs []string
		ok    bool
	}{
		{"", []string{"127.0.0.1:8848"}, true},
		{"", []string{"localhost:8848"}, true},
		{"", []string{"[::1]:8848"}, true},
		{"local", []string{"10.0.0.5:8848"}, false},
		{"", []string{"nacos.dev.company:8848"}, false},
		{"", []string{"127.0.0.1:8848", "10.0.0.5:8848"}, false},
		{"dev", []string{"10.0.0.5:8848"}, true},
		{"prod", []string{"nacos.prod:8848"}, true},
	} {
		t.Setenv("APP_ENV", c.env)
		err := Config{Addrs: c.addrs}.CheckRegistration()
		if (err == nil) != c.ok {
			t.Errorf("APP_ENV=%q %v: %v", c.env, c.addrs, err)
		}
		if err != nil && (!strings.Contains(err.Error(), "nacos.register: false") || !strings.Contains(err.Error(), c.addrs[len(c.addrs)-1])) {
			t.Errorf("the error must name the address and the way out: %v", err)
		}
	}

	t.Setenv("APP_ENV", "")
	naming, _, _ := fakeTarget(t)
	cli, err := New(Config{Addrs: []string{"10.0.0.5:8848"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cli.Close() })
	if _, err := cli.Register(Registration{Service: "order", Addr: ":8888"}); err == nil || len(naming.registered) != 0 {
		t.Errorf("must be refused before anything reaches Nacos: %v", err)
	}
}

// With a Nacos on this machine the instance is 127.0.0.1: it stays reachable
// when the Wi-Fi address changes.
func TestRegisterWithANacosOnThisMachine(t *testing.T) {
	if ip, _, err := instanceAddr(":8080", []string{"127.0.0.1:8848"}); err != nil || ip != "127.0.0.1" {
		t.Errorf("ip = %q, %v", ip, err)
	}
	if ip, _, err := instanceAddr("10.1.2.3:8080", []string{"127.0.0.1:8848"}); err != nil || ip != "10.1.2.3" {
		t.Errorf("a given host is kept: %q, %v", ip, err)
	}
}
