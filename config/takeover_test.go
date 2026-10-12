package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func resetTakeover() { takeoverOnce = sync.Once{}; takeover = nil; takeoverOff.Store(false) }

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
	writeMarker(t, root, TakeoverMarker{Service: filepath.Base(root), Tag: "alice", Env: "dev", PID: 1, Heartbeat: time.Now(),
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
	if got := RewriteURL("http://mysql.sportsbook-dev-infra:3306/x?y=1"); got != "http://127.0.0.1:13306/x?y=1" {
		t.Errorf("url: %s", got)
	}
	if got := RewriteURL("http://u:p@mysql.sportsbook-dev-infra:3306"); got != "http://u:p@127.0.0.1:13306" {
		t.Errorf("url with userinfo: %s", got)
	}
	if got := RewriteAddr("other:1"); got != "other:1" {
		t.Errorf("unknown address changed: %s", got)
	}
}

func TestTakeoverIgnoredWhenStaleDisabledOrDeployed(t *testing.T) {
	root := t.TempDir()
	t.Setenv(DirEnv, filepath.Join(root, "conf"))
	t.Setenv(EnvVar, "")
	writeMarker(t, root, TakeoverMarker{Service: filepath.Base(root), Env: "dev", Heartbeat: time.Now().Add(-time.Minute)})
	resetTakeover()
	if Takeover() != nil {
		t.Error("stale marker accepted")
	}
	writeMarker(t, root, TakeoverMarker{Service: filepath.Base(root), Env: "dev", Heartbeat: time.Now()})
	resetTakeover()
	DisableTakeover()
	if Takeover() != nil {
		t.Error("disabled but accepted")
	}
	resetTakeover()
	if Takeover() == nil {
		t.Fatal("precondition: fresh marker read")
	}
	DisableTakeover() // after the fact: still nothing
	if Takeover() != nil {
		t.Error("DisableTakeover after the first read did not take")
	}
	resetTakeover()
	writeMarker(t, root, TakeoverMarker{Service: "another-service", Env: "dev", Heartbeat: time.Now()})
	if Takeover() != nil {
		t.Error("a marker for another service was accepted")
	}
	writeMarker(t, root, TakeoverMarker{Service: filepath.Base(root), Env: "dev", Heartbeat: time.Now()})
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

// APP_ENV=local (what make run sets) still enters takeover mode, and entering
// it leaves the acknowledgement the tool waits for; any other APP_ENV does not.
func TestTakeoverUnderLocalWritesAck(t *testing.T) {
	dir := t.TempDir()
	svc := filepath.Base(dir)
	confDir := filepath.Join(dir, "conf")
	if err := os.MkdirAll(filepath.Join(dir, ".devkit"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(confDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(DirEnv, confDir)
	marker, _ := json.Marshal(TakeoverMarker{Service: svc, Tag: "zoe", Env: "dev", Heartbeat: time.Now()})
	if err := os.WriteFile(filepath.Join(dir, TakeoverFile), marker, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvVar, "dev")
	resetTakeover()
	if Takeover() != nil {
		t.Fatal("APP_ENV=dev entered takeover mode")
	}
	t.Setenv(EnvVar, "local")
	resetTakeover()
	if Takeover() == nil {
		t.Fatal("APP_ENV=local did not enter takeover mode")
	}
	b, err := os.ReadFile(filepath.Join(dir, TakeoverAckFile))
	if err != nil {
		t.Fatalf("no ack: %v", err)
	}
	var ack TakeoverAck
	if json.Unmarshal(b, &ack) != nil || ack.Service != svc || ack.Tag != "zoe" || ack.PID != os.Getpid() {
		t.Fatalf("ack: %s", b)
	}
}
