package nacosx

import (
	"testing"

	"github.com/nacos-group/nacos-sdk-go/v2/clients/naming_client"
)

func TestSharedNamingClientIsCreatedOncePerTarget(t *testing.T) {
	calls := 0
	orig := newNamingFunc
	newNamingFunc = func(c Config) (naming_client.INamingClient, error) {
		calls++
		return &naming_client.NamingClient{}, nil
	}
	defer func() { newNamingFunc = orig; sharedNaming = map[string]naming_client.INamingClient{} }()

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
