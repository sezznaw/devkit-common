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
// order was paid: cancel the cancellation), or aborted while running.
//
// A kind whose runs take minutes (an export, a reconciliation) is
// registered with Long(): its tasks run in a small pool of their own
// (delay.long_workers) so they never hold up the short tasks, and with
// Timeout(d) for a limit of its own. Every running task heartbeats its
// row; a row whose heartbeat stopped (the process died) is claimed again.
// A run interrupted by the service stopping goes back to pending at once,
// without counting as an attempt.
//
// delayx 在指定时刻执行一次函数：15 分钟后取消未支付订单、开赛封盘、一小时后重试派彩。
// 任务是本服务 `delayed_task` 表的一行，和需要它的业务变更同事务写入（订单和它的取消任务
// 要么一起存在要么都不存在），由同一进程里的调度器到点执行。至少一次：执行中崩溃会重跑，
// handler 必须幂等（先查状态："订单还没付吗？"）。失败按退避重试（1m、2m、4m…封顶 1h）直到
// max_attempts，然后标记失败并计入指标（告警）。多副本分摊。pending 的任务可按 kind+key 取消，
// 运行中的可按 kind+key 终止（Abort）。
//
// 一次要跑几分钟的 kind（导出、对账）用 Long() 注册：在自己的小池子里跑（delay.long_workers），
// 不挡短任务；Timeout(d) 给它单独的时限。运行中的任务定时给自己的行打心跳，心跳停了的行
// （进程死了）会被重新认领。被服务停机打断的任务立刻放回 pending，不计一次尝试。
package delayx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
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
	Timeout      config.Duration `yaml:"timeout"`       // per run, default 60s; a kind overrides it with Timeout(d)
	Retention    config.Duration `yaml:"retention"`     // done / failed rows kept, default 168h
	LongWorkers  int             `yaml:"long_workers"`  // runs of Long() kinds at once per replica, default 1
	Heartbeat    config.Duration `yaml:"heartbeat"`     // a running task refreshes locked_at this often, default 30s
}

func (c Config) EnabledOrDefault() bool   { return c.Enabled == nil || *c.Enabled }
func (c Config) interval() time.Duration  { return c.PollInterval.Or(time.Second) }
func (c Config) batch() int               { return orInt(c.Batch, 50) }
func (c Config) maxAttempts() int         { return orInt(c.MaxAttempts, 5) }
func (c Config) timeout() time.Duration   { return c.Timeout.Or(60 * time.Second) }
func (c Config) retention() time.Duration { return c.Retention.Or(168 * time.Hour) }
func (c Config) longWorkers() int         { return orInt(c.LongWorkers, 1) }
func (c Config) heartbeat() time.Duration { return c.Heartbeat.Or(30 * time.Second) }

// stale is how long a running row may go without a heartbeat before it
// counts as abandoned (the process died) and is claimed again.
func (c Config) stale() time.Duration { return 3 * c.heartbeat() }

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

// ErrAborted is the cause of a handler's ctx when Abort was called on its
// task: `context.Cause(ctx) == delayx.ErrAborted`. The row is already
// cancelled; the handler only has to stop and clean up.
var ErrAborted = errors.New("delayx: task aborted")

// ErrShutdown is the cause of a handler's ctx when the service is
// stopping. The task goes back to pending without counting an attempt.
var ErrShutdown = errors.New("delayx: service stopping")

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

// DDL is the table; the framework creates it (EnsureFrameworkTables).
const DDL = `CREATE TABLE IF NOT EXISTS delayed_task (
  id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  kind        VARCHAR(64)  NOT NULL COMMENT 'handler 名，如 order.cancel-unpaid',
  task_key    VARCHAR(128) NOT NULL DEFAULT '' COMMENT '业务键，如订单号；kind+key 可取消',
  run_at      DATETIME(3)  NOT NULL COMMENT '到点时间，UTC',
  payload     JSON         NOT NULL,
  status      VARCHAR(16)  NOT NULL DEFAULT 'pending' COMMENT 'pending running done failed cancelled',
  attempts    INT          NOT NULL DEFAULT 0,
  last_error  VARCHAR(512) NOT NULL DEFAULT '',
  locked_at   DATETIME(3)  NULL COMMENT '认领时间，运行中每个心跳刷新',
  created_at  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  KEY idx_delayed_task_due (status, run_at),
  KEY idx_delayed_task_key (kind, task_key)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci`

