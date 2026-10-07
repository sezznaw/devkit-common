package webhookx

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sezznaw/devkit-common/httpx"
	"github.com/sezznaw/devkit-common/kitexx"
	"github.com/sezznaw/devkit-common/zlog/zlogtest"
)

func TestServerStartsWithTheRuntime(t *testing.T) {
	zlogtest.Capture(t)
	rt := runtimeWith(t, httpx.Callback{Verify: httpx.CallbackVerify{Type: "custom"}})
	rt.Config.Service.Name = "ser-vendor"
	rt.Config.Callbacks = kitexx.CallbacksConfig{Enabled: true, Addr: "127.0.0.1:0"}
	// Hertz needs a real port to report; pick one.
	port := freePort(t)
	rt.Config.Callbacks.Addr = "127.0.0.1:" + port
	h := Server(rt)
	if Server(rt) != h {
		t.Fatal("one engine per runtime")
	}
	Handle(rt, h, "pay", "/callbacks/pay/x", func(context.Context, *Event) error { return nil }, WithVerifier(func(*Event) error { return nil }))
	// rt.Run would call the starters; call them the way it does.
	if err := startAll(rt); err != nil {
		t.Fatal(err)
	}
	defer kitexx.RunShutdownHooks()
	var resp *http.Response
	var err error
	for i := 0; i < 50; i++ {
		resp, err = http.Post("http://127.0.0.1:"+port+"/callbacks/pay/x", "application/json", strings.NewReader("{}"))
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(b) != "ok" {
		t.Errorf("%d %s", resp.StatusCode, b)
	}
	rt2 := runtimeWith(t, httpx.Callback{Verify: httpx.CallbackVerify{Type: "custom"}})
	defer func() {
		if r := recover(); r == nil {
			t.Error("callbacks.enabled false panics")
		}
	}()
	Server(rt2)
}
