package kitexx_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm/logger"

	"github.com/sezznaw/devkit-common/testx"
	"github.com/sezznaw/devkit-common/zlog/zlogtest"
)

// Two versions of the same table: the second adds a column and an index.
type migV1 struct {
	ID        int64  `gorm:"column:id;type:bigint;primaryKey;autoIncrement"`
	Username  string `gorm:"column:username;type:varchar(64);not null"`
	CreatedAt int64  `gorm:"column:created_at;type:bigint;not null"`
}

func (migV1) TableName() string { return "mig_sample" }

type migV2 struct {
	ID        int64  `gorm:"column:id;type:bigint;primaryKey;autoIncrement"`
	Username  string `gorm:"column:username;type:varchar(64);not null;uniqueIndex:uk_mig_sample_username"`
	Status    int32  `gorm:"column:status;type:tinyint;not null;default:1"`
	CreatedAt int64  `gorm:"column:created_at;type:bigint;not null"`
}

func (migV2) TableName() string { return "mig_sample" }

// A field without an explicit type: refused before anything is touched.
type migUntyped struct {
	ID     int64  `gorm:"column:id;type:bigint;primaryKey"`
	Amount string `gorm:"column:amount"`
}

func (migUntyped) TableName() string { return "mig_untyped" }

func TestAutoMigrateCreatesThenAddsColumnsAndIndexes(t *testing.T) {
	rt := testx.New(t, "ser-mig", testx.WithMySQL(""))
	m := rt.DB.Migrator()

	if err := rt.AutoMigrate(migV1{}); err != nil {
		t.Fatal(err)
	}
	if !m.HasTable("mig_sample") || m.HasColumn(&migV1{}, "status") {
		t.Fatal("v1: table not created as the model says")
	}
	// Same model again: nothing to do, no error.
	if err := rt.AutoMigrate(migV1{}); err != nil {
		t.Fatal(err)
	}
	// v2 adds a column and an index; existing rows survive.
	if err := rt.DB.Exec("INSERT INTO mig_sample (username, created_at) VALUES ('alice', 1)").Error; err != nil {
		t.Fatal(err)
	}
	if err := rt.AutoMigrate(migV2{}); err != nil {
		t.Fatal(err)
	}
	if !m.HasColumn(&migV2{}, "status") || !m.HasIndex(&migV2{}, "uk_mig_sample_username") {
		t.Fatal("v2: column or index not added")
	}
	var n int64
	if err := rt.DB.Raw("SELECT COUNT(*) FROM mig_sample WHERE status = 1").Scan(&n).Error; err != nil || n != 1 {
		t.Fatalf("existing row lost or default not applied: %d %v", n, err)
	}
}

func TestAutoMigrateRefusesAFieldWithoutAType(t *testing.T) {
	rt := testx.New(t, "ser-mig", testx.WithMySQL(""))
	err := rt.AutoMigrate(migUntyped{})
	if err == nil || !strings.Contains(err.Error(), "Amount") || !strings.Contains(err.Error(), "type") {
		t.Fatalf("untyped field accepted: %v", err)
	}
	if rt.DB.Migrator().HasTable("mig_untyped") {
		t.Fatal("table created although the model was refused")
	}
}

func TestAutoMigrateOffOnlyReports(t *testing.T) {
	rt := testx.New(t, "ser-mig", testx.WithMySQL(""))
	off := false
	rt.Config.MySQL.AutoMigrate = &off
	if err := rt.AutoMigrate(migV1{}); err != nil {
		t.Fatal(err)
	}
	if rt.DB.Migrator().HasTable("mig_sample") {
		t.Fatal("auto_migrate: false still created the table")
	}
	if rt.AutoMigrate() != nil {
		t.Fatal("no models is not an error")
	}
}

// v3 widens username and changes the default of status: both are applied.
type migV3 struct {
	ID        int64  `gorm:"column:id;type:bigint;primaryKey;autoIncrement"`
	Username  string `gorm:"column:username;type:varchar(128);not null;uniqueIndex:uk_mig_sample_username"`
	Status    int32  `gorm:"column:status;type:tinyint;not null;default:2"`
	CreatedAt int64  `gorm:"column:created_at;type:bigint;not null"`
}

func (migV3) TableName() string { return "mig_sample" }

