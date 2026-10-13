package kitexx

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/schema"

	"github.com/sezznaw/devkit-common/config"
	"github.com/sezznaw/devkit-common/delayx"
	"github.com/sezznaw/devkit-common/kafkax"
	"github.com/sezznaw/devkit-common/redisx"
	"github.com/sezznaw/devkit-common/zlog"
)

// EnsureFrameworkTables creates the tables the framework itself uses in the
// service's database, when the configuration enables the feature that needs
// them: `outbox` (kafka.outbox, PublishTx) and `delayed_task` (delay). They
// are the framework's, so no project declares them; the same rule as
// AutoMigrate decides whether this may alter the database (local and dev
// yes, elsewhere only check and complain). NewRuntime calls it right after
// opening MySQL; testx calls it for a test database.
//
// EnsureFrameworkTables 建框架自己用的表（outbox、delayed_task），按配置开了什么建什么；
// 和 AutoMigrate 一样 local / dev 才动库，别的环境只检查、缺了就报错说明。
func (rt *Runtime) EnsureFrameworkTables() error {
	if rt.DB == nil {
		return nil
	}
	cfg := rt.Config
	var want []struct{ name, ddl string }
	if cfg.Kafka.Enabled && cfg.Kafka.Outbox.EnabledOrDefault() {
		want = append(want, struct{ name, ddl string }{"outbox", kafkax.OutboxDDL})
	}
	if cfg.Delay.EnabledOrDefault() {
		want = append(want, struct{ name, ddl string }{"delayed_task", delayx.DDL})
	}
	if len(want) == 0 {
		return nil
	}
	m := rt.DB.Migrator()
	var missing []string
	for _, w := range want {
		if !m.HasTable(w.name) {
			missing = append(missing, w.name)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	if !cfg.MySQL.AutoMigrateOn() {
		return fmt.Errorf("kitexx: framework tables missing in this database: %s (auto migrate is off in %s; run the migration step of the release, or set mysql.auto_migrate: true)", strings.Join(missing, ", "), config.Env())
	}
	create := func(ctx context.Context) error {
		for _, w := range want {
			if m.HasTable(w.name) {
				continue
			}
			if err := rt.DB.WithContext(ctx).Exec(w.ddl).Error; err != nil {
				return fmt.Errorf("kitexx: create %s: %w", w.name, err)
			}
			zlog.Info("auto migrate: framework table created", zlog.Str("table", w.name))
		}
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if rt.Redis == nil {
		return create(ctx)
	}
	err := redisx.WithLock(ctx, rt.Redis, "auto-migrate", 2*time.Minute, create, redisx.Wait(5*time.Minute))
	if errors.Is(err, redisx.ErrLockLost) {
		return nil
	}
	return err
}

// AutoMigrate brings the tables of the given models (the project's
// lib/model/mysql structs) up to date with the structs: it creates a table
// that is missing and adds the columns and indexes a table lacks. Call it
// once in app.Setup with the tables this service owns; a table is migrated
// by one service only, the others just read it.
//
// It is the one way a service changes a schema. What it does, and what it
// deliberately does not:
//
//   - Adds tables, columns and indexes. Never drops or renames a column,
//     never narrows a type, never backfills data: those three kinds of
//     change need a hand-written migration (the project's guide says who).
//   - Every field must carry an explicit `type:` in its gorm tag; the
//     column type is a decision, not GORM's default. A model without one is
//     refused before anything is touched.
//   - Runs under a Redis lock when Redis is on, so the replicas and the
//     other services starting at the same time take turns and nobody sees a
//     half-made table. Without Redis it runs unlocked, with a warning.
//   - Logs every difference it finds and fixes ("auto migrate: bet: columns
//     added [settle_time]"), and says so when there is nothing to do.
//   - Is on where the schema may follow the code (local, dev); off in uat
//     and prod, where a release must not alter tables while it rolls out —
//     there it only reports what it would do and the release process runs
//     the migration as its own step. `mysql.auto_migrate` overrides.
//   - Does nothing under --verify (the post-deploy check is read-only).
//
// AutoMigrate 把这些模型（项目 lib/model/mysql 里的结构体）对应的表补到和结构体一致：缺表建表，
// 缺列、缺索引补上。每个服务在 app.Setup 里对自己拥有的表调一次；一张表只由一个服务迁移，别的只读。
// 它不删列、不改名、不收窄类型、不回填数据——这三类改动要人工写迁移；每个字段必须有显式的 type 标签；
// 开着 Redis 时在锁里跑，多副本、多服务同时启动也只有一个在改表；local / dev 默认开，uat / prod 默认关
// （只打印差异），conf 的 mysql.auto_migrate 可覆盖；--verify 下什么都不做。
func (rt *Runtime) AutoMigrate(models ...any) error {
	if rt.DB == nil {
		return errors.New("kitexx: AutoMigrate needs mysql.enabled: true")
	}
	if len(models) == 0 {
		return nil
	}
	if VerifyFlag() {
		zlog.Info("auto migrate: skipped under --verify")
		return nil
	}
	plan, err := migratePlan(rt.DB, models)
	if err != nil {
		return err
	}
	if !rt.Config.MySQL.AutoMigrateOn() {
		for _, p := range plan {
			if p.changes() {
				zlog.Warn("auto migrate is off in this environment: the table differs from the model; run the migration step of the release",
					zlog.Str("table", p.table), zlog.Str("would", p.String()))
			}
		}
		zlog.Info("auto migrate: off", zlog.Str("env", config.Env()), zlog.Int("tables", int64(len(plan))),
			zlog.Str("hint", "mysql.auto_migrate: true turns it on; local and dev have it on by default"))
		return nil
	}
	run := func(ctx context.Context) error {
		// Re-plan inside the lock: another instance may have just done it.
		plan, err = migratePlan(rt.DB, models)
		if err != nil {
			return err
		}
		todo := 0
		for _, p := range plan {
			if p.changes() {
				todo++
			}
		}
		if todo == 0 {
			zlog.Info("auto migrate: tables match the models", zlog.Int("tables", int64(len(plan))))
			return nil
		}
		start := time.Now()
		if err := rt.DB.WithContext(ctx).AutoMigrate(models...); err != nil {
			return fmt.Errorf("kitexx: auto migrate: %w", err)
		}
		for _, p := range plan {
			if p.changes() {
				zlog.Info("auto migrate: "+p.String(), zlog.Str("table", p.table))
			}
		}
		zlog.Info("auto migrate: done", zlog.Int("tables", int64(len(plan))), zlog.Int("changed", int64(todo)), zlog.Dur("took", time.Since(start)))
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if rt.Redis == nil {
		zlog.Warn("auto migrate: redis is off, migrating without a lock (two instances starting together may both alter the table)")
		return run(ctx)
	}
	err = redisx.WithLock(ctx, rt.Redis, "auto-migrate", 2*time.Minute, run, redisx.Wait(5*time.Minute))
	if errors.Is(err, redisx.ErrLockLost) {
		// The lock's ttl is refreshed while run runs; losing it means Redis
		// went away. The DDL itself is done or not; MySQL does not roll it
		// back. Say so and let the service start.
		zlog.Warn("auto migrate: lock lost while migrating (redis went away); the tables are as far as the migration got")
		return nil
	}
	return err
}

// tablePlan is what AutoMigrate would do to one table.
type tablePlan struct {
	table      string
	create     bool
	addColumns []string
	addIndexes []string
}

func (p tablePlan) changes() bool { return p.create || len(p.addColumns) > 0 || len(p.addIndexes) > 0 }

func (p tablePlan) String() string {
	if p.create {
		return p.table + ": table created"
	}
	var parts []string
	if len(p.addColumns) > 0 {
		parts = append(parts, "columns added ["+strings.Join(p.addColumns, ", ")+"]")
	}
	if len(p.addIndexes) > 0 {
		parts = append(parts, "indexes added ["+strings.Join(p.addIndexes, ", ")+"]")
	}
	if len(parts) == 0 {
		return p.table + ": no change"
	}
	return p.table + ": " + strings.Join(parts, "; ")
}

// migratePlan parses the models and compares them with the database. It
// refuses a model whose field has no explicit column type.
func migratePlan(db *gorm.DB, models []any) ([]tablePlan, error) {
	m := db.Migrator()
	var plans []tablePlan
	for _, model := range models {
		stmt := &gorm.Statement{DB: db}
		if err := stmt.Parse(model); err != nil {
			return nil, fmt.Errorf("kitexx: auto migrate: %T: %w", model, err)
		}
		s := stmt.Schema
		if err := checkModel(s); err != nil {
			return nil, err
		}
		p := tablePlan{table: s.Table}
		if !m.HasTable(model) {
			p.create = true
			plans = append(plans, p)
			continue
		}
		for _, f := range s.Fields {
			if f.DBName == "" || f.IgnoreMigration {
				continue
			}
			if !m.HasColumn(model, f.DBName) {
				p.addColumns = append(p.addColumns, f.DBName)
			}
		}
		for _, idx := range s.ParseIndexes() {
			if !m.HasIndex(model, idx.Name) {
				p.addIndexes = append(p.addIndexes, idx.Name)
			}
		}
		sort.Strings(p.addIndexes)
		plans = append(plans, p)
	}
	return plans, nil
}

// checkModel: every column has an explicit type. GORM would otherwise pick
// one (longtext for a string, double for a float), and a decimal amount
// stored as longtext is the kind of mistake that is found months later.
func checkModel(s *schema.Schema) error {
	var missing []string
	for _, f := range s.Fields {
		if f.DBName == "" || f.IgnoreMigration {
			continue
		}
		if strings.TrimSpace(f.TagSettings["TYPE"]) == "" {
			missing = append(missing, f.Name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("kitexx: auto migrate: %s: fields without an explicit type in the gorm tag: %s (write `gorm:\"column:x;type:varchar(64);not null\"`; the column type is a decision, not GORM's default)",
			s.Name, strings.Join(missing, ", "))
	}
	return nil
}
