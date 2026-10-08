package hertzx

import (
	"regexp"
	"strings"

	"github.com/cloudwego/hertz/pkg/app"

	"github.com/sezznaw/devkit-common/kitexx"
)

// Bind reads the request into req (JSON body, query, path, as the IDL's
// api.* annotations say) and checks the IDL's api.vd rules. On failure it
// answers {code: 1001, msg: "<field>: <rule>"} itself and returns false,
// so a handler is:
//
//	var req ser_api.MemberUpdateUsernameReq
//	if !hertzx.Bind(c, &req) {
//		return
//	}
//
// The hz-generated stub answers a 400 text instead; devkit lint (rule
// bind) asks for this one, so every parameter error looks the same.
//
// Bind 读请求并按 IDL 的 api.vd 规则校验；失败时自己回 {code:1001, msg:"字段: 规则"} 并返回 false。
// hz 生成的空函数回的是 400 文本，lint 规则 bind 要求换成这个，所有参数错误长一个样。
func Bind(c *app.RequestContext, req any) bool {
	if err := c.BindAndValidate(req); err != nil {
		c.JSON(200, map[string]any{"code": kitexx.CodeBadParam, "msg": FriendlyBind(err.Error())})
		return false
	}
	return true
}

var (
	vdMsg   = regexp.MustCompile(`invalid parameter:\s*(\w+)`)
	bindMsg = regexp.MustCompile(`^bind (?:body|query|path|header|form) failed,\s*err=(.*)$`)
)

// FriendlyBind shortens hertz's binding and validation errors to what a
// client can act on: "invalid parameter: Username" becomes
// "username: 不合要求", a JSON type mismatch keeps its reason.
func FriendlyBind(msg string) string {
	msg = strings.TrimSpace(msg)
	if m := vdMsg.FindStringSubmatch(msg); m != nil {
		return snake(m[1]) + ": 不合要求"
	}
	if m := bindMsg.FindStringSubmatch(msg); m != nil {
		return "请求格式错误: " + strings.TrimSpace(m[1])
	}
	return msg
}

func snake(s string) string {
	var b strings.Builder
	for i, r := range s {
		if r >= 'A' && r <= 'Z' {
			if i > 0 && (s[i-1] < 'A' || s[i-1] > 'Z') {
				b.WriteByte('_')
			}
			b.WriteRune(r + ('a' - 'A'))
		} else {
			b.WriteRune(r)
		}
	}
	return strings.TrimSuffix(b.String(), "_")
}
