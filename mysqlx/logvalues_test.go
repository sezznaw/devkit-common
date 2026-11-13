package mysqlx

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	gomysql "github.com/go-sql-driver/mysql"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"

	"github.com/sezznaw/devkit-common/zlog/zlogtest"
)

// marker is in every value the statements below bind: it must never reach a
// log record. An invented string.
const marker = "INVENTED-VALUE"

type logRow struct {
	ID  int64
	Raw string
}

func (logRow) TableName() string { return "log_row" }

// dryDB is a GORM handle with the logger Open installs for cfg, on no
// database: the statements are built and logged, nothing is dialled.
func dryDB(t *testing.T, cfg Config, takeover bool) *gorm.DB {
	t.Helper()
	was := inTakeover
	inTakeover = func() bool { return takeover }
	t.Cleanup(func() { inTakeover = was; recorderValues.Store(false) })
	db, err := gorm.Open(mysql.New(mysql.Config{DSN: "u:p@tcp(127.0.0.1:1)/x", SkipInitializeWithVersion: true}),
		&gorm.Config{Logger: newLogger(cfg), DryRun: true, DisableAutomaticPing: true, SkipDefaultTransaction: true})
	if err != nil {
		t.Fatal(err)
	}
	return db
}

// everyShape runs one statement of every shape GORM logs differently, each
// binding a value that contains marker, and returns how many it ran. Raw /
// Model(...).Scan go through GORM's own recorder logger, the rest through
// the handle's.
func everyShape(db *gorm.DB) int {
	var rows []logRow
	var one logRow
	var n int64
	db.Create(&logRow{Raw: "create-" + marker})
	db.Model(&logRow{}).Where("id = ?", 7).Updates(map[string]any{"raw": "updates-" + marker})
	db.Where("raw IN ?", []string{"in1-" + marker, "in2-" + marker}).Find(&rows)
	db.Raw("SELECT COUNT(*) FROM log_row WHERE raw = ?", "rawscan-"+marker).Scan(&n)
	db.Model(&logRow{}).Select("raw").Where("raw = ?", "modelscan-"+marker).Scan(&rows)
	db.Exec("UPDATE log_row SET raw = ? WHERE id = ?", "exec-"+marker, 1)
	db.Model(&logRow{}).Where("raw = ?", "count-"+marker).Count(&n)
	db.Where("raw = ?", "first-"+marker).First(&one)
	db.Model(&logRow{}).Where("raw = ?", "pluck-"+marker).Pluck("raw", &[]string{})
	return 9
}

