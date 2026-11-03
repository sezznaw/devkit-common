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
	type empty struct{}
	h.GET("/empty2", func(ctx context.Context, c *app.RequestContext) { OK(c, &empty{}) })
	h.GET("/biz", func(ctx context.Context, c *app.RequestContext) {
		Fail(ctx, c, kerrors.NewBizStatusError(2001, "member not found"))
	})
	h.GET("/down", func(ctx context.Context, c *app.RequestContext) { Fail(ctx, c, errors.New("dial tcp: refused")) })
	h.GET("/mine", func(ctx context.Context, c *app.RequestContext) { FailCode(c, 1001, "bad") })
	cases := map[string]string{
		"/ok":     `{"code":0,"msg":"","data":{"message":"pong"}}`,
		"/empty":  `{"code":0,"msg":""}`,
		"/empty2": `{"code":0,"msg":""}`,
		"/biz":    `{"code":2001,"msg":"member not found"}`,
		"/down":   `{"code":5001,"msg":"` + MsgUpstream + `"}`,
		"/mine":   `{"code":1001,"msg":"bad"}`,
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

// TY-108: a 1004 / 1005 relayed from an RPC (ser-auth's Refresh after the
// account was deleted) is HTTP 401 like RequireLogin's, 1005 with data.reason.
func TestLoginStateIs401(t *testing.T) {
	h := server.Default()
	h.GET("/revoked", func(ctx context.Context, c *app.RequestContext) {
		Fail(ctx, c, kerrors.NewBizStatusErrorWithExtra(1005, "会话已失效", map[string]string{"reason": "deleted"}))
	})
	h.GET("/revoked-bare", func(ctx context.Context, c *app.RequestContext) {
		Fail(ctx, c, kerrors.NewBizStatusError(1005, "会话已失效"))
	})
	h.GET("/unauth", func(ctx context.Context, c *app.RequestContext) {
		Fail(ctx, c, kerrors.NewBizStatusError(1004, "未登录"))
	})
	h.GET("/unauth-mine", func(ctx context.Context, c *app.RequestContext) { FailCode(c, 1004, "未登录") })
	h.GET("/refresh-invalid", func(ctx context.Context, c *app.RequestContext) {
		Fail(ctx, c, kerrors.NewBizStatusError(1006, "刷新令牌无效"))
	})
	cases := []struct {
		path   string
		status int
		want   string
	}{
		{"/revoked", 401, `{"code":1005,"msg":"会话已失效","data":{"reason":"deleted"}}`},
		{"/revoked-bare", 401, `{"code":1005,"msg":"会话已失效","data":{"reason":""}}`},
		{"/unauth", 401, `{"code":1004,"msg":"未登录"}`},
		{"/unauth-mine", 401, `{"code":1004,"msg":"未登录"}`},
		{"/refresh-invalid", 200, `{"code":1006,"msg":"刷新令牌无效"}`},
	}
	for _, tc := range cases {
		res := ut.PerformRequest(h.Engine, "GET", tc.path, nil).Result()
		if res.StatusCode() != tc.status {
			t.Fatalf("%s: status %d want %d", tc.path, res.StatusCode(), tc.status)
		}
		var got, exp any
		if err := json.Unmarshal(res.Body(), &got); err != nil {
			t.Fatalf("%s: %v: %s", tc.path, err, res.Body())
		}
		_ = json.Unmarshal([]byte(tc.want), &exp)
		if string(mustJSON(got)) != string(mustJSON(exp)) {
			t.Fatalf("%s: got %s want %s", tc.path, res.Body(), tc.want)
		}
	}
}
