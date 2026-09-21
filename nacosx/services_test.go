package nacosx

import (
	"errors"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nacos-group/nacos-sdk-go/v2/clients/naming_client"
	"github.com/nacos-group/nacos-sdk-go/v2/model"

	"github.com/sezznaw/devkit-common/zlog/zlogtest"
)

// watchTarget gives the watch a fake Nacos of its own, as the real one has a
// client of its own.
func watchTarget(t *testing.T) *fakeNaming {
	t.Helper()
	watched := newFakeNaming()
	orig := newWatchNamingFunc
	newWatchNamingFunc = func(Config) (naming_client.INamingClient, error) { return watched, nil }
	t.Cleanup(func() { newWatchNamingFunc = orig })
	return watched
}

func at(ip string, port uint64) model.Instance {
	in := inst(ip, true)
	in.Port = port
	return in
}

// The owner's requirement: when ser-auth starts, ser-user says so and says
// what is alive, with the number of instances of every service; the same when
// something goes.
func TestWatchServices(t *testing.T) {
	logs := zlogtest.Capture(t)
	c, _, _ := newTestClient(t)
	nacos := watchTarget(t)
	nacos.push("ser-user", at("10.0.0.1", 8888), at("10.0.0.2", 8888))

	c.WatchServices(time.Hour) // the rounds are made by hand below
	recs := logs.Records()
	first := recs[len(recs)-1]
	if first.Level != "INFO" || first.Msg != "services alive" || first.Fields["services"] != float64(1) || first.Fields["instances"] != float64(2) {
		t.Fatalf("the first record must be the overview: %+v", first)
	}
	if n := len(logs.Records()); n != len(recs) {
		t.Fatalf("what was there at start is in the overview, not in records of its own: %d", n)
	}

	// A service with a name that is new: found by the next look at the list.
	nacos.push("ser-auth", at("10.0.0.3", 8889))
	if err := c.watch.round(); err != nil {
		t.Fatal(err)
	}
	online := last(t, logs)
	if online.Msg != "service online" || online.Fields["name"] != "ser-auth" || online.Fields["alive"] != float64(1) ||
		online.Fields["services"] != float64(2) || online.Fields["instances"] != float64(3) {
		t.Fatalf("online: %+v", online)
	}
	if got := overviewOf(t, online); got != "ser-auth x1 10.0.0.3:8889 | ser-user x2 10.0.0.1:8888 10.0.0.2:8888" {
		t.Errorf("alive_now = %s", got)
	}
	if ch := report(online)["changed"].([]any)[0].(map[string]any); ch["op"] != "added" || ch["addr"] != "10.0.0.3:8889" || report(online)["service"] != "ser-auth" {
		t.Errorf("changed = %v", report(online))
	}

	// A second instance of a known service is pushed by Nacos.
	nacos.push("ser-auth", at("10.0.0.3", 8889), at("10.0.0.4", 8889))
	if more := last(t, logs); more.Msg != "service instances changed" || more.Fields["alive"] != float64(2) || more.Fields["instances"] != float64(4) {
		t.Fatalf("second instance: %+v", more)
	}

	// The same list again is no news.
	n := len(logs.Records())
	nacos.push("ser-auth", at("10.0.0.3", 8889), at("10.0.0.4", 8889))
	if len(logs.Records()) != n {
		t.Errorf("an unchanged list must not be a record: %+v", last(t, logs))
	}

	// The LAST instance of a service goes: the record the calling path never
	// gets, because the SDK protects it from empty lists.
	nacos.push("ser-user")
	offline := last(t, logs)
	if offline.Msg != "service offline" || offline.Fields["name"] != "ser-user" || offline.Fields["alive"] != float64(0) || offline.Fields["services"] != float64(1) {
		t.Fatalf("offline: %+v", offline)
	}
	if got := overviewOf(t, offline); got != "ser-auth x2 10.0.0.3:8889 10.0.0.4:8889" {
		t.Errorf("alive_now = %s", got)
	}
	if ops := report(offline)["changed"].([]any); len(ops) != 2 || ops[0].(map[string]any)["op"] != "removed" {
		t.Errorf("changed = %v", ops)
	}

	// An instance that is there but cannot take requests is not alive.
	nacos.push("ser-auth", at("10.0.0.3", 8889), inst("10.0.0.4", false))
	if sick := last(t, logs); sick.Fields["alive"] != float64(1) || !strings.Contains(overviewText(c), "! 1 more cannot take requests") {
		t.Errorf("unhealthy: %+v\n%s", sick, overviewText(c))
	}
}

