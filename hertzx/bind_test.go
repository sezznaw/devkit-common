package hertzx

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/ut"
)

type updateReq struct {
	RequestID string `json:"request_id" vd:"len($)>0"`
	Username  string `json:"username" vd:"len($)>=3 && len($)<=32"`
	Age       int    `json:"age" vd:"$>=18"`
}

func TestBind(t *testing.T) {
	h := server.New(server.WithDisablePrintRoute(true))
	h.POST("/u", func(ctx context.Context, c *app.RequestContext) {
		var req updateReq
		if !Bind(c, &req) {
			return
		}
		c.JSON(200, map[string]any{"code": 0, "username": req.Username})
	})
	post := func(body string) *ut.ResponseRecorder {
		return ut.PerformRequest(h.Engine, "POST", "/u", &ut.Body{Body: strings.NewReader(body), Len: len(body)}, ut.Header{Key: "Content-Type", Value: "application/json"})
	}
	if w := post(`{"request_id":"r1","username":"alice","age":20}`); w.Code != 200 || !strings.Contains(w.Body.String(), `"code":0`) {
		t.Fatalf("valid: %d %s", w.Code, w.Body.String())
	}
	if w := post(`{"request_id":"r1","username":"al","age":20}`); w.Code != 200 || !strings.Contains(w.Body.String(), `"code":1001`) || !strings.Contains(w.Body.String(), "username") {
		t.Fatalf("too short: %d %s", w.Code, w.Body.String())
	}
	if w := post(`{"username":"alice","age":20}`); !strings.Contains(w.Body.String(), "request_id") {
		t.Fatalf("missing request_id: %s", w.Body.String())
	}
	if w := post(`{"request_id":"r1","username":"alice","age":"x"}`); !strings.Contains(w.Body.String(), `"code":1001`) || !strings.Contains(w.Body.String(), "请求格式错误") {
		t.Fatalf("type mismatch: %s", w.Body.String())
	}
}

func TestFriendlyBind(t *testing.T) {
	if got := FriendlyBind("invalid parameter: NewPassword"); got != "new_password: 不合要求" {
		t.Error(got)
	}
	if got := FriendlyBind("bind body failed, err=Mismatch type string with value number"); got != "请求格式错误: Mismatch type string with value number" {
		t.Error(got)
	}
}
