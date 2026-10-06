package mysqlx

import (
	"context"
	"os"
	"strings"
	"testing"
)

func TestConfigDefaultsAndValidation(t *testing.T) {
	var c Config
	if c.Validate() != nil {
		t.Fatal("disabled is always valid")
	}
	c.Enabled = true
	if c.SourceName() != SourceStatic || c.RoleName() != RoleTenant {
		t.Errorf("defaults: %s %s", c.SourceName(), c.RoleName())
	}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "mysql.db") {
		t.Errorf("static without db must name mysql.db: %v", err)
	}
	c.DB, c.User = "tenant_a", "tenant_a"
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := c.Static().Addr; got != defaultAddr {
		t.Errorf("empty addr defaults to %s, got %s", defaultAddr, got)
	}
	c.Source, c.Role = "platform", "owner"
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "mysql.role") {
		t.Errorf("a bad role must be named: %v", err)
	}
	c.Source = "cloud"
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "mysql.source") {
		t.Errorf("a bad source must be named: %v", err)
	}
}

func TestDSN(t *testing.T) {
	dsn := Target{Addr: "db:3306", DB: "tenant_a", User: "u", Password: "p@ss", Params: "a=1"}.DSN()
	for _, want := range []string{"u:p@ss@tcp(db:3306)/tenant_a?", "parseTime=true", "loc=UTC", "charset=utf8mb4", "&a=1"} {
		if !strings.Contains(dsn, want) {
			t.Errorf("DSN %q lacks %q", dsn, want)
		}
	}
}

func TestOpenRefusesAnIncompleteTarget(t *testing.T) {
	if _, err := Open(context.Background(), Target{Addr: "x"}, Config{}); err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Errorf("got %v", err)
	}
}

// TestAgainstMySQL needs a real server: MYSQL_TEST_DSN_ROOT like
// "root:root@tcp(127.0.0.1:13306)/". It creates its own schema and drops it.
func TestAgainstMySQL(t *testing.T) {
	root := os.Getenv("MYSQL_TEST_DSN_ROOT")
	if root == "" {
		t.Skip("MYSQL_TEST_DSN_ROOT not set")
	}
	t.Run("resolve and open", func(t *testing.T) { testResolveAndOpen(t, root) })
}