// Handler runs one task; payload is what Schedule stored. Return nil when
// done (also when there was nothing left to do), an error to retry later.
// ctx ends at the kind's timeout, on Abort (cause ErrAborted) and when
// the service stops (cause ErrShutdown).
type Handler func(ctx context.Context, t *Task) error

// Decode reads the payload into v.
func (t *Task) Decode(v any) error { return json.Unmarshal(t.Payload, v) }

// Option configures the handler of a kind (Handle).
type Option func(*spec)

type spec struct {
	h       Handler
	long    bool
	timeout time.Duration
}

// Long marks a kind whose runs take minutes (an export, a reconciliation):
// its tasks run in the long-task pool (delay.long_workers at once per
// replica, default 1) instead of the poller, so the short tasks of the
// service are never held up behind them. Combine with Timeout.
//
// Long 标记一次要跑几分钟的 kind（导出、对账）：在长任务池里跑（每副本同时 delay.long_workers 个，
// 默认 1），不占轮询协程，短任务不会排在它后面。通常和 Timeout 一起用。
func Long() Option { return func(s *spec) { s.long = true } }

// Timeout is the limit of one run of this kind, instead of delay.timeout.
// At the limit ctx ends; what the handler returns decides retry or not.
//
// Timeout 是这个 kind 一次执行的时限，代替 delay.timeout。到时 ctx 结束，重试与否看 handler 返回什么。
func Timeout(d time.Duration) Option { return func(s *spec) { s.timeout = d } }

// Scheduler owns the handlers and the poller of one service.
type Scheduler struct {
	db      *gorm.DB
	cfg     Config
	service string
	mu      sync.RWMutex
	specs   map[string]spec
	nudge   chan struct{}
	stop    context.CancelFunc
	wg      sync.WaitGroup
	longSem chan struct{}

	// running holds the cancel of every task this process is running, so
	// Abort takes effect at once here (other replicas notice at their next
	// heartbeat) and Stop can tell an interrupted run from a failed one.
	runMu   sync.Mutex
	running map[int64]context.CancelCauseFunc
	// inflight counts claimed tasks until their outcome is written, the
	// long ones in their goroutines included: what Running reports.
	inflight atomic.Int32
}

// New makes the scheduler of a service on its database. The runtime does
// it (rt.Delay); Start runs the poller.
func New(db *gorm.DB, cfg Config, service string) *Scheduler {
	return &Scheduler{
		db: db, cfg: cfg, service: service,
		specs:   map[string]spec{},
		nudge:   make(chan struct{}, 1),
		longSem: make(chan struct{}, cfg.longWorkers()),
		running: map[int64]context.CancelCauseFunc{},
	}
}

// Handle registers the function of a kind, with its options (Long,
// Timeout). Register every kind the service schedules, in app.Setup. The
// scheduler claims, counts and cleans up only the kinds registered here:
// the table may be shared by several services of one database, and each
// one's rows are its own. A task of a kind nobody registers stays pending.
func (s *Scheduler) Handle(kind string, h Handler, opts ...Option) {
	sp := spec{h: h}
	for _, o := range opts {
		o(&sp)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.specs[kind] = sp
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
// A task already running is not touched; Abort stops those too.
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

// Abort cancels the tasks of kind and key whether pending or running. A
// run in this process gets its ctx ended now (cause ErrAborted); one on
// another replica at its next heartbeat (delay.heartbeat, 30s). The row
// is cancelled either way; what the handler had done stays, so a handler
// that must undo its work checks `context.Cause(ctx) == delayx.ErrAborted`
// before returning. Returns how many rows it cancelled.
//
// Abort 终止 kind+key 的任务，pending 和 running 都算。本进程里正在跑的立刻结束 ctx
// （cause 是 ErrAborted），别的副本上的在它下一个心跳时结束。行一律改为 cancelled。
func (s *Scheduler) Abort(ctx context.Context, tx *gorm.DB, kind, key string) (int64, error) {
	if tx == nil {
		tx = s.db
	}
	var ids []int64
	if err := tx.WithContext(ctx).Model(&Task{}).Where("kind = ? AND task_key = ? AND status IN ?", kind, key, []string{StatusPending, StatusRunning}).
		Pluck("id", &ids).Error; err != nil {
		return 0, err
	}
	if len(ids) == 0 {
		return 0, nil
	}
	res := tx.WithContext(ctx).Model(&Task{}).Where("id IN ? AND status IN ?", ids, []string{StatusPending, StatusRunning}).
		Updates(map[string]any{"status": StatusCancel, "locked_at": nil, "updated_at": time.Now()})
	if res.Error != nil {
		return 0, res.Error
	}
	for _, id := range ids {
		s.cancelLocal(id, ErrAborted)
	}
	zlog.Ctx(ctx).Info("task aborted", zlog.Str("kind", kind), zlog.Str("key", key), zlog.Int("count", res.RowsAffected))
	return res.RowsAffected, nil
}

// DB is the handle for a Schedule without a transaction.
func (s *Scheduler) DB() *gorm.DB { return s.db }

// Handlers is how many kinds are registered; the runtime starts the poller
// only when there is at least one.
func (s *Scheduler) Handlers() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.specs)
}

