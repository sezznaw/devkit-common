// Package delayx runs a function once at a chosen time: cancel an order
// that is still unpaid in 15 minutes, close betting at kick-off, retry a
// payout in an hour. A task is a row in the service's `delayed_task`
// table, scheduled in the same transaction as the business change that
// needs it (so an order and its cancellation task exist together or not
// at all), and run by a scheduler in the same process when its time comes.
//
// Delivery is at least once: a crash while a task runs reruns it, so a
// handler must be idempotent (re-check the state before acting: "is the
// order still unpaid?"). A failing handler is retried with backoff
// (1m, 2m, 4m ... capped at 1h) up to max_attempts, then the task is
// marked failed and counted in the metrics; the alert on
// delay_tasks_failed catches it. Replicas share the work (SKIP LOCKED).
// A task can be cancelled by its kind and key while still pending (the
// order was paid: cancel the cancellation).
//
// delayx 在指定时刻执行一次函数：15 分钟后取消未支付订单、开赛封盘、一小时后重试派彩。
// 任务是本服务 `delayed_task` 表的一行，和需要它的业务变更同事务写入（订单和它的取消任务
// 要么一起存在要么都不存在），由同一进程里的调度器到点执行。至少一次：执行中崩溃会重跑，
// handler 必须幂等（先查状态："订单还没付吗？"）。失败按退避重试（1m、2m、4m…封顶 1h）直到
// max_attempts，然后标记失败并计入指标（告警）。多副本分摊。pending 的任务可按 kind+key 取消。
package delayx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/sezznaw/devkit-common/config"
	"github.com/sezznaw/devkit-common/metricsx"
	"github.com/sezznaw/devkit-common/zlog"
)

// Config is the `delay:` section. On by default when the service has
// MySQL; nothing runs until app.Setup registers a handler.
type Config struct {
	Enabled      *bool           `yaml:"enabled"`
	PollInterval config.Duration `yaml:"poll_interval"` // default 1s
	Batch        int             `yaml:"batch"`         // default 50
	MaxAttempts  int             `yaml:"max_attempts"`  // default 5
	Timeout      config.Duration `yaml:"timeout"`       // per run, default 60s
	Retention    config.Duration `yaml:"retention"`     // done / failed rows kept, default 168h
}

func (c Config) EnabledOrDefault() bool   { return c.Enabled == nil || *c.Enabled }
func (c Config) interval() time.Duration  { return c.PollInterval.Or(time.Second) }
func (c Config) batch() int               { return orInt(c.Batch, 50) }
func (c Config) maxAttempts() int         { return orInt(c.MaxAttempts, 5) }
func (c Config) timeout() time.Duration   { return c.Timeout.Or(60 * time.Second) }
func (c Config) retention() time.Duration { return c.Retention.Or(168 * time.Hour) }
func orInt(v, def int) int {
	if v <= 0 {
		return def
	}
	return v
}

// Status of a task row.
const (
	StatusPending = "pending"
	StatusRunning = "running"
	StatusDone    = "done"
	StatusFailed  = "failed"
	StatusCancel  = "cancelled"
)

// Task is one row of delayed_task.
type Task struct {
	ID        int64      `gorm:"column:id;primaryKey;autoIncrement"`
	Kind      string     `gorm:"column:kind"`
	Key       string     `gorm:"column:task_key"`
	RunAt     time.Time  `gorm:"column:run_at"`
	Payload   []byte     `gorm:"column:payload"`
	Status    string     `gorm:"column:status"`
	Attempts  int        `gorm:"column:attempts"`
	LastError string     `gorm:"column:last_error"`
	LockedAt  *time.Time `gorm:"column:locked_at"`
	CreatedAt time.Time  `gorm:"column:created_at"`
	UpdatedAt time.Time  `gorm:"column:updated_at"`
}

// TableName is "delayed_task".
func (Task) TableName() string { return "delayed_task" }

