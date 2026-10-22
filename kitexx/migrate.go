package kitexx

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/schema"

	"github.com/sezznaw/devkit-common/config"
	"github.com/sezznaw/devkit-common/delayx"
	"github.com/sezznaw/devkit-common/kafkax"
	"github.com/sezznaw/devkit-common/mysqlx"
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
	outboxColumn := false // an outbox from before the service column
	for _, w := range want {
		if !m.HasTable(w.name) {
			missing = append(missing, w.name)
		} else if w.name == "outbox" && !m.HasColumn(&kafkax.OutboxRow{}, "service") {
			missing = append(missing, "outbox.service")
			outboxColumn = true
		}
	}
	if len(missing) == 0 {
		return nil
	}
	if !cfg.MySQL.AutoMigrateOn() {
		if takeover() != nil {
			return fmt.Errorf("kitexx: framework tables missing in the %s database: %s (takeover mode never creates tables there; the service deployed to %s creates them when the feature is on there)", takeover().Env, strings.Join(missing, ", "), takeover().Env)
		}
		return fmt.Errorf("kitexx: framework tables missing in this database: %s (auto migrate is off in %s; run the migration step of the release, or set mysql.auto_migrate: true)", strings.Join(missing, ", "), config.Env())
	}
	create := func(ctx context.Context) error {
		if outboxColumn && !m.HasColumn(&kafkax.OutboxRow{}, "service") {
			// the outbox is shared by every service of the database until each has
			// its own: two of them may add the column at once; the second one's
			// duplicate column / key is the first one's success
			if err := rt.DB.WithContext(ctx).Exec(kafkax.OutboxServiceColumn).Error; err != nil && !mysqlx.IsCode(err, 1060, 1061) {
				return fmt.Errorf("kitexx: add outbox.service: %w", err)
			}
			zlog.Info("auto migrate: framework column added", zlog.Str("column", "outbox.service"))
		}
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
//   - Adds tables, columns and indexes, and alters a column whose type,
//     nullability or default differs from the model (a wider varchar, a
//     bigger integer, a new default). Never drops or renames a column,
//     never narrows a type (it refuses to start instead), never backfills
//     data: those kinds of change need a hand-written migration (the
//     project's guide says who).
//   - Reads the tables in one snapshot (two information_schema queries)
//     and compares in memory; when nothing differs it returns at once,
//     without the lock. Only the tables that differ are handed to GORM.
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
//   - Never alters the database in takeover mode: the local instance of
//     `devkit takeover` runs on the shared dev database, and its models may
//     hold changes nobody has pushed. It reports the differences instead.
//
// AutoMigrate 把这些模型（项目 lib/model/mysql 里的结构体）对应的表补到和结构体一致：缺表建表，
// 缺列、缺索引补上。每个服务在 app.Setup 里对自己拥有的表调一次；一张表只由一个服务迁移，别的只读。
// 它不删列、不改名、不收窄类型、不回填数据——这三类改动要人工写迁移；每个字段必须有显式的 type 标签；
// 开着 Redis 时在锁里跑，多副本、多服务同时启动也只有一个在改表；local / dev 默认开，uat / prod 默认关
// （只打印差异），conf 的 mysql.auto_migrate 可覆盖；--verify 下什么都不做；接管模式（devkit takeover）
// 下永远不改表，只打印差异——本机连的是共享的 dev 库，本地模型里可能有没推送的改动。
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
		if m := takeover(); m != nil {
			// The shared database is not this developer's to change: say
			// what differs (queries touching it may fail) and go on.
			for _, p := range plan {
				if p.changes() {
					zlog.Warn("takeover mode does not migrate the "+m.Env+" database: the table differs from your local model; push the model change and deploy the service, then the "+m.Env+" tables follow",
						zlog.Str("table", p.table), zlog.Str("would", p.String()))
				}
			}
			zlog.Info("auto migrate: off in takeover mode", zlog.Str("env", m.Env), zlog.Int("tables", int64(len(plan))))
			return nil
		}
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
	// Nothing to do is the common case (every start after the first): say
	// so and leave, without the lock and without planning again.
	if len(plan.changed()) == 0 {
		zlog.Info("auto migrate: tables match the models", zlog.Int("tables", int64(len(plan))))
		return nil
	}
	if err := plan.refuseNarrowing(); err != nil {
		return err
	}
	run := func(ctx context.Context) error {
		// Re-plan inside the lock: another instance may have just done it.
		plan, err = migratePlan(rt.DB, models)
		if err != nil {
			return err
		}
		if err := plan.refuseNarrowing(); err != nil {
			return err
		}
		todo := plan.changed()
		if len(todo) == 0 {
			zlog.Info("auto migrate: tables match the models", zlog.Int("tables", int64(len(plan))))
			return nil
		}
		start := time.Now()
		// Only the tables the plan found different are handed to GORM, which
		// compares each column itself and alters what differs; the tables
		// that match are not touched at all.
		changed := make([]any, 0, len(todo))
		tables := make([]string, 0, len(todo))
		for _, p := range todo {
			changed = append(changed, p.model)
			tables = append(tables, p.table)
		}
		if err := rt.DB.WithContext(ctx).AutoMigrate(changed...); err != nil {
			return fmt.Errorf("kitexx: auto migrate: %w", err)
		}
		// What happened, from the database, not from the plan.
		after, err := snapshot(rt.DB.WithContext(ctx), tables)
		if err != nil {
			return err
		}
		for _, p := range todo {
			if did := p.applied(after); did != "" {
				zlog.Info("auto migrate: "+did, zlog.Str("table", p.table))
			} else {
				zlog.Info("auto migrate: "+p.table+": checked, nothing to change", zlog.Str("table", p.table), zlog.Str("planned", p.String()))
			}
		}
		zlog.Info("auto migrate: done", zlog.Int("tables", int64(len(plan))), zlog.Int("changed", int64(len(todo))), zlog.Dur("took", time.Since(start)))
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
	table string
	model any
	// before is the table as the database had it when the plan was made
	// (nil when the table does not exist), for the "what happened" record.
	before *tableSnapshot

	create       bool
	addColumns   []string
	addIndexes   []string
	alterColumns []string // "name: have -> want"
	narrowing    []string // the alterations that would narrow a type
}

func (p tablePlan) changes() bool {
	return p.create || len(p.addColumns) > 0 || len(p.addIndexes) > 0 || len(p.alterColumns) > 0
}

func (p tablePlan) String() string {
	if p.create {
		return p.table + ": table created"
	}
	var parts []string
	if len(p.addColumns) > 0 {
		parts = append(parts, "columns added ["+strings.Join(p.addColumns, ", ")+"]")
	}
	if len(p.alterColumns) > 0 {
		parts = append(parts, "columns altered ["+strings.Join(p.alterColumns, ", ")+"]")
	}
	if len(p.addIndexes) > 0 {
		parts = append(parts, "indexes added ["+strings.Join(p.addIndexes, ", ")+"]")
	}
	if len(parts) == 0 {
		return p.table + ": no change"
	}
	return p.table + ": " + strings.Join(parts, "; ")
}

// applied describes what changed between the plan's snapshot of the table
// and the database now; "" when nothing did.
func (p tablePlan) applied(after schemaSnapshot) string {
	now, ok := after[strings.ToLower(p.table)]
	if !ok {
		return ""
	}
	if p.before == nil {
		return p.table + ": table created"
	}
	var added, altered, indexes []string
	for name, col := range now.columns {
		old, had := p.before.columns[name]
		switch {
		case !had:
			added = append(added, name)
		case old.Type != col.Type:
			altered = append(altered, name+": "+old.Type+" -> "+col.Type)
		case old.Nullable != col.Nullable:
			altered = append(altered, name+": "+nullability(old.Nullable)+" -> "+nullability(col.Nullable))
		case !sameDefault(old.Default, col.Default):
			altered = append(altered, name+": default "+defaultText(old.Default)+" -> "+defaultText(col.Default))
		}
	}
	for name := range now.indexes {
		if !p.before.indexes[name] {
			indexes = append(indexes, name)
		}
	}
	sort.Strings(added)
	sort.Strings(altered)
	sort.Strings(indexes)
	var parts []string
	if len(added) > 0 {
		parts = append(parts, "columns added ["+strings.Join(added, ", ")+"]")
	}
	if len(altered) > 0 {
		parts = append(parts, "columns altered ["+strings.Join(altered, ", ")+"]")
	}
	if len(indexes) > 0 {
		parts = append(parts, "indexes added ["+strings.Join(indexes, ", ")+"]")
	}
	if len(parts) == 0 {
		return ""
	}
	return p.table + ": " + strings.Join(parts, "; ")
}

// migrationPlan is the plan for every model handed to AutoMigrate.
type migrationPlan []tablePlan

func (ps migrationPlan) changed() []tablePlan {
	var out []tablePlan
	for _, p := range ps {
		if p.changes() {
			out = append(out, p)
		}
	}
	return out
}

// refuseNarrowing is the rule "never narrows a type" as code: a column whose
// model type holds less than the column does (a shorter varchar, a smaller
// integer, fewer decimal digits, text to varchar, signed to unsigned) is
// not altered, because the ALTER would truncate or reject existing rows.
// The service does not start; someone writes the migration.
func (ps migrationPlan) refuseNarrowing() error {
	var all []string
	for _, p := range ps {
		all = append(all, p.narrowing...)
	}
	if len(all) == 0 {
		return nil
	}
	return fmt.Errorf("kitexx: auto migrate refuses to narrow a column, it could truncate data: %s. Convert the data and alter the column with a hand-written migration, then start the service", strings.Join(all, "; "))
}

// columnInfo is a column as information_schema describes it.
type columnInfo struct {
	Type     string // COLUMN_TYPE, normalized (normalizeType)
	Nullable bool
	Default  *string
}

// tableSnapshot is one table's columns and index names, keys lowercase.
type tableSnapshot struct {
	columns map[string]columnInfo
	indexes map[string]bool
}

// schemaSnapshot holds the tables that exist, by lowercase name.
type schemaSnapshot map[string]tableSnapshot

// snapshot reads the columns and indexes of these tables from
// information_schema in two queries, whatever the number of tables; the
// plan is then made in memory. (GORM's HasColumn / HasIndex are one query
// each, which for a service with twenty tables was a few hundred queries
// at every start.)
func snapshot(db *gorm.DB, tables []string) (schemaSnapshot, error) {
	out := schemaSnapshot{}
	if len(tables) == 0 {
		return out, nil
	}
	var cols []struct {
		TableName     string  `gorm:"column:table_name"`
		ColumnName    string  `gorm:"column:column_name"`
		ColumnType    string  `gorm:"column:column_type"`
		IsNullable    string  `gorm:"column:is_nullable"`
		ColumnDefault *string `gorm:"column:column_default"`
	}
	if err := db.Raw("SELECT table_name AS table_name, column_name AS column_name, column_type AS column_type, is_nullable AS is_nullable, column_default AS column_default FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name IN ?", tables).Scan(&cols).Error; err != nil {
		return nil, fmt.Errorf("kitexx: auto migrate: read columns: %w", err)
	}
	for _, c := range cols {
		t := strings.ToLower(c.TableName)
		ts, ok := out[t]
		if !ok {
			ts = tableSnapshot{columns: map[string]columnInfo{}, indexes: map[string]bool{}}
			out[t] = ts
		}
		ts.columns[strings.ToLower(c.ColumnName)] = columnInfo{Type: normalizeType(c.ColumnType), Nullable: strings.EqualFold(c.IsNullable, "YES"), Default: c.ColumnDefault}
	}
	var idx []struct {
		TableName string `gorm:"column:table_name"`
		IndexName string `gorm:"column:index_name"`
	}
	if err := db.Raw("SELECT DISTINCT table_name AS table_name, index_name AS index_name FROM information_schema.statistics WHERE table_schema = DATABASE() AND table_name IN ?", tables).Scan(&idx).Error; err != nil {
		return nil, fmt.Errorf("kitexx: auto migrate: read indexes: %w", err)
	}
	for _, i := range idx {
		if ts, ok := out[strings.ToLower(i.TableName)]; ok {
			ts.indexes[strings.ToLower(i.IndexName)] = true
		}
	}
	return out, nil
}

// migratePlan parses the models, reads the tables in one snapshot and
// compares the two. It refuses a model whose field has no explicit column
// type. A column whose type, nullability or default differs from the model
// is planned as an alteration: GORM applies it, except a narrowing, which
// refuseNarrowing stops. The comparison errs on the side of flagging a
// table: GORM compares again before it alters anything.
func migratePlan(db *gorm.DB, models []any) (migrationPlan, error) {
	type parsed struct {
		model any
		s     *schema.Schema
	}
	var all []parsed
	var tables []string
	for _, model := range models {
		stmt := &gorm.Statement{DB: db}
		if err := stmt.Parse(model); err != nil {
			return nil, fmt.Errorf("kitexx: auto migrate: %T: %w", model, err)
		}
		if err := checkModel(stmt.Schema); err != nil {
			return nil, err
		}
		all = append(all, parsed{model, stmt.Schema})
		tables = append(tables, stmt.Schema.Table)
	}
	snap, err := snapshot(db, tables)
	if err != nil {
		return nil, err
	}
	var plans migrationPlan
	for _, m := range all {
		s := m.s
		p := tablePlan{table: s.Table, model: m.model}
		ts, ok := snap[strings.ToLower(s.Table)]
		if !ok {
			p.create = true
			plans = append(plans, p)
			continue
		}
		p.before = &ts
		for _, f := range s.Fields {
			if f.DBName == "" || f.IgnoreMigration {
				continue
			}
			name := strings.ToLower(f.DBName)
			col, has := ts.columns[name]
			if !has {
				p.addColumns = append(p.addColumns, f.DBName)
				continue
			}
			want := normalizeType(f.TagSettings["TYPE"])
			if want != col.Type {
				p.alterColumns = append(p.alterColumns, f.DBName+": "+col.Type+" -> "+want)
				if narrows(col.Type, want) {
					p.narrowing = append(p.narrowing, s.Table+"."+f.DBName+": "+col.Type+" -> "+want)
				}
				continue
			}
			wantNullable := !(f.NotNull || f.PrimaryKey)
			if wantNullable != col.Nullable {
				p.alterColumns = append(p.alterColumns, f.DBName+": "+nullability(col.Nullable)+" -> "+nullability(wantNullable))
				continue
			}
			// Defaults: only when both sides state one; a column with a
			// default the model does not mention is left to GORM's judgement
			// when the table is migrated for another reason.
			if d, ok := f.TagSettings["DEFAULT"]; ok && col.Default != nil {
				want := strings.Trim(strings.TrimSpace(d), "'\"")
				if !sameDefault(&want, col.Default) {
					p.alterColumns = append(p.alterColumns, f.DBName+": default "+defaultText(col.Default)+" -> "+defaultText(&want))
				}
			}
		}
		for _, idx := range s.ParseIndexes() {
			if !ts.indexes[strings.ToLower(idx.Name)] {
				p.addIndexes = append(p.addIndexes, idx.Name)
			}
		}
		sort.Strings(p.addIndexes)
		plans = append(plans, p)
	}
	return plans, nil
}

func nullability(nullable bool) string {
	if nullable {
		return "NULL"
	}
	return "NOT NULL"
}

func defaultText(d *string) string {
	if d == nil {
		return "(none)"
	}
	return "'" + *d + "'"
}

// sameDefault compares two column defaults. A number is compared as a
// number: MySQL stores `default:0` on a decimal(24,3) column as "0.000".
func sameDefault(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	if *a == *b {
		return true
	}
	x, errX := strconv.ParseFloat(strings.TrimSpace(*a), 64)
	y, errY := strconv.ParseFloat(strings.TrimSpace(*b), 64)
	return errX == nil && errY == nil && x == y
}

// normalizeType makes a column type from a gorm tag and one from
// information_schema comparable: lowercase, no spaces around commas, no
// display width on integers (MySQL 8 reports none, except tinyint(1)),
// bool spelled as MySQL stores it.
func normalizeType(t string) string {
	t = strings.ToLower(strings.TrimSpace(t))
	t = strings.Join(strings.Fields(t), " ")
	t = strings.ReplaceAll(t, ", ", ",")
	t = strings.ReplaceAll(t, " ,", ",")
	t = strings.ReplaceAll(t, " (", "(")
	switch t {
	case "bool", "boolean":
		return "tinyint(1)"
	}
	base, params, rest := splitType(t)
	if intRank[base] > 0 && !(base == "tinyint" && len(params) == 1 && params[0] == 1) {
		return strings.TrimSpace(base + " " + rest)
	}
	return t
}

// splitType: "decimal(24,3) unsigned" -> "decimal", [24 3], "unsigned".
func splitType(t string) (base string, params []int, rest string) {
	rest = ""
	open := strings.IndexByte(t, '(')
	if open < 0 {
		f := strings.Fields(t)
		if len(f) == 0 {
			return "", nil, ""
		}
		return f[0], nil, strings.Join(f[1:], " ")
	}
	base = strings.TrimSpace(t[:open])
	closeAt := strings.IndexByte(t, ')')
	if closeAt < open {
		return base, nil, ""
	}
	for _, p := range strings.Split(t[open+1:closeAt], ",") {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil {
			return base, nil, strings.TrimSpace(t[closeAt+1:])
		}
		params = append(params, n)
	}
	return base, params, strings.TrimSpace(t[closeAt+1:])
}

var (
	intRank  = map[string]int{"tinyint": 1, "smallint": 2, "mediumint": 3, "int": 4, "integer": 4, "bigint": 5}
	textRank = map[string]int{"tinytext": 1, "text": 2, "mediumtext": 3, "longtext": 4, "tinyblob": 1, "blob": 2, "mediumblob": 3, "longblob": 4}
	sized    = map[string]bool{"varchar": true, "char": true, "varbinary": true, "binary": true}
)

// narrows reports whether changing a column from have to want (both
// normalized) could lose data: a shorter string, a smaller integer or a
// signed one made unsigned, fewer decimal digits, text to a sized string,
// or a smaller text kind. Anything else is allowed.
func narrows(have, want string) bool {
	hb, hp, hr := splitType(have)
	wb, wp, wr := splitType(want)
	switch {
	case sized[hb] && sized[wb]:
		return len(hp) == 1 && len(wp) == 1 && wp[0] < hp[0]
	case intRank[hb] > 0 && intRank[wb] > 0:
		if intRank[wb] < intRank[hb] {
			return true
		}
		return !strings.Contains(hr, "unsigned") && strings.Contains(wr, "unsigned")
	case (hb == "decimal" || hb == "numeric") && (wb == "decimal" || wb == "numeric"):
		if len(hp) == 2 && len(wp) == 2 {
			return wp[0] < hp[0] || wp[1] < hp[1]
		}
		return false
	case textRank[hb] > 0 && textRank[wb] > 0:
		return textRank[wb] < textRank[hb]
	case textRank[hb] > 0 && sized[wb]:
		return true
	}
	return false
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