// Running is how many tasks this process is running right now (the long
// ones included). Tests wait for it to reach zero after RunOnce.
func (s *Scheduler) Running() int { return int(s.inflight.Load()) }

// FailedCount is how many tasks used up their attempts, for the
// post-deploy check and the alert.
func (s *Scheduler) FailedCount(ctx context.Context) (int64, error) {
	kinds := s.all()
	if len(kinds) == 0 {
		return 0, nil
	}
	var n int64
	err := s.db.WithContext(ctx).Model(&Task{}).Where("kind IN ? AND status = ?", kinds, StatusFailed).Count(&n).Error
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
		var lastObserve time.Time
		_, long := s.kinds()
		zlog.Info("delayed tasks on", zlog.Int("handlers", int64(s.Handlers())), zlog.Int("long_kinds", int64(len(long))), zlog.Int("long_workers", int64(s.cfg.longWorkers())),
			zlog.Dur("poll_interval", s.cfg.interval()), zlog.Dur("heartbeat", s.cfg.heartbeat()), zlog.Int("max_attempts", int64(s.cfg.maxAttempts())))
		for {
			for {
				ran, err := s.RunOnce(ctx)
				if err != nil && ctx.Err() == nil {
					zlog.Error("delayed tasks round", zlog.Err(err))
				}
				if ran < s.cfg.batch() {
					break
				}
			}
			if time.Since(lastObserve) >= observeEvery {
				s.observe(ctx)
				lastObserve = time.Now()
			}
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

// Stop ends the poller, ends the ctx of every run in progress (cause
// ErrShutdown) and waits for them to return; each interrupted task is put
// back to pending to run again at once, without counting an attempt.
func (s *Scheduler) Stop() {
	// runs in progress first, so that their ctx carries ErrShutdown as the
	// cause; then the poller, whose ctx the runs descend from
	s.runMu.Lock()
	for _, cancel := range s.running {
		cancel(ErrShutdown)
	}
	s.runMu.Unlock()
	if s.stop != nil {
		s.stop()
	}
	s.wg.Wait()
}

// all is every registered kind: the rows this scheduler owns.
func (s *Scheduler) all() []string {
	short, long := s.kinds()
	return append(short, long...)
}

// kinds splits the registered kinds into short and long.
func (s *Scheduler) kinds() (short, long []string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for k, sp := range s.specs {
		if sp.long {
			long = append(long, k)
		} else {
			short = append(short, k)
		}
	}
	return short, long
}

func (s *Scheduler) spec(kind string) (spec, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sp, ok := s.specs[kind]
	return sp, ok
}

// RunOnce claims one batch of due tasks (one transaction, SKIP LOCKED,
// marked running) and runs them: short kinds here, in order; long kinds
// handed to the pool, as many as it has room for. Returns how many it
// claimed. The poller calls it; a test calls it to run what is due
// without waiting, then waits for Running() to reach zero.
func (s *Scheduler) RunOnce(ctx context.Context) (int, error) {
	short, long := s.kinds()
	if len(short) == 0 && len(long) == 0 {
		return 0, nil
	}
	free := cap(s.longSem) - len(s.longSem)
	var claimed []Task
	now := time.Now()
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// a row of one of our kinds that is due, or running without a heartbeat
		// for stale() (the process died), is claimed; long kinds only up to the
		// pool's room. Other services' kinds in the same table are theirs.
		due := func(limit int, kinds []string) ([]Task, error) {
			var rows []Task
			err := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).
				Where("kind IN ? AND ((status = ? AND run_at <= ?) OR (status = ? AND locked_at < ?))", kinds, StatusPending, now, StatusRunning, now.Add(-s.cfg.stale())).
				Order("run_at").Limit(limit).Find(&rows).Error
			return rows, err
		}
		var rows []Task
		if len(short) > 0 {
			var err error
			if rows, err = due(s.cfg.batch(), short); err != nil {
				return err
			}
		}
		if len(long) > 0 && free > 0 {
			more, err := due(free, long)
			if err != nil {
				return err
			}
			rows = append(rows, more...)
		}
		for i := range rows {
			if err := tx.Model(&rows[i]).Updates(map[string]any{"status": StatusRunning, "locked_at": now}).Error; err != nil {
				return err
			}
			rows[i].Status, rows[i].LockedAt = StatusRunning, &now
		}
		claimed = rows
		return nil
	})
	if err != nil {
		return 0, err
	}
	s.inflight.Add(int32(len(claimed)))
	for i := range claimed {
		t := &claimed[i]
		if sp, ok := s.spec(t.Kind); ok && sp.long {
			s.longSem <- struct{}{} // room was checked above; the poller is the only sender
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				defer func() { <-s.longSem }()
				s.run(ctx, t)
			}()
			continue
		}
		s.run(ctx, t)
	}
	return len(claimed), nil
}

