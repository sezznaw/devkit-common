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
	vdMsg = regexp.MustCompile(`invalid parameter:\s*(\w+)`)
	// (?s): the decoder's error can run over several lines
	bindMsg = regexp.MustCompile(`(?s)^bind (?:body|query|path|header|form) failed,\s*err=(.*)$`)
)

// bindReasonLimit is the most of a decoder's reason a client is shown.
const bindReasonLimit = 120

// FriendlyBind shortens hertz's binding and validation errors to what a
// client can act on: "invalid parameter: Username" becomes
// "username: 不合要求", a JSON error keeps its reason and position but not
// the excerpt of the body the decoder prints with it (bindReason).
func FriendlyBind(msg string) string {
	msg = strings.TrimSpace(msg)
	if m := vdMsg.FindStringSubmatch(msg); m != nil {
		return snake(m[1]) + ": 不合要求"
	}
	if m := bindMsg.FindStringSubmatch(msg); m != nil {
		return "请求格式错误: " + bindReason(m[1])
	}
	return msg
}

// bindReason keeps the first line of a decoder's error. The JSON decoder
// (sonic) prints the reason and the position, then a blank line and some 30
// bytes of the body on either side of the position:
//
//	Mismatch type int32 with value  "at index 86: mismatched type with value\n\n\t<body>\n\t....^..\n"
//
// and the body of a back-office write holds a secret or a one-time code as
// often as not: one field of the wrong type, and the value next to it went
// into the 1001 answer and from there into the audit log, where a msg is
// never redacted. The line break arrives as a real one or, inside a quoted
// error, as the two characters `\n`; either ends the reason. encoding/json
// prints no excerpt and passes unchanged.
//
// bindReason 只留解码器报错的第一行：sonic 的报错是「原因 + 位置」，空一行后跟出错位置前后
// 各约 30 字节的请求体原文。后台写接口的请求体里常有密钥、动态码，一个字段类型不对，旁边的值
// 就进了 1001 的 msg，再进审计（msg 不打码）。换行可能是真的换行，也可能是被引号转义成的两个
// 字符 \n，都在那里截断。
func bindReason(s string) string {
	for _, nl := range []string{"\n", `\n`} {
		if i := strings.Index(s, nl); i >= 0 {
			s = s[:i]
		}
	}
	s = strings.Trim(strings.TrimSpace(s), `"`)
	return cutUTF8(strings.TrimSpace(s), bindReasonLimit, "")
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
