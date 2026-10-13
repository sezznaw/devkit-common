package kitexx_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sezznaw/devkit-common/testx"
)

// emptyMigrations is a migrations directory with nothing in it but what
// testx insists on: the schema here comes from the models.
func emptyMigrations(t *testing.T) string {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "000001_nothing.up.sql"), []byte("SELECT 1;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

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
	rt := testx.New(t, "ser-mig", testx.WithMySQL(emptyMigrations(t)))
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
	rt := testx.New(t, "ser-mig", testx.WithMySQL(emptyMigrations(t)))
	err := rt.AutoMigrate(migUntyped{})
	if err == nil || !strings.Contains(err.Error(), "Amount") || !strings.Contains(err.Error(), "type") {
		t.Fatalf("untyped field accepted: %v", err)
	}
	if rt.DB.Migrator().HasTable("mig_untyped") {
		t.Fatal("table created although the model was refused")
	}
}

func TestAutoMigrateOffOnlyReports(t *testing.T) {
	rt := testx.New(t, "ser-mig", testx.WithMySQL(emptyMigrations(t)))
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