// v4 narrows username: refused.
type migV4 struct {
	ID        int64  `gorm:"column:id;type:bigint;primaryKey;autoIncrement"`
	Username  string `gorm:"column:username;type:varchar(32);not null;uniqueIndex:uk_mig_sample_username"`
	Status    int32  `gorm:"column:status;type:tinyint;not null;default:2"`
	CreatedAt int64  `gorm:"column:created_at;type:bigint;not null"`
}

func (migV4) TableName() string { return "mig_sample" }

func columnOf(t *testing.T, rt *testx.Runtime, table, column string) (typ string, def *string) {
	t.Helper()
	var row struct {
		ColumnType    string  `gorm:"column:column_type"`
		ColumnDefault *string `gorm:"column:column_default"`
	}
	if err := rt.DB.Raw("SELECT column_type AS column_type, column_default AS column_default FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = ? AND column_name = ?", table, column).Scan(&row).Error; err != nil {
		t.Fatal(err)
	}
	return row.ColumnType, row.ColumnDefault
}

func TestAutoMigrateAltersAColumnButRefusesToNarrow(t *testing.T) {
	rt := testx.New(t, "ser-mig", testx.WithMySQL(""))
	if err := rt.AutoMigrate(migV2{}); err != nil {
		t.Fatal(err)
	}
	logs := zlogtest.Capture(t)
	if err := rt.AutoMigrate(migV3{}); err != nil {
		t.Fatal(err)
	}
	typ, def := columnOf(t, rt, "mig_sample", "username")
	if typ != "varchar(128)" {
		t.Fatalf("username not widened: %s", typ)
	}
	if _, def = columnOf(t, rt, "mig_sample", "status"); def == nil || *def != "2" {
		t.Fatalf("status default not changed: %v", def)
	}
	if !logs.Has("INFO", "mig_sample: columns altered [status: default '1' -> '2', username: varchar(64) -> varchar(128)]") {
		t.Fatalf("the record says what was altered: %s", logs)
	}

	err := rt.AutoMigrate(migV4{})
	if err == nil || !strings.Contains(err.Error(), "narrow") || !strings.Contains(err.Error(), "mig_sample.username: varchar(128) -> varchar(32)") {
		t.Fatalf("narrowing accepted: %v", err)
	}
	if typ, _ := columnOf(t, rt, "mig_sample", "username"); typ != "varchar(128)" {
		t.Fatalf("narrowing was applied: %s", typ)
	}
}

// countingLogger counts the SQL statements GORM runs.
type countingLogger struct {
	logger.Interface
	n int
}

func (c *countingLogger) LogMode(logger.LogLevel) logger.Interface { return c }
func (c *countingLogger) Trace(context.Context, time.Time, func() (string, int64), error) {
	c.n++
}

func TestAutoMigrateNoChangeIsTwoQueriesAndNoLock(t *testing.T) {
	rt := testx.New(t, "ser-mig", testx.WithMySQL(""))
	if err := rt.AutoMigrate(migV2{}, migUntypedOK{}); err != nil {
		t.Fatal(err)
	}
	logs := zlogtest.Capture(t)
	counter := &countingLogger{Interface: rt.DB.Logger}
	rt.DB.Logger = counter
	if err := rt.AutoMigrate(migV2{}, migUntypedOK{}); err != nil {
		t.Fatal(err)
	}
	if counter.n != 2 {
		t.Fatalf("planning two tables that match took %d queries, want 2 (one for columns, one for indexes)", counter.n)
	}
	if !logs.Has("INFO", "tables match the models") {
		t.Fatalf("%s", logs)
	}
	// The lock is Redis; nothing to do means nothing locked.
	if keys := rt.Redis.Keys(context.Background(), "*auto-migrate*").Val(); len(keys) != 0 {
		t.Fatalf("lock taken although nothing changed: %v", keys)
	}
}

// A second, typed table for the two-tables test. The decimal default is
// the case that bit on dev: MySQL reports `default:0` as "0.000".
type migUntypedOK struct {
	ID     int64  `gorm:"column:id;type:bigint;primaryKey"`
	Note   string `gorm:"column:note;type:varchar(64);not null;default:''"`
	Amount string `gorm:"column:amount;type:decimal(24,3);not null;default:0"`
	Rate   string `gorm:"column:rate;type:decimal(24,3);not null;default:1"`
	Flag   bool   `gorm:"column:flag;type:tinyint(1);not null;default:0"`
}

func (migUntypedOK) TableName() string { return "mig_other" }
