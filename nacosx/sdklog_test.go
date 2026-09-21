package nacosx

import (
	"strings"
	"testing"

	sdklogger "github.com/nacos-group/nacos-sdk-go/v2/common/logger"
	"go.uber.org/zap/zapcore"

	"github.com/sezznaw/devkit-common/zlog"
	"github.com/sezznaw/devkit-common/zlog/zlogtest"
)

func TestSDKLogGoesThroughZlog(t *testing.T) {
	resetOutage(t)
	logs := zlogtest.Capture(t)
	sdklogger.SetLogger(&sdkLogger{l: zlog.With(zlog.Str("logger", "nacos-sdk")), min: zapcore.WarnLevel})

	sdklogger.Infof("content=%s", "password: x")                                                         // what the SDK does with every configuration
	sdklogger.Warnf("read Config Content failed. cause file doesn't exist, file path: %s", "x_failover") // with every read
	sdklogger.Warnf("out of date data received, old-t: %d", 7)
	sdklogger.Error("request failed\ngoroutine 1\n\tmain.go:1")

	recs := logs.Records()
	if len(recs) != 2 {
		t.Fatalf("below warn nothing is let through: %s", logs)
	}
	if recs[0].Level != "WARN" || recs[0].Msg != "out of date data received, old-t: 7" || recs[0].Fields["logger"] != "nacos-sdk" {
		t.Errorf("first record: %+v", recs[0])
	}
	if caller, _ := recs[0].Fields["caller"].(string); !strings.Contains(caller, "sdklog_test.go") {
		t.Errorf("caller must be the line that logged, got %q", caller)
	}
	if recs[1].Msg != "request failed" || !strings.Contains(recs[1].Fields["detail"].(string), "main.go:1") {
		t.Errorf("several lines stay one record: %+v", recs[1])
	}
}