func TestWatchServicesOnTheConsole(t *testing.T) {
	alive := ServiceOverview{
		{Name: "ser-auth", Alive: 1, Instances: []string{"127.0.0.1:8889"}},
		{Name: "user", Alive: 2, Instances: []string{"127.0.0.1:8888", "127.0.0.1:8890"}, Unusable: 1},
	}
	for name, c := range map[string]struct {
		report ServiceReport
		want   string
	}{
		"what is alive": {ServiceReport{Alive: alive}, `
┌──────────┬───┬───────────────────────────────┐
│ SERVICE  │ N │ INSTANCES                     │
├──────────┼───┼───────────────────────────────┤
│ ser-auth │ 1 │   127.0.0.1:8889              │
│ user     │ 2 │   127.0.0.1:8888              │
│          │   │   127.0.0.1:8890              │
│          │   │ ! 1 more cannot take requests │
└──────────┴───┴───────────────────────────────┘
`},
		"an instance came, one went": {ServiceReport{Service: "user", Alive: alive[:1:1].with(ServiceState{Name: "user", Alive: 1, Instances: []string{"127.0.0.1:8890"}}), Changed: InstanceChanges{
			{Op: "added", Instance: Instance{Addr: "127.0.0.1:8890", Weight: 10, Healthy: true, Enabled: true}},
			{Op: "removed", Instance: Instance{Addr: "127.0.0.1:8888"}},
		}}, `
┌──────────┬───┬──────────────────┐
│ SERVICE  │ N │ INSTANCES        │
├──────────┼───┼──────────────────┤
│ ser-auth │ 1 │   127.0.0.1:8889 │
│ user     │ 1 │ + 127.0.0.1:8890 │
│          │   │ - 127.0.0.1:8888 │
└──────────┴───┴──────────────────┘
`},
		"nothing of a service is left: it keeps its row": {ServiceReport{Service: "pay", Alive: alive[:1], Changed: InstanceChanges{
			{Op: "removed", Instance: Instance{Addr: "127.0.0.1:9000"}},
		}}, `
┌──────────┬───┬──────────────────┐
│ SERVICE  │ N │ INSTANCES        │
├──────────┼───┼──────────────────┤
│ pay      │ 0 │ - 127.0.0.1:9000 │
│ ser-auth │ 1 │   127.0.0.1:8889 │
└──────────┴───┴──────────────────┘
`},
		"this process is pointed out": {ServiceReport{Alive: alive[:1], This: map[string]bool{"ser-auth@127.0.0.1:8889": true}}, `
┌──────────┬───┬──────────────────────────────────┐
│ SERVICE  │ N │ INSTANCES                        │
├──────────┼───┼──────────────────────────────────┤
│ ser-auth │ 1 │   127.0.0.1:8889  ← this process │
└──────────┴───┴──────────────────────────────────┘
`},
		"nothing at all": {ServiceReport{}, "\n(no service is alive)\n"},
	} {
		if got := "\n" + c.report.LogBlock(false); got != c.want {
			t.Errorf("%s:\ngot:%s\nwant:%s", name, got, c.want)
		}
		// Colors must not move the frame: without them the text is the same.
		if got := "\n" + stripANSI(c.report.LogBlock(true)); got != c.want {
			t.Errorf("%s, colored:\ngot:%s\nwant:%s", name, got, c.want)
		}
	}
}

func (o ServiceOverview) with(s ServiceState) ServiceOverview { return append(o, s) }

var ansi = regexp.MustCompile("\x1b\\[[0-9;]*m")

func stripANSI(s string) string { return ansi.ReplaceAllString(s, "") }

