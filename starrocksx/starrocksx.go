// Package starrocksx is the read side for reports: a service's handle on
// the StarRocks database `report`, where CDC keeps a copy of the business
// tables (MySQL binlog → Canal → Redpanda → Routine Load, a few seconds
// behind). It is for reports, statistics, back-office lists over large
// ranges and exports, and only for those: it is a copy, eventually
// consistent, so nothing that decides anything (is the balance enough, was
// the bet settled) reads it; that reads MySQL through the service that owns
// the table.
//
// The handle is query only: Query scans a SELECT into a slice of structs,
// QueryRow one row. Every query names the tenant with the `:tenant`
// placeholder (`WHERE tenant_id = :tenant`), which the handle fills with
// this deployment's tenant and refuses to run without; the user is the
// read-only `report` account; a query has a deadline (report.timeout, 30s)
// and a row cap (report.max_rows, 10000: page or aggregate beyond that).
// StarRocks speaks the MySQL protocol, so the driver is the MySQL one.
//
// starrocksx 是报表的读端：服务对 StarRocks `report` 库的句柄，CDC 在那里维护业务表的副本
// （MySQL binlog → Canal → Redpanda → Routine Load，延迟几秒）。只给报表、统计、大范围的后台
// 列表和导出用：它是最终一致的副本，任何要据此做判断的读（余额够不够、注单结没结）都不能用它，
// 那些读走拥有该表的服务查 MySQL。只能查询：Query 把 SELECT 扫进结构体切片，QueryRow 一行。
// 每条查询必须用 `:tenant` 占位符写租户（`WHERE tenant_id = :tenant`），句柄填本部署的租户，
// 没写的拒绝执行；只读账号；有时限（report.timeout，30s）和行数上限（report.max_rows，10000）。
package starrocksx

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"time"

	gomysql "github.com/go-sql-driver/mysql"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/sezznaw/devkit-common/config"
	"github.com/sezznaw/devkit-common/metricsx"
	"github.com/sezznaw/devkit-common/zlog"
)

const tracerName = "github.com/sezznaw/devkit-common/starrocksx"

// Config is the `report:` section.
type Config struct {
	// Enabled opens the report database as rt.Report. Only the report
	// service turns it on (devkit lint, rule report-only).
	Enabled bool `yaml:"enabled"`
	// Source: "platform" (default) reads addr / db / user / password_env
	// from the starrocks section of infra.yaml in Nacos; "static" uses the
	// fields below (a laptop).
	Source   string `yaml:"source"`
	Addr     string `yaml:"addr"`
	DB       string `yaml:"db"`
	User     string `yaml:"user"`
	Password string `yaml:"password"`
	// Timeout of one query. Default 30s.
	Timeout config.Duration `yaml:"timeout"`
	// MaxRows a Query may return; more is an error (page it, or aggregate).
	// Default 10000.
	MaxRows int `yaml:"max_rows"`
	// MaxOpenConns to the FE. Default 8: reports are few and heavy.
	MaxOpenConns int `yaml:"max_open_conns"`
}

const (
	SourceStatic   = "static"
	SourcePlatform = "platform"
)

// SourceName is source or its default, platform.
func (c Config) SourceName() string { return cmp.Or(strings.TrimSpace(c.Source), SourcePlatform) }

// Validate checks the section.
func (c Config) Validate() error {
	switch c.SourceName() {
	case SourceStatic:
		if c.Addr == "" || c.DB == "" || c.User == "" {
			return errors.New("report.source static needs report.addr, report.db and report.user")
		}
	case SourcePlatform:
	default:
		return fmt.Errorf("report.source %q: static or platform", c.Source)
	}
	return nil
}

func (c Config) timeout() time.Duration { return c.Timeout.Or(30 * time.Second) }
func (c Config) maxRows() int {
	if c.MaxRows <= 0 {
		return 10000
	}
	return c.MaxRows
}
func (c Config) maxOpen() int {
	if c.MaxOpenConns <= 0 {
		return 8
	}
	return c.MaxOpenConns
}

// Target is where the report database is.
type Target struct {
	Addr, DB, User, Password string
}

// Static is the target of a static configuration.
func (c Config) Static() Target {
	return Target{Addr: c.Addr, DB: c.DB, User: c.User, Password: c.Password}
}

// Client is the report database handle.
type Client struct {
	db      *sql.DB
	tenant  string
	timeout time.Duration
	maxRows int
	dbName  string
}

