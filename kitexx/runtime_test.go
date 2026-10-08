package kitexx

import (
	"bytes"
	"strings"
	"testing"

	"github.com/sezznaw/devkit-common/mysqlx"
	"github.com/sezznaw/devkit-common/redisx"
)

func TestRuntimeWithoutMySQL(t *testing.T) {
	var cfg Config
	cfg.Service.Name = "order"
	cfg.RegistryDisabled = true
	cfg.Log.Output = &bytes.Buffer{}
	rt, err := NewRuntime(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if rt.DB != nil {
		t.Error("mysql disabled: DB stays nil")
	}
	opts, err := rt.Options()
	if err != nil || len(opts) != 9 {
		t.Errorf("options: %d %v", len(opts), err)
	}
}

func TestRuntimeMySQLThatIsNotThereFailsTheStart(t *testing.T) {
	var cfg Config
	cfg.Service.Name = "order"
	cfg.RegistryDisabled = true
	cfg.Log.Output = &bytes.Buffer{}
	cfg.MySQL = mysqlx.Config{Enabled: true, Addr: "127.0.0.1:1", DB: "tenant_a", User: "tenant_a"}
	_, err := NewRuntime(cfg)
	if err == nil || !strings.Contains(err.Error(), "127.0.0.1:1") {
		t.Fatalf("the start must fail and name the database: %v", err)
	}
	cfg.MySQL = mysqlx.Config{Enabled: true, Source: "platform", Role: "boss"}
	if _, err := NewRuntime(cfg); err == nil || !strings.Contains(err.Error(), "mysql.role") {
		t.Fatalf("a bad role is refused before anything is dialled: %v", err)
	}
}

func TestRuntimeRedisThatIsNotThereFailsTheStart(t *testing.T) {
	var cfg Config
	cfg.Service.Name = "order"
	cfg.RegistryDisabled = true
	cfg.Log.Output = &bytes.Buffer{}
	cfg.Redis = redisx.Config{Enabled: true, Addr: "127.0.0.1:1", DialTimeout: 1}
	_, err := NewRuntime(cfg)
	if err == nil || !strings.Contains(err.Error(), "127.0.0.1:1") {
		t.Fatalf("the start must fail and name the server: %v", err)
	}
	rt, err := NewRuntime(Config{Service: cfg.Service, RegistryDisabled: true, Log: cfg.Log})
	if err != nil || rt.Redis != nil {
		t.Fatalf("redis disabled: Redis stays nil: %v", err)
	}
}
