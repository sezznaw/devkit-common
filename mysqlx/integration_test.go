package mysqlx

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

func testResolveAndOpen(t *testing.T, rootDSN string) {
	ctx := context.Background()
	admin, err := sql.Open("mysql", rootDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	for _, q := range []string{
		"DROP DATABASE IF EXISTS mysqlx_platform", "DROP DATABASE IF EXISTS mysqlx_tenant",
		"CREATE DATABASE mysqlx_platform", "CREATE DATABASE mysqlx_tenant",
		`CREATE TABLE mysqlx_platform.datasource (
			tenant_code VARCHAR(32), role VARCHAR(16), kind VARCHAR(16), host VARCHAR(255), port INT,
			db_name VARCHAR(64), username VARCHAR(64), password_env VARCHAR(64), params VARCHAR(255))`,
		"INSERT INTO mysqlx_platform.datasource VALUES ('tenant_a','tenant','mysql','127.0.0.1',13306,'mysqlx_tenant','root','MYSQLX_TEST_PW','')",
		"INSERT INTO mysqlx_platform.datasource VALUES ('platform','platform','mysql','127.0.0.1',13306,'mysqlx_platform','root','MYSQLX_TEST_PW','')",
		"INSERT INTO mysqlx_platform.datasource VALUES ('tenant_a','tenant','valkey','valkey',6379,'2','','MYSQLX_TEST_PW','')",
		"CREATE TABLE mysqlx_tenant.t (id INT PRIMARY KEY)", "INSERT INTO mysqlx_tenant.t VALUES (1)",
	} {
		if _, err := admin.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	defer admin.Exec("DROP DATABASE IF EXISTS mysqlx_platform")
	defer admin.Exec("DROP DATABASE IF EXISTS mysqlx_tenant")

	// root DSN "root:root@tcp(127.0.0.1:13306)/" -> platform target
	userPass := strings.SplitN(strings.SplitN(rootDSN, "@", 2)[0], ":", 2)
	platform := Target{Addr: "127.0.0.1:13306", DB: "mysqlx_platform", User: userPass[0], Password: userPass[1]}
	env := map[string]string{"MYSQLX_TEST_PW": userPass[1]}
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
	if tgt.DB != "mysqlx_tenant" || tgt.Addr != "127.0.0.1:13306" {
		t.Errorf("resolved %+v", tgt)
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
