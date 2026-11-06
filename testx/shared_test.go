package testx

import (
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"testing"

	"gorm.io/gorm"
)

type sharedRow struct {
	ID   int64  `gorm:"column:id;type:bigint;primaryKey;autoIncrement"`
	Name string `gorm:"column:name;type:varchar(32);not null"`
}

func (sharedRow) TableName() string { return "shared_row" }

func currentDB(t *testing.T, db *gorm.DB) string {
	t.Helper()
	var name string
	if err := db.Raw("SELECT DATABASE()").Scan(&name).Error; err != nil {
		t.Fatal(err)
	}
	return name
}

func countRows(t *testing.T, db *gorm.DB) int64 {
	t.Helper()
	var n int64
	if err := db.Model(&sharedRow{}).Count(&n).Error; err != nil {
		t.Fatal(err)
	}
	return n
}

func insertRow(t *testing.T, db *gorm.DB, name string) int64 {
	t.Helper()
	r := sharedRow{Name: name}
	if err := db.Create(&r).Error; err != nil {
		t.Fatal(err)
	}
	return r.ID
}

// TestRuntimeMySQLSharedDatabase: the tests of a process take turns on one database per
// service, and each finds it as a new one would be: no rows, ids from 1, a
// table an earlier test dropped back in place. A second Runtime while the
// first is alive gets a database of its own.
func TestRuntimeMySQLSharedDatabase(t *testing.T) {
	if os.Getenv(DSNEnv) == "" {
		t.Skip(DSNEnv + " not set")
	}
	opts := []Option{WithMySQL(""), WithModels(sharedRow{})}
	var first string
	t.Run("first test writes rows", func(t *testing.T) {
		rt := New(t, "shared-test", opts...)
		first = currentDB(t, rt.DB)
		for i := 1; i <= 3; i++ {
			if id := insertRow(t, rt.DB, "a"); id != int64(i) {
				t.Fatalf("id %d, want %d", id, i)
			}
		}
		// a row in a framework table too
		if err := rt.DB.Exec("INSERT INTO outbox (event_id, topic, event_type, payload) VALUES (UUID(), 'events.x', 'x.done', '{}')").Error; err != nil {
			t.Fatal(err)
		}
	})
	if first == "" {
		t.Skip("no MySQL answered")
	}
	t.Run("next test gets it empty", func(t *testing.T) {
		rt := New(t, "shared-test", opts...)
		if got := currentDB(t, rt.DB); got != first {
			t.Fatalf("database %s, want the shared %s", got, first)
		}
		if n := countRows(t, rt.DB); n != 0 {
			t.Fatalf("%d rows left from the previous test", n)
		}
		var outbox int64
		if err := rt.DB.Raw("SELECT COUNT(*) FROM outbox").Scan(&outbox).Error; err != nil || outbox != 0 {
			t.Fatalf("outbox rows %d, err %v", outbox, err)
		}
		if id := insertRow(t, rt.DB, "b"); id != 1 {
			t.Fatalf("AUTO_INCREMENT not reset: id %d", id)
		}
	})
	t.Run("a second runtime in the same test gets its own database", func(t *testing.T) {
		a := New(t, "shared-test", opts...)
		b := New(t, "shared-test", opts...)
		if currentDB(t, a.DB) == currentDB(t, b.DB) {
			t.Fatal("two live runtimes share a database")
		}
		insertRow(t, a.DB, "a")
		if n := countRows(t, b.DB); n != 0 {
			t.Fatalf("b sees %d rows of a", n)
		}
	})
	t.Run("a test drops a table", func(t *testing.T) {
		rt := New(t, "shared-test", opts...)
		if err := rt.DB.Exec("DROP TABLE shared_row").Error; err != nil {
			t.Fatal(err)
		}
	})
	t.Run("the next one has it again", func(t *testing.T) {
		rt := New(t, "shared-test", opts...)
		if id := insertRow(t, rt.DB, "c"); id != 1 {
			t.Fatalf("id %d", id)
		}
	})
	t.Run("a test makes writes fail with a trigger, a view and a procedure", func(t *testing.T) {
		rt := New(t, "shared-test", opts...)
		for _, ddl := range []string{
			"CREATE TRIGGER shared_fail BEFORE INSERT ON shared_row FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'boom'",
			"CREATE VIEW shared_view AS SELECT id FROM shared_row",
			"CREATE PROCEDURE shared_proc() SELECT 1",
		} {
			if err := rt.DB.Exec(ddl).Error; err != nil {
				t.Fatal(err)
			}
		}
	})
	t.Run("the next one does not inherit them", func(t *testing.T) {
		rt := New(t, "shared-test", opts...)
		if id := insertRow(t, rt.DB, "e"); id != 1 {
			t.Fatalf("id %d", id)
		}
		var n int64
		if err := rt.DB.Raw("SELECT (SELECT COUNT(*) FROM information_schema.VIEWS WHERE TABLE_SCHEMA = DATABASE()) + (SELECT COUNT(*) FROM information_schema.ROUTINES WHERE ROUTINE_SCHEMA = DATABASE())").Scan(&n).Error; err != nil || n != 0 {
			t.Fatalf("%d views / routines left, err %v", n, err)
		}
	})
	t.Run("a test widens a column past the model", func(t *testing.T) {
		rt := New(t, "shared-test", opts...)
		if err := rt.DB.Exec("ALTER TABLE shared_row MODIFY name varchar(64) NOT NULL").Error; err != nil {
			t.Fatal(err)
		}
	})
	t.Run("the next one gets the model's column, not an AutoMigrate refusal", func(t *testing.T) {
		// on the altered table AutoMigrate would refuse to narrow varchar(64) back to varchar(32)
		rt := New(t, "shared-test", opts...)
		var typ string
		if err := rt.DB.Raw("SELECT COLUMN_TYPE FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'shared_row' AND COLUMN_NAME = 'name'").Scan(&typ).Error; err != nil {
			t.Fatal(err)
		}
		if typ != "varchar(32)" {
			t.Fatalf("name is %s", typ)
		}
	})
	t.Run("WithFreshDatabase gets a database of its own, dropped after", func(t *testing.T) {
		var fresh string
		t.Run("fresh", func(t *testing.T) {
			rt := New(t, "shared-test", WithFreshDatabase(), WithModels(sharedRow{}))
			if fresh = currentDB(t, rt.DB); fresh == first {
				t.Fatal("WithFreshDatabase got the shared database")
			}
		})
		rt := New(t, "shared-test", opts...)
		var n int
		if err := rt.DB.Raw("SELECT COUNT(*) FROM information_schema.SCHEMATA WHERE SCHEMA_NAME = ?", fresh).Scan(&n).Error; err != nil || n != 0 {
			t.Fatalf("%s still there (%d), err %v", fresh, n, err)
		}
	})
	t.Run("a table whose AUTO_INCREMENT was moved", func(t *testing.T) {
		rt := New(t, "shared-test", opts...)
		if err := rt.DB.Exec("ALTER TABLE shared_row AUTO_INCREMENT = 1000").Error; err != nil {
			t.Fatal(err)
		}
	})
	t.Run("starts from 1 again", func(t *testing.T) {
		rt := New(t, "shared-test", opts...)
		if id := insertRow(t, rt.DB, "d"); id != 1 {
			t.Fatalf("id %d", id)
		}
	})
}

