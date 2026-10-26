package mysqlx

import (
	"context"
	"database/sql"
	"net"
	"strconv"
	"strings"
	"testing"

	gomysql "github.com/go-sql-driver/mysql"
)

// testResolveAndOpen runs against the server rootDSN names, wherever it
// listens and with whatever account: the datasource rows it writes point
// back at that server. (It assumed 127.0.0.1:13306 and the account root, the
// CI job's container, and failed against any other, the local stack's 3306
// among them.)
func testResolveAndOpen(t *testing.T, rootDSN string) {
	ctx := context.Background()
	root, err := gomysql.ParseDSN(rootDSN)
	if err != nil {
		t.Fatalf("MYSQL_TEST_DSN_ROOT: %v", err)
	}
	host, port, err := net.SplitHostPort(root.Addr)
	if root.Net != "tcp" || err != nil {
		t.Skipf("MYSQL_TEST_DSN_ROOT names %s(%s): the datasource table holds a host and a port, the test needs a TCP address", root.Net, root.Addr)
	}
	portNo, err := strconv.Atoi(port)
	if err != nil {
		t.Fatalf("MYSQL_TEST_DSN_ROOT port %q: %v", port, err)
	}
	admin, err := sql.Open("mysql", rootDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	mysqlRow := "INSERT INTO mysqlx_platform.datasource VALUES (?, ?, 'mysql', ?, ?, ?, ?, 'MYSQLX_TEST_PW', '')"
	for _, s := range [][]any{
		{"DROP DATABASE IF EXISTS mysqlx_platform"}, {"DROP DATABASE IF EXISTS mysqlx_tenant"},
		{"CREATE DATABASE mysqlx_platform"}, {"CREATE DATABASE mysqlx_tenant"},
		{`CREATE TABLE mysqlx_platform.datasource (
			tenant_code VARCHAR(32), role VARCHAR(16), kind VARCHAR(16), host VARCHAR(255), port INT,
			db_name VARCHAR(64), username VARCHAR(64), password_env VARCHAR(64), params VARCHAR(255))`},
		{mysqlRow, "tenant_a", "tenant", host, portNo, "mysqlx_tenant", root.User},
		{mysqlRow, "platform", "platform", host, portNo, "mysqlx_platform", root.User},
		{"INSERT INTO mysqlx_platform.datasource VALUES ('tenant_a','tenant','valkey','valkey',6379,'2','','MYSQLX_TEST_PW','')"},
		{"CREATE TABLE mysqlx_tenant.t (id INT PRIMARY KEY)"}, {"INSERT INTO mysqlx_tenant.t VALUES (1)"},
	} {
		if _, err := admin.ExecContext(ctx, s[0].(string), s[1:]...); err != nil {
			t.Fatalf("%s: %v", s[0], err)
		}
	}
	defer admin.Exec("DROP DATABASE IF EXISTS mysqlx_platform")
	defer admin.Exec("DROP DATABASE IF EXISTS mysqlx_tenant")

	// the root DSN's server and account -> the platform target
	platform := Target{Addr: root.Addr, DB: "mysqlx_platform", User: root.User, Password: root.Passwd}
	env := map[string]string{"MYSQLX_TEST_PW": root.Passwd}
	getenv := func(k string) string { return env[k] }

	if _, err := Resolve(ctx, platform, "", RoleTenant, getenv); err == nil || !strings.Contains(err.Error(), "TENANT_CODE") {
		t.Errorf("no tenant must be named: %v", err)
	}
	if _, err := Resolve(ctx, platform, "nobody", RoleTenant, getenv); err == nil || !strings.Contains(err.Error(), `"nobody"`) {
		t.Errorf("an unknown tenant must be named: %v", err)
	}
	if _, err := Resolve(ctx, platform, "tenant_a", RoleTenant, func(string) string { return "" }); err == nil || !strings.Contains(err.Error(), "MYSQLX_TEST_PW") {
		t.Errorf("an unset password variable must be named: %v", err)
	}
	tgt, err := Resolve(ctx, platform, "tenant_a", RoleTenant, getenv)
	if err != nil {
		t.Fatal(err)
	}
	if tgt.DB != "mysqlx_tenant" || tgt.Addr != root.Addr || tgt.User != root.User {
		t.Errorf("resolved %s/%s as %s, want %s/mysqlx_tenant as %s", tgt.Addr, tgt.DB, tgt.User, root.Addr, root.User)
	}
	ptgt, err := Resolve(ctx, platform, "whatever", RolePlatform, getenv)
	if err != nil || ptgt.DB != "mysqlx_platform" {
		t.Errorf("role platform resolves the platform db: %+v %v", ptgt, err)
	}

	vk, err := ResolveKind(ctx, platform, "tenant_a", RoleTenant, "valkey", getenv)
	if err != nil || vk.Addr != "valkey:6379" || vk.DB != "2" {
		t.Errorf("valkey row: %+v %v", vk, err)
	}

	db, err := Open(ctx, tgt, Config{Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	defer Close(db)
	var n int
	if err := db.WithContext(ctx).Raw("SELECT COUNT(*) FROM t").Scan(&n).Error; err != nil || n != 1 {
		t.Errorf("query through gorm: n=%d err=%v", n, err)
	}
	if _, err := Open(ctx, Target{Addr: "127.0.0.1:1", DB: "x", User: "x"}, Config{}); err == nil || !strings.Contains(err.Error(), "is the database up") {
		t.Errorf("a database that is not there must fail with the hint: %v", err)
	}
}
