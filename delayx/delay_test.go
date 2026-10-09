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

type cancelPayload struct {
	OrderNo string `json:"order_no"`
}

// TestScheduler runs the scheduler against a real MySQL (MYSQL_TEST_DSN_ROOT,
// as the outbox test): a task scheduled in a committed transaction runs once
// at its time with its payload; one in a rolled-back transaction never runs;
// a cancelled one never runs; a failing handler is retried and then marked
// failed; a stale running task (a dead process) is claimed again.
func TestScheduler(t *testing.T) {
	root := os.Getenv("MYSQL_TEST_DSN_ROOT")
	if root == "" {
		t.Skip("MYSQL_TEST_DSN_ROOT not set")
	}
	zlogtest.Discard(t)
	admin, err := sql.Open("mysql", root)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	for _, q := range []string{"DROP DATABASE IF EXISTS delayx_test", "CREATE DATABASE delayx_test"} {
		if _, err := admin.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	defer admin.Exec("DROP DATABASE IF EXISTS delayx_test")
	db, err := gorm.Open(mysql.Open(root+"delayx_test?parseTime=true&charset=utf8mb4"), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(DDL).Error; err != nil {
		t.Fatal(err)
	}

	s := New(db, Config{PollInterval: config.Duration(50 * time.Millisecond), Batch: 10, MaxAttempts: 2, Timeout: config.Duration(time.Second)}, "order")
	var ran atomic.Int32
	var gotOrder atomic.Value
	s.Handle("order.cancel-unpaid", func(ctx context.Context, task *Task) error {
		var p cancelPayload
		if err := task.Decode(&p); err != nil {
			return err
		}
		gotOrder.Store(p.OrderNo)
		ran.Add(1)
		return nil
	})
	var failures atomic.Int32
	s.Handle("always.fails", func(ctx context.Context, task *Task) error {
		failures.Add(1)
		return errors.New("boom")
	})
	ctx := context.Background()

	// committed: runs once, soon after run_at
	if err := db.Transaction(func(tx *gorm.DB) error {
		_, err := s.Schedule(ctx, tx, "order.cancel-unpaid", "A1", time.Now().Add(200*time.Millisecond), cancelPayload{OrderNo: "A1"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// rolled back: never exists
	_ = db.Transaction(func(tx *gorm.DB) error {
		if _, err := s.Schedule(ctx, tx, "order.cancel-unpaid", "ROLLED", time.Now(), cancelPayload{OrderNo: "ROLLED"}); err != nil {
			return err
		}
		return errors.New("rollback")
	})
	// cancelled before its time: never runs
	if _, err := s.Schedule(ctx, s.DB(), "order.cancel-unpaid", "PAID", time.Now().Add(300*time.Millisecond), cancelPayload{OrderNo: "PAID"}); err != nil {
		t.Fatal(err)
	}
	if n, err := s.Cancel(ctx, nil, "order.cancel-unpaid", "PAID"); err != nil || n != 1 {
		t.Fatalf("cancel: n=%d err=%v", n, err)
	}
	// failing: retried max_attempts times then failed (backoff shortened by editing run_at below)
	failID, err := s.Schedule(ctx, s.DB(), "always.fails", "F", time.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	// stale running (a process died mid-run): claimed again
	stale := time.Now().Add(-10 * time.Minute)
	db.Create(&Task{Kind: "order.cancel-unpaid", Key: "STALE", RunAt: stale, Payload: []byte(`{"order_no":"STALE"}`), Status: StatusRunning, LockedAt: &stale})

	s.Start(ctx)
	defer s.Stop()

	deadline := time.Now().Add(5 * time.Second)
	for ran.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if ran.Load() != 2 {
		t.Fatalf("expected the committed and the stale task to run, ran=%d", ran.Load())
	}
	var rows []Task
	db.Order("id").Find(&rows)
	byKey := map[string]Task{}
	for _, r := range rows {
		byKey[r.Key] = r
	}
	if byKey["A1"].Status != StatusDone || byKey["A1"].Attempts != 1 {
		t.Fatalf("A1: %+v", byKey["A1"])
	}
	if _, ok := byKey["ROLLED"]; ok {
		t.Fatal("rolled-back task exists")
	}
	if byKey["PAID"].Status != StatusCancel {
		t.Fatalf("PAID: %+v", byKey["PAID"])
	}
	if byKey["STALE"].Status != StatusDone {
		t.Fatalf("STALE: %+v", byKey["STALE"])
	}

	// first failure put it back to pending with a backoff; pull run_at forward for the test
	waitFor(t, func() bool {
		var f Task
		db.First(&f, failID)
		return f.Attempts == 1 && f.Status == StatusPending && f.LastError == "boom"
	})
	db.Model(&Task{}).Where("id = ?", failID).Update("run_at", time.Now())
	s.poke()
	waitFor(t, func() bool {
		var f Task
		db.First(&f, failID)
		return f.Status == StatusFailed
	})
	var f Task
	db.First(&f, failID)
	if f.Attempts != 2 || failures.Load() != 2 {
		t.Fatalf("failed task: attempts=%d handler calls=%d", f.Attempts, failures.Load())
	}
	if n, _ := s.FailedCount(ctx); n != 1 {
		t.Fatalf("FailedCount=%d", n)
	}
	if late, _ := s.Overdue(ctx); late != 0 {
		t.Fatalf("Overdue=%s, want 0", late)
	}
	if ran.Load() != 2 {
		t.Fatalf("a task ran twice: %d", ran.Load())
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in 5s")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestBackoff(t *testing.T) {
	for i, want := range []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute} {
		if got := backoff(i + 1); got != want {
			t.Fatalf("backoff(%d)=%s want %s", i+1, got, want)
		}
	}
	if backoff(50) != time.Hour {
		t.Fatal("cap")
	}
}

func TestScheduleNeedsTx(t *testing.T) {
	s := New(nil, Config{}, "x")
	if _, err := s.Schedule(context.Background(), nil, "k", "", time.Now(), nil); err == nil {
		t.Fatal("nil tx accepted")
	}
}
