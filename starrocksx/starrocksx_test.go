package starrocksx

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"

	gomysql "github.com/go-sql-driver/mysql"

	"github.com/sezznaw/devkit-common/zlog/zlogtest"
)

func TestPrepare(t *testing.T) {
	c := &Client{tenant: "tenant_a"}
	q, args, err := c.prepare("SELECT uid FROM member WHERE tenant_id = :tenant AND status = ? AND note = ':tenant?' AND created_at > ?", []any{1, "x"})
	if err != nil {
		t.Fatal(err)
	}
	if q != "SELECT uid FROM member WHERE tenant_id = ? AND status = ? AND note = ':tenant?' AND created_at > ?" {
		t.Fatalf("%q", q)
	}
	if len(args) != 3 || args[0] != "tenant_a" || args[1] != 1 || args[2] != "x" {
		t.Fatalf("%v", args)
	}
	// :tenant may appear twice, anywhere
	q, args, _ = c.prepare("WITH a AS (SELECT 1 FROM t WHERE tenant_id = :tenant) SELECT * FROM a, b WHERE b.tenant_id = :tenant AND b.x = ?", []any{9})
	if len(args) != 3 || args[0] != "tenant_a" || args[1] != "tenant_a" || args[2] != 9 {
		t.Fatalf("%q %v", q, args)
	}
	if _, _, err := c.prepare("SELECT 1 FROM member WHERE status = ?", []any{1}); !errors.Is(err, ErrNoTenant) {
		t.Fatalf("no tenant: %v", err)
	}
	if _, _, err := c.prepare("DELETE FROM member WHERE tenant_id = :tenant", nil); !errors.Is(err, ErrNotReadOnly) {
		t.Fatalf("delete: %v", err)
	}
	if _, _, err := c.prepare("-- comment\n  select 1 where tenant_id = :tenant", nil); err != nil {
		t.Fatalf("comment then select: %v", err)
	}
	if _, _, err := c.prepare("SELECT ? FROM t WHERE tenant_id = :tenant", nil); err == nil {
		t.Fatal("arg count")
	}
	if snake("CreatedAt") != "created_at" || snake("UID") != "uid" || snake("N") != "n" {
		t.Fatal(snake("CreatedAt"), snake("UID"))
	}
}

// TestAgainstMySQL uses a MySQL as the stand-in for StarRocks (same protocol):
// Query scans structs and scalars, QueryRow one row, the row cap applies.
func TestAgainstMySQL(t *testing.T) {
	root := os.Getenv("MYSQL_TEST_DSN_ROOT")
	if root == "" {
		t.Skip("MYSQL_TEST_DSN_ROOT not set")
	}
	zlogtest.Discard(t)
	admin, err := sql.Open("mysql", root)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	for _, q := range []string{"DROP DATABASE IF EXISTS starrocksx_test", "CREATE DATABASE starrocksx_test",
		"CREATE TABLE starrocksx_test.member (tenant_id VARCHAR(32), uid BIGINT, username VARCHAR(64), status INT, created_at DATETIME(3))",
		"INSERT INTO starrocksx_test.member VALUES ('tenant_a',1,'alice',1,NOW(3)),('tenant_a',2,'bob',0,NOW(3)),('tenant_b',3,'eve',1,NOW(3))"} {
		if _, err := admin.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	defer admin.Exec("DROP DATABASE IF EXISTS starrocksx_test")
	dsn, _ := gomysql.ParseDSN(root)
	c, err := Open(context.Background(), Target{Addr: dsn.Addr, DB: "starrocksx_test", User: dsn.User, Password: dsn.Passwd}, Config{MaxRows: 2}, "tenant_a")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx := context.Background()
	var byStatus []struct {
		Status int   `db:"status"`
		N      int64 `db:"n"`
	}
	if err := c.Query(ctx, &byStatus, "SELECT status, COUNT(*) AS n FROM member WHERE tenant_id = :tenant GROUP BY status ORDER BY status"); err != nil {
		t.Fatal(err)
	}
	if len(byStatus) != 2 || byStatus[0].Status != 0 || byStatus[0].N != 1 || byStatus[1].N != 1 {
		t.Fatalf("%+v", byStatus)
	}
	var names []string
	if err := c.Query(ctx, &names, "SELECT username FROM member WHERE tenant_id = :tenant AND status = ? ORDER BY uid", 1); err != nil || len(names) != 1 || names[0] != "alice" {
		t.Fatalf("%v %v", names, err)
	}
	var total int64
	if err := c.QueryRow(ctx, &total, "SELECT COUNT(*) FROM member WHERE tenant_id = :tenant"); err != nil || total != 2 {
		t.Fatalf("total=%d %v (tenant_b must not count)", total, err)
	}
	var one struct {
		UID      int64
		Username string
	}
	if err := c.QueryRow(ctx, &one, "SELECT uid, username FROM member WHERE tenant_id = :tenant AND uid = ?", 2); err != nil || one.Username != "bob" {
		t.Fatalf("%+v %v", one, err)
	}
	if err := c.QueryRow(ctx, &one, "SELECT uid, username FROM member WHERE tenant_id = :tenant AND uid = ?", 99); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("no row: %v", err)
	}
	var all []struct{ UID int64 }
	admin.Exec("INSERT INTO starrocksx_test.member VALUES ('tenant_a',4,'carl',1,NOW(3))")
	if err := c.Query(ctx, &all, "SELECT uid FROM member WHERE tenant_id = :tenant"); !errors.Is(err, ErrTooManyRows) {
		t.Fatalf("row cap: %v", err)
	}
	var bad []struct{ Nope int }
	if err := c.Query(ctx, &bad, "SELECT uid FROM member WHERE tenant_id = :tenant"); err == nil {
		t.Fatal("unmapped column accepted")
	}
}
