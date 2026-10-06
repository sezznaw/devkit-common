package redisx

import (
	"context"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
)

func TestConfigAndTarget(t *testing.T) {
	var c Config
	if c.Validate() != nil {
		t.Fatal("disabled is always valid")
	}
	c.Enabled = true
	if c.Static().Addr != defaultAddr {
		t.Errorf("empty addr defaults to %s", defaultAddr)
	}
	c.Source = "platform"
	c.Role = "boss"
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "redis.role") {
		t.Errorf("a bad role must be named: %v", err)
	}
	if tg, err := TargetFromRow("valkey:6379", "3", "pw"); err != nil || tg.DB != 3 || tg.Password != "pw" {
		t.Errorf("row -> target: %+v %v", tg, err)
	}
	if _, err := TargetFromRow("valkey:6379", "cache", "pw"); err == nil {
		t.Error("a db_name that is not an index must be refused")
	}
}

func TestOpenAgainstMiniredis(t *testing.T) {
	srv := miniredis.RunT(t)
	srv.RequireAuth("secret")
	cli, err := Open(context.Background(), Target{Addr: srv.Addr(), Password: "secret", DB: 1}, Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	if err := cli.Set(context.Background(), "k", "v", 0).Err(); err != nil {
		t.Fatal(err)
	}
	if got, _ := cli.Get(context.Background(), "k").Result(); got != "v" {
		t.Errorf("got %q", got)
	}
	if _, err := Open(context.Background(), Target{Addr: srv.Addr(), Password: "wrong"}, Config{}); err == nil || !strings.Contains(err.Error(), "password") {
		t.Errorf("a wrong password must fail with the hint: %v", err)
	}
}