// Open connects (MySQL protocol), pings, and returns the handle bound to
// tenant: what `:tenant` becomes in every query.
func Open(ctx context.Context, t Target, cfg Config, tenant string) (*Client, error) {
	if t.Addr == "" || t.DB == "" || t.User == "" {
		return nil, fmt.Errorf("starrocksx: target is incomplete: addr=%q db=%q user=%q", t.Addr, t.DB, t.User)
	}
	if tenant == "" {
		return nil, errors.New("starrocksx: the tenant is required (TENANT_CODE): every report query is filtered by it")
	}
	mc := gomysql.NewConfig()
	mc.User, mc.Passwd, mc.Net, mc.Addr, mc.DBName = t.User, t.Password, "tcp", t.Addr, t.DB
	mc.ParseTime = true
	mc.Timeout = 5 * time.Second
	mc.ReadTimeout = cfg.timeout() + 5*time.Second
	conn, err := gomysql.NewConnector(mc)
	if err != nil {
		return nil, fmt.Errorf("starrocksx: %w", err)
	}
	db := sql.OpenDB(conn)
	db.SetMaxOpenConns(cfg.maxOpen())
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(30 * time.Minute)
	pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := db.PingContext(pctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("starrocksx: ping %s/%s: %w (is StarRocks up, is the report account right?)", t.Addr, t.DB, err)
	}
	return &Client{db: db, tenant: tenant, timeout: cfg.timeout(), maxRows: cfg.maxRows(), dbName: t.DB}, nil
}

// Close closes the pool.
func (c *Client) Close() error { return c.db.Close() }

// Tenant is the value `:tenant` stands for.
func (c *Client) Tenant() string { return c.tenant }

// ErrNoTenant: the query does not name the tenant.
var ErrNoTenant = errors.New("starrocksx: the query has no `tenant_id = :tenant`: every report query is filtered by the tenant")

// ErrNotReadOnly: the statement is not a SELECT.
var ErrNotReadOnly = errors.New("starrocksx: only SELECT (or WITH ... SELECT) runs on the report database")

// ErrTooManyRows: the result exceeds report.max_rows.
var ErrTooManyRows = errors.New("starrocksx: too many rows: page the query (LIMIT/OFFSET) or aggregate")

var tenantPH = regexp.MustCompile(`:tenant\b`)
var leading = regexp.MustCompile(`(?is)^\s*(?:--[^\n]*\n\s*|/\*.*?\*/\s*)*(select|with)\b`)

// prepare checks the statement and turns `:tenant` into a positional `?`
// with the tenant inserted at the right place among args.
func (c *Client) prepare(query string, args []any) (string, []any, error) {
	if !leading.MatchString(query) {
		return "", nil, ErrNotReadOnly
	}
	if !tenantPH.MatchString(query) {
		return "", nil, ErrNoTenant
	}
	var sb strings.Builder
	out := make([]any, 0, len(args)+2)
	ai := 0
	inStr := byte(0)
	for i := 0; i < len(query); i++ {
		ch := query[i]
		if inStr != 0 {
			sb.WriteByte(ch)
			if ch == inStr {
				inStr = 0
			}
			continue
		}
		switch {
		case ch == '\'' || ch == '"' || ch == '`':
			inStr = ch
			sb.WriteByte(ch)
		case ch == '?':
			if ai >= len(args) {
				return "", nil, fmt.Errorf("starrocksx: the query has more ? than arguments (%d)", len(args))
			}
			out = append(out, args[ai])
			ai++
			sb.WriteByte(ch)
		case ch == ':' && strings.HasPrefix(query[i:], ":tenant") && (i+7 == len(query) || !isWord(query[i+7])):
			out = append(out, c.tenant)
			sb.WriteByte('?')
			i += 6
		default:
			sb.WriteByte(ch)
		}
	}
	if ai != len(args) {
		return "", nil, fmt.Errorf("starrocksx: the query has %d ? but %d arguments", ai, len(args))
	}
	return sb.String(), out, nil
}

func isWord(b byte) bool {
	return b == '_' || b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
}

