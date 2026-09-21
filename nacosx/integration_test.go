package nacosx

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nacos-group/nacos-sdk-go/v2/vo"

	"github.com/sezznaw/devkit-common/zlog/zlogtest"
)

// TestAgainstNacos runs against a real server, which is where the assumptions
// about the SDK that the fakes are built on are checked: that it keeps our
// logger, pushes changes, and calls back on Subscribe.
//
//	NACOS_ADDR=127.0.0.1:8848 go test ./nacosx/ -run TestAgainstNacos -v
//
// CI starts a Nacos for it; without NACOS_ADDR it is skipped.
func TestAgainstNacos(t *testing.T) {
	addr := os.Getenv("NACOS_ADDR")
	if addr == "" {
		t.Skip("NACOS_ADDR is not set")
	}
	logs := zlogtest.Capture(t)
	c, err := New(Config{Addrs: []string{addr}, CacheDir: t.TempDir(), SDKLogLevel: "info"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	eventually := func(t *testing.T, what string, cond func() bool) {
		t.Helper()
		for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
			if cond() {
				return
			}
		}
		t.Fatalf("timed out waiting for %s\n%s", what, logs)
	}

	t.Run("config", func(t *testing.T) {
		dataID := fmt.Sprintf("nacosx-test-%d.yaml", time.Now().UnixNano())
		cc, err := c.ConfigClient()
		if err != nil {
			t.Fatal(err)
		}
		publish := func(content string) {
			t.Helper()
			if ok, err := cc.PublishConfig(vo.ConfigParam{DataId: dataID, Group: "DEFAULT_GROUP", Content: content}); err != nil || !ok {
				t.Fatalf("publish: %v %v", ok, err)
			}
		}
		defer cc.DeleteConfig(vo.ConfigParam{DataId: dataID, Group: "DEFAULT_GROUP"})

		if _, err := Watch[dynamic](c, dataID); err == nil || !strings.Contains(err.Error(), "does not exist") {
			t.Fatalf("a configuration that does not exist: %v", err)
		}
		publish(v1)
		eventually(t, "the configuration to be readable", func() bool { s, _ := c.Get(dataID); return s != "" })
		v, err := Watch[dynamic](c, dataID)
		if err != nil || v.Get().Battle.MaxLevel != 60 {
			t.Fatalf("%v %+v", err, v)
		}
		publish(strings.Replace(v1, "60", "80", 1))
		eventually(t, "the change", func() bool { return v.Get().Battle.MaxLevel == 80 })
		publish("battle:\n  max_level: -1\n")
		eventually(t, "the rejection", func() bool { return logs.Has("ERROR", "config change rejected") })
		cc.DeleteConfig(vo.ConfigParam{DataId: dataID, Group: "DEFAULT_GROUP"})
		eventually(t, "the deletion", func() bool { return logs.Has("WARN", "config deleted or emptied") })
		if v.Get().Battle.MaxLevel != 80 {
			t.Errorf("the last good version must stay in effect: %+v", v.Get())
		}
		if n := strings.Count(logs.String(), `"msg":"config changed"`); n != 1 {
			t.Errorf("one change, one record; got %d", n)
		}
	})

	t.Run("naming", func(t *testing.T) {
		name := fmt.Sprintf("nacosx-test-%d", time.Now().UnixNano())
		first, err := c.Register(Registration{Service: name, Addr: "10.1.1.1:8888"})
		if err != nil {
			t.Fatal(err)
		}
		defer first()
		eventually(t, "the first instance", func() bool { l, _ := c.Instances(name); return len(l) == 1 })
		// From another client: one connection holds one instance per service.
		other, err := New(Config{Addrs: []string{addr}, CacheDir: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		defer other.Close()
		second, err := other.Register(Registration{Service: name, Addr: "10.1.1.2:8888", Weight: 20})
		if err != nil {
			t.Fatal(err)
		}
		eventually(t, "the second instance, pushed", func() bool { l, _ := c.Instances(name); return len(l) == 2 })
		if err := second(); err != nil {
			t.Fatal(err)
		}
		eventually(t, "the second instance to leave", func() bool { l, _ := c.Instances(name); return len(l) == 1 })
		if !logs.Has("INFO", "service discovered") || !logs.Has("INFO", "instances changed") {
			t.Errorf("records: %s", logs)
		}
	})

	t.Run("sdk log", func(t *testing.T) {
		if !strings.Contains(logs.String(), `"logger":"nacos-sdk"`) {
			t.Errorf("the SDK must log through zlog: %s", logs)
		}
		if _, err := os.Stat("log/nacos-sdk.log"); err == nil {
			t.Error("the SDK must not write a log file of its own")
		}
	})
}