func sqlRecords(logs *zlogtest.Logs) (out []string) {
	for _, r := range logs.Records() {
		if s, ok := r.Fields["sql"].(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// TestLogCarriesNoValues: no SQL record carries a bound value, whichever way
// the statement was run; the statement keeps its placeholders.
func TestLogCarriesNoValues(t *testing.T) {
	t.Setenv("APP_ENV", "dev")
	logs := zlogtest.Capture(t)
	want := everyShape(dryDB(t, Config{}, false))
	got := sqlRecords(logs)
	if len(got) < want {
		t.Fatalf("%d SQL records for %d statements:\n%s", len(got), want, logs)
	}
	for _, s := range got {
		if strings.Contains(s, marker) {
			t.Errorf("a bound value is in the log: %s", s)
		}
		if !strings.Contains(s, "?") {
			t.Errorf("the statement lost its placeholders: %s", s)
		}
	}
}

// TestLogValuesOnlyLocal: mysql.log_values shows the values on a developer's
// machine and nowhere else; ignored, it says so.
func TestLogValuesOnlyLocal(t *testing.T) {
	const ignored = "mysql.log_values is ignored outside the local environment: bound values carry personal data and secrets"
	t.Run("local", func(t *testing.T) {
		t.Setenv("APP_ENV", "local")
		logs := zlogtest.Capture(t)
		want := everyShape(dryDB(t, Config{LogValues: true}, false))
		got := sqlRecords(logs)
		if len(got) < want {
			t.Fatalf("%d SQL records for %d statements", len(got), want)
		}
		for _, s := range got {
			if !strings.Contains(s, marker) {
				t.Errorf("log_values on a developer's machine: no value in %s", s)
			}
		}
		if logs.Has("WARN", ignored) {
			t.Error("log_values is honoured in local, yet reported as ignored")
		}
	})
	t.Run("dev", func(t *testing.T) {
		t.Setenv("APP_ENV", "dev")
		logs := zlogtest.Capture(t)
		everyShape(dryDB(t, Config{LogValues: true}, false))
		for _, s := range sqlRecords(logs) {
			if strings.Contains(s, marker) {
				t.Errorf("log_values outside local must be ignored: %s", s)
			}
		}
		if !logs.Has("WARN", ignored) {
			t.Errorf("an ignored log_values must be reported:\n%s", logs)
		}
	})
	t.Run("takeover", func(t *testing.T) {
		// a developer's machine on the shared dev database: its data is dev's
		t.Setenv("APP_ENV", "local")
		logs := zlogtest.Capture(t)
		everyShape(dryDB(t, Config{LogValues: true}, true))
		for _, s := range sqlRecords(logs) {
			if strings.Contains(s, marker) {
				t.Errorf("log_values in takeover mode must be ignored: %s", s)
			}
		}
		if !logs.Has("WARN", ignored) {
			t.Errorf("an ignored log_values must be reported:\n%s", logs)
		}
	})
}

// TestLogMasksValuesInMySQLErrors: MySQL quotes the offending value in its
// error text; the record of a failed statement carries the text without it,
// the error itself is not touched.
func TestLogMasksValuesInMySQLErrors(t *testing.T) {
	t.Setenv("APP_ENV", "dev")
	cases := []struct {
		number uint16
		msg    string
		want   string
	}{
		{1062, "Duplicate entry 'ord-" + marker + "' for key 'log_row.uk_raw'", "Duplicate entry '?' for key 'log_row.uk_raw'"},
		{1062, "Duplicate entry 'it's " + marker + "' for key 'x' for key 'log_row.uk_raw'", "Duplicate entry '?' for key 'log_row.uk_raw'"},
		{1366, "Incorrect string value: '\\xF0\\x9F " + marker + "' for column 'raw' at row 1", "Incorrect string value: '?' for column 'raw' at row 1"},
		{1366, "Incorrect integer value: '" + marker + "' for column `db`.`log_row`.`id` at row 1", "Incorrect integer value: '?' for column `db`.`log_row`.`id` at row 1"},
		{1292, "Truncated incorrect DOUBLE value: '" + marker + "'", "Truncated incorrect DOUBLE value: '?'"},
		{1292, "Incorrect datetime value: '" + marker + "' for column 'created_at' at row 1", "Incorrect datetime value: '?' for column 'created_at' at row 1"},
	}
	for _, c := range cases {
		logs := zlogtest.Capture(t)
		cause := &gomysql.MySQLError{Number: c.number, Message: c.msg}
		err := fmt.Errorf("insert: %w", cause)
		newLogger(Config{}).Trace(context.Background(), time.Now(), func() (string, int64) { return "INSERT INTO log_row (raw) VALUES (?)", 0 }, err)
		recs := logs.Records()
		if len(recs) != 1 || recs[0].Level != "ERROR" {
			t.Fatalf("%d: records: %s", c.number, logs)
		}
		got, _ := recs[0].Fields["err"].(string)
		if strings.Contains(logs.String(), marker) {
			t.Errorf("%d: the value is in the record: %s", c.number, logs)
		}
		if !strings.Contains(got, c.want) || !strings.Contains(got, fmt.Sprint(c.number)) || !strings.HasPrefix(got, "insert: ") {
			t.Errorf("%d: logged %q, want the error number, the wrapping text and %q", c.number, got, c.want)
		}
		// the caller's error is the driver's, untouched
		var me *gomysql.MySQLError
		if !errors.As(err, &me) || !strings.Contains(err.Error(), marker) {
			t.Errorf("%d: the error handed back was changed: %v", c.number, err)
		}
	}
	// an error that is not MySQL's is logged as it is
	logs := zlogtest.Capture(t)
	newLogger(Config{}).Trace(context.Background(), time.Now(), func() (string, int64) { return "SELECT 1", 0 }, errors.New("invalid connection 'x'"))
	if recs := logs.Records(); len(recs) != 1 || recs[0].Fields["err"] != "invalid connection 'x'" {
		t.Errorf("non-MySQL error: %s", logs)
	}
	// a missing row is no error of the query
	logs = zlogtest.Capture(t)
	newLogger(Config{}).Trace(context.Background(), time.Now(), func() (string, int64) { return "SELECT 1", 0 }, gorm.ErrRecordNotFound)
	if recs := logs.Records(); len(recs) != 1 || recs[0].Level != "DEBUG" {
		t.Errorf("ErrRecordNotFound: %s", logs)
	}
}

// testFailedInsertLogsNoValues runs against a real server: a duplicate key
// makes MySQL quote the value, and Raw(...).Scan goes through GORM's
// recorder on a real connection.
func testFailedInsertLogsNoValues(t *testing.T, rootDSN string) {
	t.Setenv("APP_ENV", "dev")
	ctx := context.Background()
	root, err := gomysql.ParseDSN(rootDSN)
	if err != nil {
		t.Fatalf("MYSQL_TEST_DSN_ROOT: %v", err)
	}
	admin, err := sql.Open("mysql", rootDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	for _, s := range []string{
		"DROP DATABASE IF EXISTS mysqlx_logvalues", "CREATE DATABASE mysqlx_logvalues",
		"CREATE TABLE mysqlx_logvalues.log_row (id BIGINT PRIMARY KEY AUTO_INCREMENT, raw VARCHAR(64) NOT NULL, UNIQUE KEY uk_raw (raw))",
	} {
		if _, err := admin.ExecContext(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	defer admin.Exec("DROP DATABASE IF EXISTS mysqlx_logvalues")
	db, err := Open(ctx, Target{Addr: root.Addr, DB: "mysqlx_logvalues", User: root.User, Password: root.Passwd}, Config{Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	defer Close(db)
	db = db.WithContext(ctx)

	logs := zlogtest.Capture(t)
	value := "dup-" + marker
	if err := db.Create(&logRow{Raw: value}).Error; err != nil {
		t.Fatal(err)
	}
	err = db.Create(&logRow{Raw: value}).Error
	if err == nil || !strings.Contains(err.Error(), "Duplicate entry") || !strings.Contains(err.Error(), value) {
		t.Fatalf("the caller must still get MySQL's own error, value included (callers match on it): %v", err)
	}
	var me *gomysql.MySQLError
	if !errors.As(err, &me) || me.Number != 1062 {
		t.Fatalf("the caller's error is no longer the driver's: %v", err)
	}
	var n int64
	if err := db.Raw("SELECT COUNT(*) FROM log_row WHERE raw = ?", value).Scan(&n).Error; err != nil || n != 1 {
		t.Fatalf("count: n=%d err=%v", n, err)
	}
	if strings.Contains(logs.String(), marker) {
		t.Fatalf("a bound value reached the log:\n%s", logs)
	}
	var failed bool
	for _, r := range logs.Records() {
		if r.Level != "ERROR" {
			continue
		}
		failed = true
		if e, _ := r.Fields["err"].(string); !strings.Contains(e, "Duplicate entry '?' for key") || !strings.Contains(e, "1062") {
			t.Errorf("the failed insert is logged as %q, want MySQL's text with the value masked", e)
		}
		if s, _ := r.Fields["sql"].(string); !strings.Contains(s, "INSERT INTO") || !strings.Contains(s, "?") {
			t.Errorf("the failed statement is logged as %q", s)
		}
	}
	if !failed {
		t.Fatalf("the failed insert is not logged at error:\n%s", logs)
	}
}
