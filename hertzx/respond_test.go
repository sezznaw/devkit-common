package hertzx

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/cloudwego/kitex/pkg/kerrors"
)

func TestEnvelope(t *testing.T) {
	h := server.Default()
	h.GET("/ok", func(ctx context.Context, c *app.RequestContext) { OK(c, map[string]any{"message": "pong"}) })
	h.GET("/empty", func(ctx context.Context, c *app.RequestContext) { OK(c, nil) })
	h.GET("/biz", func(ctx context.Context, c *app.RequestContext) { Fail(ctx, c, kerrors.NewBizStatusError(2001, "member not found")) })
	h.GET("/down", func(ctx context.Context, c *app.RequestContext) { Fail(ctx, c, errors.New("dial tcp: refused")) })
	h.GET("/mine", func(ctx context.Context, c *app.RequestContext) { FailCode(c, 1001, "bad") })
	cases := map[string]string{
		"/ok":    `{"code":0,"msg":"","data":{"message":"pong"}}`,
		"/empty": `{"code":0,"msg":""}`,
		"/biz":   `{"code":2001,"msg":"member not found"}`,
		"/down":  `{"code":5001,"msg":"` + MsgUpstream + `"}`,
		"/mine":  `{"code":1001,"msg":"bad"}`,
	}
	for path, want := range cases {
		w := ut.PerformRequest(h.Engine, "GET", path, nil)
		res := w.Result()
		if res.StatusCode() != 200 {
			t.Fatalf("%s: status %d", path, res.StatusCode())
		}
		var got, exp any
		if err := json.Unmarshal(res.Body(), &got); err != nil {
			t.Fatalf("%s: %v: %s", path, err, res.Body())
		}
		_ = json.Unmarshal([]byte(want), &exp)
		if string(mustJSON(got)) != string(mustJSON(exp)) {
			t.Fatalf("%s: got %s want %s", path, res.Body(), want)
		}
	}
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }
