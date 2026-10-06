package jobx

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/sezznaw/devkit-common/zlog/zlogtest"
)

func ok(ctx context.Context) error { return nil }

func TestValidateAndList(t *testing.T) {
	jobs := []Job{{Name: "reconcile-bets", Schedule: "*/5 * * * *", Timeout: time.Minute, Run: ok, Description: "对账"}, {Name: "expire-bonus", Schedule: "0 * * * *", Run: ok}}
	var buf bytes.Buffer
	if err := WriteList(&buf, jobs); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"name": "reconcile-bets"`, `"schedule": "*/5 * * * *"`, `"timeout_seconds": 60`, `"timeout_seconds": 600`, `"description": "对账"`} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("list lacks %s:\n%s", want, buf.String())
		}
	}
	bad := []struct {
		job  Job
		want string
	}{
		{Job{Name: "Bad_Name", Schedule: "* * * * *", Run: ok}, "lowercase"},
		{Job{Name: "x", Schedule: "every day", Run: ok}, "schedule"},
		{Job{Name: "x", Schedule: "* * * * *"}, "no Run"},
	}
	for _, b := range bad {
		if err := Validate([]Job{b.job}); err == nil || !strings.Contains(err.Error(), b.want) {
			t.Errorf("%+v: %v", b.job, err)
		}
	}
	if err := Validate([]Job{{Name: "x", Schedule: "* * * * *", Run: ok}, {Name: "x", Schedule: "* * * * *", Run: ok}}); err == nil || !strings.Contains(err.Error(), "twice") {
		t.Errorf("duplicate: %v", err)
	}
}

func TestExecute(t *testing.T) {
	logs := zlogtest.Capture(t)
	srv := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: srv.Addr()})
	defer rdb.Close()
	ran := false
	jobs := []Job{
		{Name: "fine", Schedule: "* * * * *", Run: func(ctx context.Context) error { ran = true; return nil }},
		{Name: "fails", Schedule: "* * * * *", Run: func(ctx context.Context) error { return errors.New("db down") }},
		{Name: "panics", Schedule: "* * * * *", Run: func(ctx context.Context) error { panic("oops") }},
		{Name: "slow", Schedule: "* * * * *", Timeout: 50 * time.Millisecond, Run: func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }},
	}
	if err := Execute(context.Background(), "report", jobs, "fine", rdb); err != nil || !ran {
		t.Fatalf("fine: %v ran=%v", err, ran)
	}
	if !logs.Has("INFO", "job finished") {
		t.Error("a run ends with 'job finished'")
	}
	if strings.Contains(logs.String(), "00000000000000000000000000000000") {
		t.Error("with tracing off the trace_id is the run id, not zeros")
	}
	if n, _ := rdb.Exists(context.Background(), "job:report:fine").Result(); n != 0 {
		t.Error("the lock is released after the run")
	}
	if err := Execute(context.Background(), "report", jobs, "fails", rdb); err == nil || !strings.Contains(err.Error(), "db down") {
		t.Errorf("fails: %v", err)
	}
	if err := Execute(context.Background(), "report", jobs, "panics", rdb); err == nil || !strings.Contains(err.Error(), "panicked") {
		t.Errorf("panics: %v", err)
	}
	if err := Execute(context.Background(), "report", jobs, "slow", rdb); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Errorf("slow: %v", err)
	}
	if err := Execute(context.Background(), "report", jobs, "nope", rdb); err == nil || !strings.Contains(err.Error(), `no job "nope"`) {
		t.Errorf("unknown: %v", err)
	}
	if !logs.Has("ERROR", "job not found; nothing run") {
		t.Error("an unknown job name is logged, not only an exit code")
	}
	// The lock keeps a second run out.
	rdb.SetNX(context.Background(), "job:report:fine", "other", time.Minute)
	if err := Execute(context.Background(), "report", jobs, "fine", rdb); !errors.Is(err, ErrLocked) {
		t.Errorf("locked: %v", err)
	}
	// Without a locker it runs, with a warning.
	if err := Execute(context.Background(), "report", jobs, "fine", nil); err != nil || !logs.Has("WARN", "job runs without a lock: redis.enabled is false; the CronJob's concurrencyPolicy is the only guard") {
		t.Errorf("no locker: %v", err)
	}
}
