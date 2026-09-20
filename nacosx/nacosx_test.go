package nacosx

import (
	"errors"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nacos-group/nacos-sdk-go/v2/clients/naming_client"
)

func TestSharedNamingClientIsCreatedOncePerTarget(t *testing.T) {
	calls := 0
	orig, origProbe := newNamingFunc, probeFunc
	newNamingFunc = func(c Config) (naming_client.INamingClient, error) {
		calls++
		return &naming_client.NamingClient{}, nil
	}
	probeFunc = func(Config, time.Duration) error { return nil }
	defer func() {
		newNamingFunc, probeFunc = orig, origProbe
		sharedNaming = map[string]naming_client.INamingClient{}
	}()

	a := Config{Addrs: []string{"n1:8848"}, Namespace: "dev"}
	c1, _ := SharedNamingClient(a)
	c2, _ := SharedNamingClient(a)
	if c1 != c2 || calls != 1 {
		t.Fatalf("same target must share one client: calls=%d", calls)
	}
	if _, _ = SharedNamingClient(Config{Addrs: []string{"n1:8848"}, Namespace: "prod"}); calls != 2 {
		t.Fatalf("a different namespace needs its own client: calls=%d", calls)
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

func TestSharedNamingClientProbesFirst(t *testing.T) {
	created := false
	orig, origProbe := newNamingFunc, probeFunc
	newNamingFunc = func(Config) (naming_client.INamingClient, error) {
		created = true
		return &naming_client.NamingClient{}, nil
	}
	probeFunc = func(Config, time.Duration) error { return errors.New("cannot reach Nacos: x") }
	defer func() {
		newNamingFunc, probeFunc = orig, origProbe
		sharedNaming = map[string]naming_client.INamingClient{}
	}()
	if _, err := SharedNamingClient(Config{Addrs: []string{"h:1"}}); err == nil || created {
		t.Fatalf("an unreachable Nacos must stop before the SDK client is created (err=%v created=%v)", err, created)
	}
}
