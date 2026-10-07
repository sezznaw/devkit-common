package kitexx

import (
	"strings"
	"testing"

	"github.com/sezznaw/devkit-common/httpx"
	"github.com/sezznaw/devkit-common/zlog/zlogtest"
)

func TestProvidersNeedTheProviderNamespace(t *testing.T) {
	zlogtest.Capture(t)
	cfg := Config{Providers: map[string]httpx.Provider{"odds": {BaseURL: "https://api.vendor.com"}}}
	cfg.Service.Name = "ser-odds"
	cfg.RegistryDisabled = true
	t.Setenv(PodNamespaceEnv, "sportsbook-dev")
	if _, err := NewRuntime(cfg); err == nil || !strings.Contains(err.Error(), "sportsbook-dev-provider") {
		t.Errorf("business namespace: %v", err)
	}
	t.Setenv(PodNamespaceEnv, "sportsbook-dev-provider")
	rt, err := NewRuntime(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if rt.Provider("odds").Name != "odds" {
		t.Error("the client is there")
	}
	defer func() {
		if r := recover(); r == nil || !strings.Contains(r.(string), `no provider "pay"`) {
			t.Errorf("unknown provider: %v", r)
		}
	}()
	rt.Provider("pay")
}