// Query runs a SELECT and scans the rows into dest, a pointer to a slice
// of structs (columns matched to fields by `db:"col"` tags or the
// snake_case of the field name) or of scalars (one column).
//
//	var rows []struct{ Status int `db:"status"`; N int64 `db:"n"` }
//	err := rt.Report.Query(ctx, &rows, "SELECT status, COUNT(*) AS n FROM member WHERE tenant_id = :tenant GROUP BY status")
func (c *Client) Query(ctx context.Context, dest any, query string, args ...any) error {
	rv := reflect.ValueOf(dest)
	if rv.Kind() != reflect.Pointer || rv.Elem().Kind() != reflect.Slice {
		return errors.New("starrocksx: Query scans into a pointer to a slice")
	}
	q, a, err := c.prepare(query, args)
	if err != nil {
		return err
	}
	ctx, span := otel.Tracer(tracerName).Start(ctx, "report.query", trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attribute.String("db.system", "starrocks"), attribute.String("db.name", c.dbName), attribute.String("db.statement", clip(q, 500))))
	defer span.End()
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	start := time.Now()
	rows, err := c.db.QueryContext(ctx, q, a...)
	if err != nil {
		return c.fail(ctx, span, start, q, err)
	}
	defer rows.Close()
	n, err := scanRows(rows, rv.Elem(), c.maxRows)
	if err != nil {
		return c.fail(ctx, span, start, q, err)
	}
	metricsx.ReportQueries.WithLabelValues("ok").Inc()
	metricsx.ReportQueryDuration.Observe(time.Since(start).Seconds())
	zlog.Ctx(ctx).Debug("report query", zlog.Str("sql", clip(q, 300)), zlog.Int("rows", n), zlog.Dur("latency", time.Since(start)))
	return nil
}

// QueryRow runs a SELECT expected to return one row and scans it into
// dest, a pointer to a struct or to a scalar. No row is sql.ErrNoRows.
func (c *Client) QueryRow(ctx context.Context, dest any, query string, args ...any) error {
	rv := reflect.ValueOf(dest)
	if rv.Kind() != reflect.Pointer {
		return errors.New("starrocksx: QueryRow scans into a pointer")
	}
	slice := reflect.New(reflect.SliceOf(rv.Elem().Type()))
	if err := c.Query(ctx, slice.Interface(), query, args...); err != nil {
		return err
	}
	if slice.Elem().Len() == 0 {
		return sql.ErrNoRows
	}
	rv.Elem().Set(slice.Elem().Index(0))
	return nil
}

func (c *Client) fail(ctx context.Context, span trace.Span, start time.Time, q string, err error) error {
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
	metricsx.ReportQueries.WithLabelValues("error").Inc()
	zlog.Ctx(ctx).Error("report query failed", zlog.Str("sql", clip(q, 300)), zlog.Dur("latency", time.Since(start)), zlog.Err(err))
	return fmt.Errorf("starrocksx: %w", err)
}

// scanRows appends each row to the slice; structs by column name, scalars
// by position.
func scanRows(rows *sql.Rows, slice reflect.Value, maxRows int) (int, error) {
	cols, err := rows.Columns()
	if err != nil {
		return 0, err
	}
	elem := slice.Type().Elem()
	isStruct := elem.Kind() == reflect.Struct && elem != reflect.TypeOf(time.Time{})
	var fieldIdx [][]int
	if isStruct {
		fieldIdx = make([][]int, len(cols))
		byName := map[string][]int{}
		var walk func(t reflect.Type, prefix []int)
		walk = func(t reflect.Type, prefix []int) {
			for i := 0; i < t.NumField(); i++ {
				f := t.Field(i)
				if !f.IsExported() {
					continue
				}
				idx := append(append([]int{}, prefix...), i)
				if f.Anonymous && f.Type.Kind() == reflect.Struct {
					walk(f.Type, idx)
					continue
				}
				name := f.Tag.Get("db")
				if name == "" {
					name = snake(f.Name)
				}
				if name == "-" {
					continue
				}
				byName[strings.ToLower(name)] = idx
			}
		}
		walk(elem, nil)
		for i, col := range cols {
			idx, ok := byName[strings.ToLower(col)]
			if !ok {
				return 0, fmt.Errorf("column %q has no field in %s (tag it `db:\"%s\"`)", col, elem, col)
			}
			fieldIdx[i] = idx
		}
	} else if len(cols) != 1 {
		return 0, fmt.Errorf("%d columns into a slice of %s: scan into structs", len(cols), elem)
	}
	n := 0
	for rows.Next() {
		if n >= maxRows {
			return n, ErrTooManyRows
		}
		item := reflect.New(elem).Elem()
		targets := make([]any, len(cols))
		if isStruct {
			for i := range cols {
				targets[i] = item.FieldByIndex(fieldIdx[i]).Addr().Interface()
			}
		} else {
			targets[0] = item.Addr().Interface()
		}
		if err := rows.Scan(targets...); err != nil {
			return n, err
		}
		slice.Set(reflect.Append(slice, item))
		n++
	}
	return n, rows.Err()
}

func snake(s string) string {
	var sb strings.Builder
	for i, r := range s {
		if r >= 'A' && r <= 'Z' {
			if i > 0 && (s[i-1] < 'A' || s[i-1] > 'Z') {
				sb.WriteByte('_')
			}
			r += 'a' - 'A'
		}
		sb.WriteRune(r)
	}
	return sb.String()
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