// TestRuntimeMySQLSharedDatabaseAbandoned: a process cannot drop its database when it
// exits, so the next one drops those whose process is gone, and keeps one
// whose process still runs.
func TestRuntimeMySQLSharedDatabaseAbandoned(t *testing.T) {
	root := os.Getenv(DSNEnv)
	if root == "" {
		t.Skip(DSNEnv + " not set")
	}
	admin, err := sql.Open("mysql", root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() }) // registered first: runs after the DROP below
	if err := admin.Ping(); err != nil {
		t.Skip("no MySQL answered")
	}
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Skip("cannot start a process to get a finished pid")
	}
	dead := cmd.Process.Pid
	// another service's database counts too: any finished process of this host
	gone, kept := fmt.Sprintf("%s%d", sharedPrefix("abandon-test-a"), dead), fmt.Sprintf("%s%d", sharedPrefix("abandon-test-b"), os.Getppid())
	for _, name := range []string{gone, kept} {
		if _, err := admin.Exec("CREATE DATABASE IF NOT EXISTS `" + name + "`"); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _, _ = admin.Exec("DROP DATABASE IF EXISTS `" + kept + "`") })
	dropAbandoned(admin)
	exists := func(name string) bool {
		var n int
		_ = admin.QueryRow("SELECT COUNT(*) FROM information_schema.SCHEMATA WHERE SCHEMA_NAME = ?", name).Scan(&n)
		return n == 1
	}
	if exists(gone) {
		t.Fatalf("%s (process %d finished) was not dropped", gone, dead)
	}
	if !exists(kept) {
		t.Fatalf("%s (process %d running) was dropped", kept, os.Getppid())
	}
}
