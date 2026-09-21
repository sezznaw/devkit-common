package nacosx

import (
	"errors"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nacos-group/nacos-sdk-go/v2/clients/config_client"
	"github.com/nacos-group/nacos-sdk-go/v2/clients/naming_client"
	sdklogger "github.com/nacos-group/nacos-sdk-go/v2/common/logger"
	"go.uber.org/zap/zapcore"

	"github.com/sezznaw/devkit-common/zlog"
	"github.com/sezznaw/devkit-common/zlog/zlogtest"
)

// fakeTarget makes New work without a server, and counts the clients made.
func fakeTarget(t *testing.T) (naming *fakeNaming, config *fakeConfig, made *int) {
	t.Helper()
	naming, config, made = newFakeNaming(), &fakeConfig{}, new(int)
	origN, origC, origP := newNamingFunc, newConfigFunc, probeFunc
	newNamingFunc = func(Config) (naming_client.INamingClient, error) { *made++; return naming, nil }
	newConfigFunc = func(Config) (config_client.IConfigClient, error) { return config, nil }
	probeFunc = func(Config, time.Duration) error { return nil }
	t.Cleanup(func() {
		newNamingFunc, newConfigFunc, probeFunc = origN, origC, origP
		CloseShared()
	})
	return naming, config, made
}

func newTestClient(t *testing.T) (*Client, *fakeNaming, *fakeConfig) {
	t.Helper()
	naming, config, _ := fakeTarget(t)
	c, err := New(Config{Addrs: []string{"n1:8848"}, Namespace: "dev"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c, naming, config
}

func TestSharedIsCreatedOncePerTarget(t *testing.T) {
	zlogtest.Discard(t)
	_, _, made := fakeTarget(t)
	a := Config{Addrs: []string{"n1:8848"}, Namespace: "dev"}
	c1, _ := Shared(a)
	c2, _ := Shared(a)
	if c1 != c2 || *made != 1 {
		t.Fatalf("same target must share one client: made=%d", *made)
	}
	if _, _ = Shared(Config{Addrs: []string{"n1:8848"}, Namespace: "prod"}); *made != 2 {
		t.Fatalf("a different namespace needs its own client: made=%d", *made)
	}
}

func TestNewAnnouncesTheConnection(t *testing.T) {
	logs := zlogtest.Capture(t)
	newTestClient(t)
	recs := logs.Records()
	if len(recs) != 1 || recs[0].Msg != "nacos connected" || recs[0].Fields["namespace"] != "dev" || recs[0].Fields["group"] != "DEFAULT_GROUP" {
		t.Fatalf("records: %s", logs)
	}
}

func TestParamsValidation(t *testing.T) {
	if _, err := (Config{}).params(); err == nil {
		t.Error("empty addrs must be rejected")
	}
	if _, err := (Config{Addrs: []string{"no-port"}}).params(); err == nil {
		t.Error("addr without port must be rejected")
	}
}

// listenPair opens a main port and main+1000, like a Nacos 2.x server.
func listenPair(t *testing.T) (addr string, closeGRPC func()) {
	for i := 0; i < 20; i++ {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := l.Addr().(*net.TCPAddr).Port
		g, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port+grpcPortOffset))
		if err != nil {
			l.Close()
			continue // that port happens to be taken, try another pair
		}
		t.Cleanup(func() { l.Close(); g.Close() })
		return l.Addr().String(), func() { g.Close() }
	}
	t.Skip("could not find a free port pair")
	return "", nil
}