// The records of the calling path repeat what the watch says, so they go to
// debug; without the watch they stay where they were.
func TestWatchServicesQuietensTheCallingPath(t *testing.T) {
	logs := zlogtest.Capture(t)
	c, naming, _ := newTestClient(t)
	watchTarget(t)
	c.WatchServices(time.Hour)
	naming.push("pay", inst("10.0.0.9", true))
	if _, err := c.Instances("pay"); err != nil {
		t.Fatal(err)
	}
	if logs.Has("INFO", "service discovered") || !logs.Has("DEBUG", "service discovered") {
		t.Errorf("records: %s", logs)
	}
	naming.push("pay", inst("10.0.0.9", false))
	if !logs.Has("WARN", "instances changed: none can take requests") {
		t.Errorf("that nothing can take the requests of this service stays a warning: %s", logs)
	}
}

// Records are not worth a failed start, and an outage is one warning.
func TestWatchServicesSurvivesNacosTrouble(t *testing.T) {
	logs := zlogtest.Capture(t)
	c, _, _ := newTestClient(t)
	nacos := watchTarget(t)
	nacos.listErr = errors.New("nacos is restarting")
	c.WatchServices(time.Hour)
	if !logs.Has("WARN", "cannot list the services in Nacos; trying again") || !logs.Has("INFO", "services alive") {
		t.Fatalf("records: %s", logs)
	}
	nacos.listErr = nil
	nacos.push("ser-user", at("10.0.0.1", 8888))
	if err := c.watch.round(); err != nil {
		t.Fatal(err)
	}
	if got := last(t, logs); got.Msg != "service online" {
		t.Errorf("after the outage: %+v", got)
	}
	c.WatchServices(time.Hour) // a second call does nothing
	if n := strings.Count(logs.String(), `"services alive"`); n != 1 {
		t.Errorf("%d overviews", n)
	}

	off := Offline(t.TempDir())
	off.WatchServices(time.Hour)
	if off.watch != nil {
		t.Error("an offline client has nothing to watch")
	}
}

func last(t *testing.T, logs *zlogtest.Logs) zlogtest.Record {
	t.Helper()
	recs := logs.Records()
	if len(recs) == 0 {
		t.Fatal("no record")
	}
	return recs[len(recs)-1]
}

func overviewOf(t *testing.T, r zlogtest.Record) string {
	t.Helper()
	var parts []string
	for _, s := range report(r)["alive"].([]any) {
		m := s.(map[string]any)
		line := m["name"].(string) + " x" + jsonNumber(m["alive"])
		for _, a := range m["instances"].([]any) {
			line += " " + a.(string)
		}
		parts = append(parts, line)
	}
	return strings.Join(parts, " | ")
}

func report(r zlogtest.Record) map[string]any { return r.Fields["overview"].(map[string]any) }

func jsonNumber(v any) string { return strconv.Itoa(int(v.(float64))) }

func overviewText(c *Client) string {
	c.watch.mu.Lock()
	defer c.watch.mu.Unlock()
	return c.watch.overview().LogBlock(false)
}

// The table says which of the instances is the process that prints it.
func TestWatchServicesPointsOutThisProcess(t *testing.T) {
	logs := zlogtest.Capture(t)
	c, _, _ := newTestClient(t)
	nacos := watchTarget(t)
	dereg, err := c.Register(Registration{Service: "ser-user", Addr: "10.0.0.1:8888"})
	if err != nil {
		t.Fatal(err)
	}
	nacos.push("ser-user", at("10.0.0.1", 8888), at("10.0.0.2", 8888))
	c.WatchServices(time.Hour)
	if !logs.Has("INFO", "services alive") {
		t.Fatalf("records: %s", logs)
	}
	c.watch.mu.Lock()
	text := ServiceReport{Alive: c.watch.overview(), This: c.watch.this()}.LogBlock(false)
	c.watch.mu.Unlock()
	if !strings.Contains(text, "10.0.0.1:8888  ← this process") || strings.Contains(text, "10.0.0.2:8888  ←") {
		t.Errorf("table:\n%s", text)
	}
	if dereg() != nil || len(c.watch.this()) != 0 {
		t.Errorf("after deregistering nothing here is this process: %v", c.watch.this())
	}
}
