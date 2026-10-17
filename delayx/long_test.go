package delayx

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/sezznaw/devkit-common/config"
	"github.com/sezznaw/devkit-common/zlog/zlogtest"
)

// testDB is a fresh database on MYSQL_TEST_DSN_ROOT with the delayed_task
// table, dropped when the test ends; the test is skipped without a server.
func testDB(t *testing.T, name string) *gorm.DB {
	t.Helper()
	root := os.Getenv("MYSQL_TEST_DSN_ROOT")
	if root == "" {
		t.Skip("MYSQL_TEST_DSN_ROOT not set")
	}
	zlogtest.Discard(t)
	admin, err := sql.Open("mysql", root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Exec("DROP DATABASE IF EXISTS " + name); admin.Close() })
	for _, q := range []string{"DROP DATABASE IF EXISTS " + name, "CREATE DATABASE " + name} {
		if _, err := admin.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db, err := gorm.Open(mysql.Open(root+name+"?parseTime=true&charset=utf8mb4"), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(DDL).Error; err != nil {
		t.Fatal(err)
	}
	return db
}

// fast polls and heartbeats: stale() is 300ms.
func fast(long int) Config {
	return Config{PollInterval: config.Duration(50 * time.Millisecond), Heartbeat: config.Duration(100 * time.Millisecond), Batch: 10, MaxAttempts: 3, LongWorkers: long}
}

func row(t *testing.T, db *gorm.DB, id int64) Task {
	t.Helper()
	var r Task
	if err := db.First(&r, id).Error; err != nil {
		t.Fatal(err)
	}
	return r
}

// A long kind runs in its pool, one at a time with long_workers 1, and a
// short task due at the same moment does not wait behind it.
func TestLongPoolDoesNotBlockShortTasks(t *testing.T) {
	db := testDB(t, "delayx_long")
	s := New(db, fast(1), "report")
	release := make(chan struct{})
	var running, maxRunning, longRuns atomic.Int32
	s.Handle("export.run", func(ctx context.Context, task *Task) error {
		n := running.Add(1)
		for {
			m := maxRunning.Load()
			if n <= m || maxRunning.CompareAndSwap(m, n) {
				break
			}
		}
		defer running.Add(-1)
		longRuns.Add(1)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}, Long(), Timeout(10*time.Second))
	var quick atomic.Int32
	s.Handle("quick", func(ctx context.Context, task *Task) error { quick.Add(1); return nil })
	ctx := context.Background()
	for _, k := range []string{"E1", "E2"} {
		if _, err := s.Schedule(ctx, s.DB(), "export.run", k, time.Now(), nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Schedule(ctx, s.DB(), "quick", "Q", time.Now(), nil); err != nil {
		t.Fatal(err)
	}
	s.Start(ctx)
	defer s.Stop()

	// the short task runs while the first long one is still blocked
	waitFor(t, func() bool { return quick.Load() == 1 })
	waitFor(t, func() bool { return longRuns.Load() == 1 })
	time.Sleep(200 * time.Millisecond) // several polls: the second long task must not start
	if longRuns.Load() != 1 || maxRunning.Load() != 1 {
		t.Fatalf("long tasks started=%d at once=%d, want 1 and 1", longRuns.Load(), maxRunning.Load())
	}
	release <- struct{}{}
	waitFor(t, func() bool { return longRuns.Load() == 2 })
	release <- struct{}{}
	waitFor(t, func() bool {
		var n int64
		db.Model(&Task{}).Where("status = ?", StatusDone).Count(&n)
		return n == 3
	})
	if maxRunning.Load() != 1 {
		t.Fatalf("two long tasks ran at once")
	}
}

// A running task's heartbeat keeps its row from being taken for abandoned:
// a run longer than stale() is still one run.
func TestHeartbeatKeepsTheClaim(t *testing.T) {
	db := testDB(t, "delayx_beat")
	s := New(db, fast(2), "report")
	var runs atomic.Int32
	s.Handle("export.run", func(ctx context.Context, task *Task) error {
		runs.Add(1)
		select {
		case <-time.After(time.Second): // > 3 heartbeats
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}, Long(), Timeout(5*time.Second))
	ctx := context.Background()
	id, err := s.Schedule(ctx, s.DB(), "export.run", "E", time.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	s.Start(ctx)
	defer s.Stop()
	waitFor(t, func() bool { return runs.Load() == 1 })
	first := *row(t, db, id).LockedAt
	time.Sleep(350 * time.Millisecond)
	beat := *row(t, db, id).LockedAt
	if !beat.After(first) {
		t.Fatalf("locked_at not refreshed: %s then %s", first, beat)
	}
	waitFor(t, func() bool { return row(t, db, id).Status == StatusDone })
	if runs.Load() != 1 {
		t.Fatalf("task ran %d times", runs.Load())
	}
}

// Abort ends a run in this process at once with cause ErrAborted and
// leaves the row cancelled; a pending task of the same key is cancelled too.
func TestAbortLocal(t *testing.T) {
	db := testDB(t, "delayx_abort")
	s := New(db, fast(1), "report")
	var cause atomic.Value
	started := make(chan struct{}, 1)
	s.Handle("export.run", func(ctx context.Context, task *Task) error {
		started <- struct{}{}
		<-ctx.Done()
		cause.Store(context.Cause(ctx))
		return ctx.Err()
	}, Long(), Timeout(10*time.Second))
	ctx := context.Background()
	runID, _ := s.Schedule(ctx, s.DB(), "export.run", "E", time.Now(), nil)
	pendID, _ := s.Schedule(ctx, s.DB(), "export.run", "E", time.Now().Add(time.Hour), nil)
	s.Start(ctx)
	defer s.Stop()
	<-started
	n, err := s.Abort(ctx, nil, "export.run", "E")
	if err != nil || n != 2 {
		t.Fatalf("Abort: n=%d err=%v", n, err)
	}
	waitFor(t, func() bool { return s.Running() == 0 })
	if got, _ := cause.Load().(error); !errors.Is(got, ErrAborted) {
		t.Fatalf("handler saw cause %v, want ErrAborted", got)
	}
	r := row(t, db, runID)
	if r.Status != StatusCancel || r.LockedAt != nil || r.Attempts != 0 {
		t.Fatalf("aborted row: %+v", r)
	}
	if row(t, db, pendID).Status != StatusCancel {
		t.Fatal("pending task of the same key not cancelled")
	}
}

// A row cancelled by another replica (here: a plain UPDATE) ends the run
// here at the next heartbeat.
func TestAbortNoticedByHeartbeat(t *testing.T) {
	db := testDB(t, "delayx_abort_remote")
	s := New(db, fast(1), "report")
	var cause atomic.Value
	started := make(chan struct{}, 1)
	s.Handle("export.run", func(ctx context.Context, task *Task) error {
		started <- struct{}{}
		<-ctx.Done()
		cause.Store(context.Cause(ctx))
		return ctx.Err()
	}, Long(), Timeout(10*time.Second))
	ctx := context.Background()
	id, _ := s.Schedule(ctx, s.DB(), "export.run", "E", time.Now(), nil)
	s.Start(ctx)
	defer s.Stop()
	<-started
	db.Model(&Task{}).Where("id = ?", id).Update("status", StatusCancel)
	waitFor(t, func() bool { return s.Running() == 0 })
	if got, _ := cause.Load().(error); !errors.Is(got, ErrAborted) {
		t.Fatalf("handler saw cause %v, want ErrAborted", got)
	}
	if r := row(t, db, id); r.Status != StatusCancel {
		t.Fatalf("row: %+v", r)
	}
}

// Timeout(d) of a kind replaces delay.timeout: the run ends at d and what
// the handler returns decides (here: an error, so a retry).
func TestKindTimeout(t *testing.T) {
	db := testDB(t, "delayx_timeout")
	cfg := fast(1)
	cfg.Timeout = config.Duration(10 * time.Second) // the default the kind overrides
	s := New(db, cfg, "report")
	s.Handle("export.run", func(ctx context.Context, task *Task) error {
		<-ctx.Done()
		return ctx.Err()
	}, Long(), Timeout(150*time.Millisecond))
	ctx := context.Background()
	id, _ := s.Schedule(ctx, s.DB(), "export.run", "E", time.Now(), nil)
	s.Start(ctx)
	defer s.Stop()
	waitFor(t, func() bool { r := row(t, db, id); return r.Status == StatusPending && r.Attempts == 1 })
	if r := row(t, db, id); r.LastError != context.DeadlineExceeded.Error() {
		t.Fatalf("last_error=%q", r.LastError)
	}
}

// Stop ends a run in progress with cause ErrShutdown and puts the task back
// to pending to run again at once, without counting an attempt.
func TestStopRequeuesWithoutAttempt(t *testing.T) {
	db := testDB(t, "delayx_stop")
	s := New(db, fast(1), "report")
	var cause atomic.Value
	started := make(chan struct{}, 1)
	s.Handle("export.run", func(ctx context.Context, task *Task) error {
		started <- struct{}{}
		<-ctx.Done()
		cause.Store(context.Cause(ctx))
		return ctx.Err()
	}, Long(), Timeout(10*time.Second))
	ctx := context.Background()
	id, _ := s.Schedule(ctx, s.DB(), "export.run", "E", time.Now(), nil)
	s.Start(ctx)
	<-started
	s.Stop()
	if got, _ := cause.Load().(error); !errors.Is(got, ErrShutdown) {
		t.Fatalf("handler saw cause %v, want ErrShutdown", got)
	}
	r := row(t, db, id)
	if r.Status != StatusPending || r.Attempts != 0 || r.LockedAt != nil || r.RunAt.After(time.Now()) {
		t.Fatalf("after Stop: %+v", r)
	}
	if r.LastError != ErrShutdown.Error() {
		t.Fatalf("last_error=%q", r.LastError)
	}
}

// RunOnce dispatches long tasks to the pool and returns; a test waits for
// Running() to reach zero (testx.RunDue does).
func TestRunOnceDispatchesLong(t *testing.T) {
	db := testDB(t, "delayx_runonce")
	s := New(db, fast(2), "report")
	var runs atomic.Int32
	s.Handle("export.run", func(ctx context.Context, task *Task) error { runs.Add(1); return nil }, Long())
	ctx := context.Background()
	s.Schedule(ctx, s.DB(), "export.run", "A", time.Now(), nil)
	s.Schedule(ctx, s.DB(), "export.run", "B", time.Now(), nil)
	s.Schedule(ctx, s.DB(), "export.run", "C", time.Now(), nil) // no room: left for the next round
	n, err := s.RunOnce(ctx)
	if err != nil || n != 2 {
		t.Fatalf("RunOnce: n=%d err=%v", n, err)
	}
	waitFor(t, func() bool { return s.Running() == 0 })
	if runs.Load() != 2 {
		t.Fatalf("runs=%d", runs.Load())
	}
	if n, _ := s.RunOnce(ctx); n != 1 {
		t.Fatalf("second round claimed %d", n)
	}
	waitFor(t, func() bool { return s.Running() == 0 && runs.Load() == 3 })
}