// bookkeeping is the ctx for writing a task's outcome: the run's own ctx
// may be over (timeout, abort, shutdown) and the row still has to be updated.
func bookkeeping(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
}

func (s *Scheduler) run(ctx context.Context, t *Task) {
	defer s.inflight.Add(-1)
	sp, ok := s.spec(t.Kind)
	start := time.Now()
	logger := zlog.With(zlog.Str("kind", t.Kind), zlog.Str("key", t.Key), zlog.Int("task_id", t.ID), zlog.Int("attempt", int64(t.Attempts+1)))
	bctx, bcancel := bookkeeping(ctx)
	defer bcancel()
	if !ok {
		// no handler in this build: back to pending for a build that has one
		s.db.WithContext(bctx).Model(t).Where("status = ?", StatusRunning).Updates(map[string]any{"status": StatusPending, "locked_at": nil, "last_error": "no handler for kind " + t.Kind})
		logger.Error("delayed task has no handler")
		metricsx.DelayTasksRun.WithLabelValues(t.Kind, "no_handler").Inc()
		return
	}
	timeout := sp.timeout
	if timeout <= 0 {
		timeout = s.cfg.timeout()
	}
	rctx, cancel := context.WithCancelCause(ctx)
	rctx, tcancel := context.WithTimeout(rctx, timeout)
	s.track(t.ID, cancel)
	stopBeat := s.heartbeat(rctx, t, cancel)
	err := safely(rctx, sp.h, t)
	cause := context.Cause(rctx) // before our own cancels overwrite it
	stopBeat()
	tcancel()
	s.untrack(t.ID)
	cancel(nil)

	outcome := func(result string, fields map[string]any) int64 {
		res := s.db.WithContext(bctx).Model(t).Where("status = ?", StatusRunning).Updates(fields)
		if res.Error != nil {
			logger.Error("delayed task: write outcome", zlog.Str("result", result), zlog.Err(res.Error))
		}
		metricsx.DelayTasksRun.WithLabelValues(t.Kind, result).Inc()
		return res.RowsAffected
	}
	switch {
	case errors.Is(cause, ErrAborted):
		// Abort already made the row cancelled; the guard above means a
		// row that is somehow still running is finished as cancelled too
		outcome("aborted", map[string]any{"status": StatusCancel, "locked_at": nil, "last_error": clip(ErrAborted.Error())})
		logger.Info("delayed task aborted", zlog.Dur("took", time.Since(start)))
	case err != nil && (errors.Is(cause, ErrShutdown) || ctx.Err() != nil):
		// interrupted by the service stopping: run again at once, no attempt counted
		outcome("requeued", map[string]any{"status": StatusPending, "locked_at": nil, "run_at": time.Now(), "last_error": clip(ErrShutdown.Error())})
		logger.Info("delayed task interrupted by shutdown, back to pending", zlog.Dur("took", time.Since(start)))
	case err == nil:
		if n := outcome("ok", map[string]any{"status": StatusDone, "attempts": t.Attempts + 1, "last_error": "", "locked_at": nil}); n == 0 {
			logger.Warn("delayed task finished but its row was no longer running (aborted or reclaimed)")
		}
		logger.Info("delayed task done", zlog.Dur("took", time.Since(start)), zlog.Dur("late", start.Sub(t.RunAt)))
	default:
		attempts := t.Attempts + 1
		if attempts >= s.cfg.maxAttempts() {
			outcome("failed", map[string]any{"status": StatusFailed, "attempts": attempts, "last_error": clip(err.Error()), "locked_at": nil})
			logger.Error("delayed task failed for good", zlog.Int("attempts", int64(attempts)), zlog.Err(err))
			return
		}
		next := time.Now().Add(backoff(attempts))
		outcome("retry", map[string]any{"status": StatusPending, "attempts": attempts, "last_error": clip(err.Error()), "locked_at": nil, "run_at": next})
		logger.Warn("delayed task failed, will retry", zlog.Str("next", next.UTC().Format(time.RFC3339)), zlog.Err(err))
	}
}

