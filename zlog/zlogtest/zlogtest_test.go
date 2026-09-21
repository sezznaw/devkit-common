package zlogtest

import (
	"context"
	"errors"
	"testing"

	"github.com/sezznaw/devkit-common/zlog"
)

var pkgLog = zlog.With(zlog.Str("component", "repo"))

func TestCapture(t *testing.T) {
	t.Run("captures", func(t *testing.T) {
		logs := Capture(t)
		zlog.Debug("debug is captured too")
		pkgLog.Warn("login failed", zlog.Int("uid", 1001), zlog.Err(errors.New("bad password")))
		zlog.Ctx(zlog.CtxWith(context.Background(), zlog.Str("trace_id", "t1"))).Infof("saved %d", 7)

		if !logs.Has("WARN", "login failed") || !logs.Has("DEBUG", "captured") || !logs.Has("INFO", "saved 7") {
			t.Fatalf("missing records:\n%s", logs)
		}
		if logs.Has("ERROR", "login failed") || logs.Has("WARN", "no such message") {
			t.Error("Has matched a record that does not exist")
		}
		recs := logs.Records()
		if len(recs) != 3 {
			t.Fatalf("want 3 records, got %d", len(recs))
		}
		if f := recs[1].Fields; f["uid"] != float64(1001) || f["err"] != "bad password" || f["component"] != "repo" {
			t.Errorf("fields: %v", f)
		}
		if recs[2].Fields["trace_id"] != "t1" {
			t.Errorf("fields of the context: %v", recs[2].Fields)
		}
	})

	// The default logger is back once the test that captured has ended.
	t.Run("restores", func(t *testing.T) {
		logs := Capture(t)
		zlog.Info("second test")
		if n := len(logs.Records()); n != 1 {
			t.Errorf("records of the previous test leaked: %d", n)
		}
	})
}

func TestDiscard(t *testing.T) {
	Discard(t)
	zlog.Info("not shown")
	if zlog.Enabled(zlog.LevelInfo) {
		t.Error("Discard must switch info off")
	}
}
