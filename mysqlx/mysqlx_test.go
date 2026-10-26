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

// TestAgainstMySQL needs a real server: MYSQL_TEST_DSN_ROOT, the root DSN
// testx reads too, on any TCP address ("root:root@tcp(127.0.0.1:13306)/" in
// CI, the local stack's 3306 on a laptop); without it the test is skipped.
// It creates its own schema and drops it.
func TestAgainstMySQL(t *testing.T) {
	root := os.Getenv("MYSQL_TEST_DSN_ROOT")
	if root == "" {
		t.Skip("MYSQL_TEST_DSN_ROOT not set")
	}
	t.Run("resolve and open", func(t *testing.T) { testResolveAndOpen(t, root) })
}

func TestAutoMigrateNeverOnInTakeover(t *testing.T) {
	t.Setenv("APP_ENV", "dev")
	on := true
	if !(Config{}).AutoMigrateOn() || !(Config{AutoMigrate: &on}).AutoMigrateOn() {
		t.Fatal("dev without takeover migrates")
	}
	was := inTakeover
	t.Cleanup(func() { inTakeover = was })
	inTakeover = func() bool { return true }
	if (Config{}).AutoMigrateOn() {
		t.Error("takeover mode must not migrate the shared database")
	}
	if (Config{AutoMigrate: &on}).AutoMigrateOn() {
		t.Error("auto_migrate: true must not override takeover mode")
	}
}

// source service: the domain comes from the service name, the database and
// account are <tenant>_<domain>, bad names are refused before dialling.
func TestServiceSource(t *testing.T) {
	for svc, want := range map[string]string{"ser-member": "member", "ser-finance-admin": "finance_admin", "geo": "geo"} {
		if got := (Config{}).DomainName(svc); got != want {
			t.Fatalf("%s: domain %q", svc, got)
		}
	}
	if got := (Config{Domain: "wallet_v2"}).DomainName("ser-wallet"); got != "wallet_v2" {
		t.Fatalf("explicit domain: %q", got)
	}
	if got := (Config{}).PasswordEnvName(); got != DefaultServicePasswordEnv {
		t.Fatalf("password env: %q", got)
	}
	if name, err := ServiceDatabase("tenant_a", "geo"); err != nil || name != "tenant_a_geo" {
		t.Fatalf("name: %q %v", name, err)
	}
	if _, err := ServiceDatabase("", "geo"); err == nil {
		t.Fatal("empty tenant accepted")
	}
	if _, err := ServiceDatabase("tenant_a", "Geo-X"); err == nil {
		t.Fatal("bad domain accepted")
	}
	if _, err := ServiceDatabase("tenant_a", "a_very_long_domain_name_over_limit"); err == nil {
		t.Fatal("33+ characters accepted")
	}
	if err := (Config{Enabled: true, Source: SourceService}).Validate(); err != nil {
		t.Fatalf("service: %v", err)
	}
	if err := (Config{Enabled: true, Source: SourceService, Role: RolePlatform}).Validate(); err == nil {
		t.Fatal("service + role platform accepted")
	}
	if err := (Config{Enabled: true, Source: SourceService, Domain: "Bad Name"}).Validate(); err == nil {
		t.Fatal("bad domain accepted by Validate")
	}
}