// DDL is the table; the project's migrations create it.
const DDL = `CREATE TABLE IF NOT EXISTS delayed_task (
  id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  kind        VARCHAR(64)  NOT NULL COMMENT 'handler 名，如 order.cancel-unpaid',
  task_key    VARCHAR(128) NOT NULL DEFAULT '' COMMENT '业务键，如订单号；kind+key 可取消',
  run_at      DATETIME(3)  NOT NULL COMMENT '到点时间，UTC',
  payload     JSON         NOT NULL,
  status      VARCHAR(16)  NOT NULL DEFAULT 'pending' COMMENT 'pending running done failed cancelled',
  attempts    INT          NOT NULL DEFAULT 0,
  last_error  VARCHAR(512) NOT NULL DEFAULT '',
  locked_at   DATETIME(3)  NULL,
  created_at  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  KEY idx_delayed_task_due (status, run_at),
  KEY idx_delayed_task_key (kind, task_key)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci`

// Handler runs one task; payload is what Schedule stored. Return nil when
// done (also when there was nothing left to do), an error to retry later.
type Handler func(ctx context.Context, t *Task) error

// Decode reads the payload into v.
func (t *Task) Decode(v any) error { return json.Unmarshal(t.Payload, v) }

// Scheduler owns the handlers and the poller of one service.
type Scheduler struct {
	db       *gorm.DB
	cfg      Config
	service  string
	mu       sync.RWMutex
	handlers map[string]Handler
	nudge    chan struct{}
	stop     context.CancelFunc
	wg       sync.WaitGroup
}

// New makes the scheduler of a service on its database. The runtime does
// it (rt.Delay); Start runs the poller.
func New(db *gorm.DB, cfg Config, service string) *Scheduler {
	return &Scheduler{db: db, cfg: cfg, service: service, handlers: map[string]Handler{}, nudge: make(chan struct{}, 1)}
}

// Handle registers the function of a kind. Register every kind the
// service schedules, in app.Setup; a task whose kind has no handler
// stays pending and is reported.
func (s *Scheduler) Handle(kind string, h Handler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlers[kind] = h
}

// Schedule stores a task to run at runAt, inside tx (the business
// change's transaction; pass s.DB() when there is none). payload is
// JSON-encoded for the handler.
//
// Schedule 在事务 tx 里登记一个 runAt 时刻执行的任务（没有事务就传 s.DB()）。
func (s *Scheduler) Schedule(ctx context.Context, tx *gorm.DB, kind, key string, runAt time.Time, payload any) (int64, error) {
	if tx == nil {
		return 0, errors.New("delayx: Schedule needs the transaction's *gorm.DB (or s.DB())")
	}
	if kind == "" {
		return 0, errors.New("delayx: kind is required")
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return 0, fmt.Errorf("delayx: encode payload of %s: %w", kind, err)
	}
	t := &Task{Kind: kind, Key: key, RunAt: runAt.UTC(), Payload: b, Status: StatusPending}
	if err := tx.WithContext(ctx).Create(t).Error; err != nil {
		return 0, fmt.Errorf("delayx: schedule %s: %w", kind, err)
	}
	zlog.Ctx(ctx).Info("task scheduled", zlog.Str("kind", kind), zlog.Str("key", key), zlog.Int("task_id", t.ID), zlog.Str("run_at", runAt.UTC().Format(time.RFC3339)))
	if time.Until(runAt) <= s.cfg.interval() {
		s.poke()
	}
	return t.ID, nil
}

// Cancel marks the pending tasks of kind and key cancelled (inside tx when
// the cancellation belongs to a change). Returns how many it cancelled.
func (s *Scheduler) Cancel(ctx context.Context, tx *gorm.DB, kind, key string) (int64, error) {
	if tx == nil {
		tx = s.db
	}
	res := tx.WithContext(ctx).Model(&Task{}).Where("kind = ? AND task_key = ? AND status = ?", kind, key, StatusPending).
		Updates(map[string]any{"status": StatusCancel, "updated_at": time.Now()})
	if res.Error != nil {
		return 0, res.Error
	}
	if res.RowsAffected > 0 {
		zlog.Ctx(ctx).Info("task cancelled", zlog.Str("kind", kind), zlog.Str("key", key), zlog.Int("count", res.RowsAffected))
	}
	return res.RowsAffected, nil
}

