package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func resetTakeover() { takeoverOnce = sync.Once{}; takeover = nil; takeoverOff = false }

func writeMarker(t *testing.T, root string, m TakeoverMarker) {
	t.Helper()
	os.MkdirAll(filepath.Join(root, ".devkit"), 0o755)
	os.MkdirAll(filepath.Join(root, "conf"), 0o755)
	os.WriteFile(filepath.Join(root, "conf", "dev.yaml"), []byte("name: dev\n"), 0o644)
	os.WriteFile(filepath.Join(root, "conf", "local.yaml"), []byte("name: local\n"), 0o644)
	b, _ := json.Marshal(m)
	os.WriteFile(filepath.Join(root, ".devkit", "takeover.json"), b, 0o644)
}

func TestTakeoverSwitchesProfileEnvAndAddresses(t *testing.T) {
	root := t.TempDir()
	t.Setenv(DirEnv, filepath.Join(root, "conf"))
	t.Setenv(EnvVar, "")
	t.Setenv("MYSQL_TENANT_A_PASSWORD", "")
	writeMarker(t, root, TakeoverMarker{Service: "ser-member", Tag: "alice", Env: "dev", PID: 1, Heartbeat: time.Now(),
		EnvVars: map[string]string{"MYSQL_TENANT_A_PASSWORD": "pw"},
		Rewrite: map[string]string{"mysql.sportsbook-dev-infra:3306": "127.0.0.1:13306", "redpanda-0.redpanda.ns.svc.cluster.local:9093": "127.0.0.1:19093"}})
	resetTakeover()
	defer resetTakeover()
	m := Takeover()
	if m == nil || m.Tag != "alice" {
		t.Fatalf("marker not picked up: %+v", m)
	}
	var out struct{ Name string }
	if err := LoadDefault(&out); err != nil || out.Name != "dev" {
		t.Errorf("profile: %v %+v", err, out)
	}
	if os.Getenv("MYSQL_TENANT_A_PASSWORD") != "pw" {
		t.Error("env var from marker not set")
	}
	if got := RewriteAddr("MySQL.sportsbook-dev-infra:3306"); got != "127.0.0.1:13306" {
		t.Errorf("rewrite: %s", got)
	}
	if got := RewriteAddr("redpanda-0.redpanda.ns.svc.cluster.local.:9093"); got != "127.0.0.1:19093" {
		t.Errorf("trailing dot: %s", got)
	}
	if got := RewriteURL("http://mysql.sportsbook-dev-infra:3306/x"); got != "http://127.0.0.1:13306/x" {
		t.Errorf("url: %s", got)
	}
	if got := RewriteAddr("other:1"); got != "other:1" {
		t.Errorf("unknown address changed: %s", got)
	}
}

func TestTakeoverIgnoredWhenStaleDisabledOrDeployed(t *testing.T) {
	root := t.TempDir()
	t.Setenv(DirEnv, filepath.Join(root, "conf"))
	t.Setenv(EnvVar, "")
	writeMarker(t, root, TakeoverMarker{Service: "s", Env: "dev", Heartbeat: time.Now().Add(-time.Minute)})
	resetTakeover()
	if Takeover() != nil {
		t.Error("stale marker accepted")
	}
	writeMarker(t, root, TakeoverMarker{Service: "s", Env: "dev", Heartbeat: time.Now()})
	resetTakeover()
	DisableTakeover()
	if Takeover() != nil {
		t.Error("disabled but accepted")
	}
	resetTakeover()
	t.Setenv(EnvVar, "dev")
	if Takeover() != nil {
		t.Error("deployment (APP_ENV set) looked at the marker")
	}
	resetTakeover()
	t.Setenv(EnvVar, "")
	var out struct{ Name string }
	if Takeover() == nil || LoadDefault(&out) != nil || out.Name != "dev" {
		t.Errorf("fresh marker: %+v %+v", Takeover(), out)
	}
	if RewriteAddr("x:1") != "x:1" {
		t.Error("empty table changed an address")
	}
}