func TestProbe(t *testing.T) {
	addr, closeGRPC := listenPair(t)
	if err := Probe(Config{Addrs: []string{addr}}, time.Second); err != nil {
		t.Fatalf("both ports open, got %v", err)
	}
	// One dead server does not matter while another answers.
	if err := Probe(Config{Addrs: []string{"127.0.0.1:1", addr}}, time.Second); err != nil {
		t.Fatalf("one reachable server is enough, got %v", err)
	}

	closeGRPC()
	err := Probe(Config{Addrs: []string{addr}}, time.Second)
	if err == nil || !strings.Contains(err.Error(), "gRPC port") || !strings.Contains(err.Error(), "main port + 1000") {
		t.Fatalf("a closed gRPC port must be explained, got %v", err)
	}

	err = Probe(Config{Addrs: []string{"127.0.0.1:1"}}, time.Second)
	if err == nil || !strings.Contains(err.Error(), "127.0.0.1:1") || !strings.Contains(err.Error(), "nacos.addrs") {
		t.Fatalf("an unreachable server must name the address and the setting, got %v", err)
	}
	if strings.Contains(err.Error(), "dial tcp") {
		t.Errorf("keep the message short, without the dial boilerplate: %v", err)
	}
	if err := Probe(Config{}, time.Second); err == nil {
		t.Error("no addrs must be an error")
	}
}

func TestNewProbesFirst(t *testing.T) {
	_, _, made := fakeTarget(t)
	probeFunc = func(Config, time.Duration) error { return errors.New("cannot reach Nacos: x") }
	if _, err := New(Config{Addrs: []string{"h:1"}}); err == nil || *made != 0 {
		t.Fatalf("an unreachable Nacos must stop before the SDK client is created (err=%v made=%d)", err, *made)
	}
}

// resetOutage gives a test the state of a process that has just started.
func resetOutage(t *testing.T) {
	t.Helper()
	fresh := func() {
		outage.mu.Lock()
		outage.down, outage.suppressed, outage.unhealthy = false, 0, map[*Client]bool{}
		outage.mu.Unlock()
	}
	fresh()
	t.Cleanup(fresh)
}

func TestConnectionLostAndRestored(t *testing.T) {
	resetOutage(t)
	logs := zlogtest.Capture(t)
	naming := newFakeNaming()
	c := NewFrom(Config{Addrs: []string{"n1:8848"}}, naming, nil)
	go c.watchConnection(time.Millisecond)
	defer c.Close()

	naming.healthy.Store(false)
	waitFor(t, func() bool { return logs.Has("WARN", "nacos connection lost") })
	naming.healthy.Store(true)
	waitFor(t, func() bool { return logs.Has("INFO", "nacos connection restored") })
	if n := len(logs.Records()); n != 2 {
		t.Errorf("one record per transition, not per look: %d\n%s", n, logs)
	}
}

// What the SDK has to say about an outage is one record of ours when it
// begins and a count when it is over.
func TestOutageIsNotAFloodOfSDKRecords(t *testing.T) {
	resetOutage(t)
	logs := zlogtest.Capture(t)
	sdklogger.SetLogger(&sdkLogger{l: zlog.With(zlog.Str("logger", "nacos-sdk")), min: zapcore.WarnLevel})
	naming := newFakeNaming()
	c := NewFrom(Config{}, naming, nil)

	sdklogger.Warn("something else") // not an outage: written
	naming.healthy.Store(false)
	sdklogger.Warnf("x fail to connect server, after trying 1 times, error=%s", "connection refused")
	for i := 0; i < 50; i++ {
		sdklogger.Errorf("Send request fail, request=ConfigBatchListenRequest, retryTimes=%d", i)
	}
	outage.report(c, false, 0)
	outage.report(c, true, 0)
	sdklogger.Warn("after the outage") // written again

	recs := logs.Records()
	if len(recs) != 4 {
		t.Fatalf("want 4 records, got %d:\n%s", len(recs), logs)
	}
	if r := recs[1]; r.Msg[:21] != "nacos connection lost" || !strings.Contains(r.Fields["cause"].(string), "connection refused") {
		t.Errorf("the outage begins with the SDK's first complaint as the cause: %+v", r)
	}
	if r := recs[2]; r.Msg[:25] != "nacos connection restored" || r.Fields["sdk_records_hidden"] != float64(51) {
		t.Errorf("the outage ends with the count of what was left out: %+v", r)
	}

	// At debug the SDK's own account is wanted, all of it.
	sdklogger.SetLogger(&sdkLogger{l: zlog.With(), min: zapcore.DebugLevel})
	sdklogger.Warn("y fail to connect server")
	if !logs.Has("WARN", "y fail to connect server") || outage.down {
		t.Errorf("sdk_log_level debug hides nothing: %s", logs)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(2 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatal("timed out waiting")
}