// DB is the handle for a Schedule without a transaction.
func (s *Scheduler) DB() *gorm.DB { return s.db }

// Handlers is how many kinds are registered; the runtime starts the poller
// only when there is at least one.
func (s *Scheduler) Handlers() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.handlers)
}

// FailedCount is how many tasks used up their attempts, for the
// post-deploy check and the alert.
func (s *Scheduler) FailedCount(ctx context.Context) (int64, error) {
	var n int64
	err := s.db.WithContext(ctx).Model(&Task{}).Where("status = ?", StatusFailed).Count(&n).Error
	return n, err
}

func (s *Scheduler) poke() {
	select {
	case s.nudge <- struct{}{}:
	default:
	}
}

// Start runs the poller until ctx ends (the runtime starts it right
// before the server). Nothing runs without a registered handler.
func (s *Scheduler) Start(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	s.stop = cancel
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(s.cfg.interval())
		defer ticker.Stop()
		cleanup := time.NewTicker(time.Hour)
		defer cleanup.Stop()
		s.mu.RLock()
		n := len(s.handlers)
		s.mu.RUnlock()
		zlog.Info("delayed tasks on", zlog.Int("handlers", int64(n)), zlog.Dur("poll_interval", s.cfg.interval()), zlog.Int("max_attempts", int64(s.cfg.maxAttempts())))
		for {
			for {
				ran, err := s.runOnce(ctx)
				if err != nil && ctx.Err() == nil {
					zlog.Error("delayed tasks round", zlog.Err(err))
				}
				if ran < s.cfg.batch() {
					break
				}
			}
			s.observe(ctx)
			select {
			case <-ctx.Done():
				return
			case <-s.nudge:
			case <-ticker.C:
			case <-cleanup.C:
				s.cleanup(ctx)
			}
		}
	}()
}

// Stop ends the poller and waits for the task in progress.
func (s *Scheduler) Stop() {
	if s.stop != nil {
		s.stop()
	}
	s.wg.Wait()
}

// runOnce claims a batch of due tasks (one transaction, SKIP LOCKED, marked
// running) and runs them; returns how many it claimed.
func (s *Scheduler) runOnce(ctx context.Context) (int, error) {
	var claimed []Task
	now := time.Now()
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var due []Task
		// a task stuck in running for longer than the timeout (the process
		// died) is claimed again
		stale := now.Add(-s.cfg.timeout() - time.Minute)
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).
			Where("(status = ? AND run_at <= ?) OR (status = ? AND locked_at < ?)", StatusPending, now, StatusRunning, stale).
			Order("run_at").Limit(s.cfg.batch()).Find(&due).Error; err != nil {
			return err
		}
		for i := range due {
			if err := tx.Model(&due[i]).Updates(map[string]any{"status": StatusRunning, "locked_at": now}).Error; err != nil {
				return err
			}
			due[i].Status, due[i].LockedAt = StatusRunning, &now
		}
		claimed = due
		return nil
	})
	if err != nil {
		return 0, err
	}
	for i := range claimed {
		s.run(ctx, &claimed[i])
	}
	return len(claimed), nil
}