func (s *Scheduler) track(id int64, cancel context.CancelCauseFunc) {
	s.runMu.Lock()
	s.running[id] = cancel
	s.runMu.Unlock()
}

func (s *Scheduler) untrack(id int64) {
	s.runMu.Lock()
	delete(s.running, id)
	s.runMu.Unlock()
}

func (s *Scheduler) cancelLocal(id int64, cause error) {
	s.runMu.Lock()
	cancel := s.running[id]
	s.runMu.Unlock()
	if cancel != nil {
		cancel(cause)
	}
}

// heartbeat refreshes locked_at of a running task every delay.heartbeat so
// that no replica takes it for abandoned. A row that is no longer running
// (Abort from elsewhere) ends the run with cause ErrAborted. A failed
// refresh (database hiccup) is only logged: the run goes on and the row
// is reclaimed by someone only after stale().
func (s *Scheduler) heartbeat(ctx context.Context, t *Task, cancel context.CancelCauseFunc) (stop func()) {
	done := make(chan struct{})
	var once sync.Once
	go func() {
		ticker := time.NewTicker(s.cfg.heartbeat())
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				bctx, bcancel := bookkeeping(ctx)
				res := s.db.WithContext(bctx).Model(&Task{}).Where("id = ? AND status = ?", t.ID, StatusRunning).Update("locked_at", time.Now())
				bcancel()
				if res.Error != nil {
					zlog.Warn("delayed task heartbeat", zlog.Str("kind", t.Kind), zlog.Int("task_id", t.ID), zlog.Err(res.Error))
					continue
				}
				if res.RowsAffected == 0 {
					cancel(ErrAborted)
					return
				}
			}
		}
	}()
	return func() { once.Do(func() { close(done) }) }
}

func safely(ctx context.Context, h Handler, t *Task) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return h(ctx, t)
}

func clip(msg string) string {
	if len(msg) > 500 {
		return msg[:500]
	}
	return msg
}

// backoff: 1m, 2m, 4m ... capped at 1h.
func backoff(attempts int) time.Duration {
	d := time.Minute << uint(min(attempts-1, 10))
	if d > time.Hour {
		d = time.Hour
	}
	return d
}

// observeEvery spaces the gauge queries: three counts a second per replica
// would be most of a quiet service's query log.
const observeEvery = 15 * time.Second

func (s *Scheduler) observe(ctx context.Context) {
	kinds := s.all()
	var pending, failed int64
	s.db.WithContext(ctx).Model(&Task{}).Where("kind IN ? AND status = ?", kinds, StatusPending).Count(&pending)
	s.db.WithContext(ctx).Model(&Task{}).Where("kind IN ? AND status = ?", kinds, StatusFailed).Count(&failed)
	metricsx.DelayTasksPending.Set(float64(pending))
	metricsx.DelayTasksFailed.Set(float64(failed))
	var oldest struct{ RunAt *time.Time }
	s.db.WithContext(ctx).Model(&Task{}).Select("MIN(run_at) AS run_at").Where("kind IN ? AND status = ? AND run_at <= ?", kinds, StatusPending, time.Now()).Scan(&oldest)
	if oldest.RunAt == nil {
		metricsx.DelayTasksOverdue.Set(0)
	} else {
		metricsx.DelayTasksOverdue.Set(time.Since(*oldest.RunAt).Seconds())
	}
}

func (s *Scheduler) cleanup(ctx context.Context) {
	res := s.db.WithContext(ctx).Where("kind IN ? AND status IN ? AND updated_at < ?", s.all(), []string{StatusDone, StatusCancel}, time.Now().Add(-s.cfg.retention())).Limit(10000).Delete(&Task{})
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
	kinds := s.all()
	if len(kinds) == 0 {
		return 0, nil
	}
	var oldest struct{ RunAt *time.Time }
	err := s.db.WithContext(ctx).Model(&Task{}).Select("MIN(run_at) AS run_at").Where("kind IN ? AND status = ? AND run_at <= ?", kinds, StatusPending, time.Now()).Scan(&oldest).Error
	if err != nil || oldest.RunAt == nil {
		return 0, err
	}
	return time.Since(*oldest.RunAt), nil
}
