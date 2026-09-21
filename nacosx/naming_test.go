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
