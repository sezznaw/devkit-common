package hertzx

import (
	"context"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/ut"

	"github.com/sezznaw/devkit-common/zlog/zlogtest"
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
	// the decoder's excerpt of the body is cut off: what remains is the reason and the position
	for _, c := range []struct{ in, want string }{
		// sonic, as the error prints it: reason, blank line, the body around the position, a caret;
		// the whole of it quoted (the line breaks are the two characters \n) ...
		{`bind body failed, err="Syntax error at index 109: invalid char\n\n\tsecret\":\"` + bindMarker + `\",}\n\t....^..\n"`,
			"请求格式错误: Syntax error at index 109: invalid char"},
		// ... or as it is
		{"bind body failed, err=Syntax error at index 109: invalid char\n\n\tsecret\":\"" + bindMarker + "\",}\n\t....^..\n",
			"请求格式错误: Syntax error at index 109: invalid char"},
		{`bind body failed, err=Mismatch type int32 with value  "at index 86: mismatched type with value\n\n\t` + bindMarker + `\"}\n"`,
			`请求格式错误: Mismatch type int32 with value  "at index 86: mismatched type with value`},
		// encoding/json quotes no body: unchanged
		{"bind body failed, err=json: cannot unmarshal string into Go struct field updateReq.age of type int",
			"请求格式错误: json: cannot unmarshal string into Go struct field updateReq.age of type int"},
	} {
		if got := FriendlyBind(c.in); got != c.want {
			t.Errorf("FriendlyBind(%q)\n got %q\nwant %q", c.in, got, c.want)
		}
	}
	if got := FriendlyBind("bind body failed, err=" + strings.Repeat("原因", 100)); len(got) > len("请求格式错误: ")+bindReasonLimit || !utf8.ValidString(got) {
		t.Errorf("a long reason is cut to %d bytes at a character boundary, got %d bytes", bindReasonLimit, len(got))
	}
}

// bindMarker is in every secret-looking value of the bodies below: it must
// reach neither the answer nor the audit log. An invented string.
const bindMarker = "INVENTED-SECRET-0001"

type providerReq struct {
	RequestID string `json:"request_id"`
	Name      string `json:"name"`
	Secret    string `json:"secret"`
	OtpCode   string `json:"otp_code"`
	Status    int32  `json:"status"`
}

// TestBindErrorDoesNotEchoBody: a body Bind cannot read (broken JSON, a
// field of the wrong type) is answered 1001 without any of its content, and
// the audit entry of the request holds none of it either: not as the
// request (a broken body used to be parsed as a form, its fragments as
// keys), not in the answer's msg (the decoder quotes the body around the
// position of the error).
func TestBindErrorDoesNotEchoBody(t *testing.T) {
	zlogtest.Capture(t)
	var mu sync.Mutex
	var got []AuditEntry
	h := server.New(server.WithDisablePrintRoute(true))
	Audit(h, []byte(auditSpec), func(ctx context.Context, e AuditEntry) {
		mu.Lock()
		got = append(got, e)
		mu.Unlock()
	})
	h.POST("/v1/admin/create", func(ctx context.Context, c *app.RequestContext) {
		var req providerReq
		if !Bind(c, &req) {
			return
		}
		c.JSON(200, map[string]any{"code": 0})
	})
	for name, body := range map[string]string{
		"trailing comma, a value with =": `{"request_id":"r1","name":"A=B","secret":"` + bindMarker + `","status":1,"otp_code":"` + bindMarker + `",}`,
		"missing comma":                  `{"request_id":"r1","name":"A","secret":"` + bindMarker + `" "status":1}`,
		"wrong type":                     `{"request_id":"r1","name":"A","secret":"` + bindMarker + `","status":"x"}`,
		"wrong type, secret last":        `{"request_id":"r1","name":"A","status":"x","otp_code":"` + bindMarker + `"}`,
		"cut short, base64 padding":      `{"request_id":"r1","name":"A","secret":"` + bindMarker + `==","status":1`,
	} {
		for _, contentType := range []string{"application/json", "application/x-www-form-urlencoded"} {
			mu.Lock()
			got = nil
			mu.Unlock()
			w := ut.PerformRequest(h.Engine, "POST", "/v1/admin/create", &ut.Body{Body: strings.NewReader(body), Len: len(body)}, ut.Header{Key: "Content-Type", Value: contentType})
			answer := w.Body.String()
			mu.Lock()
			entries := append([]AuditEntry(nil), got...)
			mu.Unlock()
			if len(entries) != 1 {
				t.Fatalf("%s (%s): %d audit entries", name, contentType, len(entries))
			}
			e := entries[0]
			if contentType == "application/json" && e.Code != 1001 {
				t.Errorf("%s: code %d, want 1001 (%s)", name, e.Code, answer)
			}
			for where, text := range map[string]string{"the answer": answer, "audit request": e.Request, "audit response": e.Response, "audit msg": e.Msg} {
				if strings.Contains(text, bindMarker) || strings.Contains(text, bindMarker[len(bindMarker)-9:]) {
					t.Errorf("%s (%s): the body's secret is in %s: %s", name, contentType, where, text)
				}
			}
		}
	}
}