func (s *Scheduler) run(ctx context.Context, t *Task) {
	s.mu.RLock()
	h, ok := s.handlers[t.Kind]
	s.mu.RUnlock()
	start := time.Now()
	logger := zlog.With(zlog.Str("kind", t.Kind), zlog.Str("key", t.Key), zlog.Int("task_id", t.ID), zlog.Int("attempt", int64(t.Attempts+1)))
	if !ok {
		// no handler in this build: back to pending for a build that has one
		s.db.WithContext(ctx).Model(t).Updates(map[string]any{"status": StatusPending, "locked_at": nil, "last_error": "no handler for kind " + t.Kind})
		logger.Error("delayed task has no handler")
		metricsx.DelayTasksRun.WithLabelValues(t.Kind, "no_handler").Inc()
		return
	}
	rctx, cancel := context.WithTimeout(ctx, s.cfg.timeout())
	err := safely(rctx, h, t)
	cancel()
	if err == nil {
		s.db.WithContext(ctx).Model(t).Updates(map[string]any{"status": StatusDone, "attempts": t.Attempts + 1, "last_error": "", "locked_at": nil})
		logger.Info("delayed task done", zlog.Dur("took", time.Since(start)), zlog.Dur("late", start.Sub(t.RunAt)))
		metricsx.DelayTasksRun.WithLabelValues(t.Kind, "ok").Inc()
		return
	}
	msg := err.Error()
	if len(msg) > 500 {
		msg = msg[:500]
	}
	attempts := t.Attempts + 1
	if attempts >= s.cfg.maxAttempts() {
		s.db.WithContext(ctx).Model(t).Updates(map[string]any{"status": StatusFailed, "attempts": attempts, "last_error": msg, "locked_at": nil})
		logger.Error("delayed task failed for good", zlog.Int("attempts", int64(attempts)), zlog.Err(err))
		metricsx.DelayTasksRun.WithLabelValues(t.Kind, "failed").Inc()
		return
	}
	next := time.Now().Add(backoff(attempts))
	s.db.WithContext(ctx).Model(t).Updates(map[string]any{"status": StatusPending, "attempts": attempts, "last_error": msg, "locked_at": nil, "run_at": next})
	logger.Warn("delayed task failed, will retry", zlog.Str("next", next.UTC().Format(time.RFC3339)), zlog.Err(err))
	metricsx.DelayTasksRun.WithLabelValues(t.Kind, "retry").Inc()
}

func safely(ctx context.Context, h Handler, t *Task) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return h(ctx, t)
}

// backoff: 1m, 2m, 4m ... capped at 1h.
func backoff(attempts int) time.Duration {
	d := time.Minute << uint(min(attempts-1, 10))
	if d > time.Hour {
		d = time.Hour
	}
	return d
}

func (s *Scheduler) observe(ctx context.Context) {
	var pending, failed int64
	s.db.WithContext(ctx).Model(&Task{}).Where("status = ?", StatusPending).Count(&pending)
	s.db.WithContext(ctx).Model(&Task{}).Where("status = ?", StatusFailed).Count(&failed)
	metricsx.DelayTasksPending.Set(float64(pending))
	metricsx.DelayTasksFailed.Set(float64(failed))
	var oldest struct{ RunAt *time.Time }
	s.db.WithContext(ctx).Model(&Task{}).Select("MIN(run_at) AS run_at").Where("status = ? AND run_at <= ?", StatusPending, time.Now()).Scan(&oldest)
	if oldest.RunAt == nil {
		metricsx.DelayTasksOverdue.Set(0)
	} else {
		metricsx.DelayTasksOverdue.Set(time.Since(*oldest.RunAt).Seconds())
	}
}

func (s *Scheduler) cleanup(ctx context.Context) {
	res := s.db.WithContext(ctx).Where("status IN ? AND updated_at < ?", []string{StatusDone, StatusCancel}, time.Now().Add(-s.cfg.retention())).Limit(10000).Delete(&Task{})
	if res.Error != nil {
		zlog.Error("delayed tasks cleanup", zlog.Err(res.Error))
		return
	}
	if res.RowsAffected > 0 {
		zlog.Info("delayed tasks cleanup", zlog.Int("deleted", res.RowsAffected))
	}
}

// Overdue is the age of the oldest due task not yet run, for the
// post-deploy check; 0 when none.
func (s *Scheduler) Overdue(ctx context.Context) (time.Duration, error) {
	var oldest struct{ RunAt *time.Time }
	err := s.db.WithContext(ctx).Model(&Task{}).Select("MIN(run_at) AS run_at").Where("status = ? AND run_at <= ?", StatusPending, time.Now()).Scan(&oldest).Error
	if err != nil || oldest.RunAt == nil {
		return 0, err
	}
	return time.Since(*oldest.RunAt), nil
}
