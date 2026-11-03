package hertzx

import (
	"context"
	"errors"
	"reflect"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
	"github.com/cloudwego/kitex/pkg/kerrors"

	"github.com/sezznaw/devkit-common/zlog"
)

// Envelope is what every gateway endpoint answers: {code, msg, data}. The
// IDL does not spell it out; a method returns its data struct and the
// handler ends with OK or Fail, which add the envelope once, here. HTTP
// status stays 200 for a business failure; the client looks at code. The
// exceptions are the codes the framework's own middleware answers with a
// status: 1004 and 1005 are HTTP 401 and 1007 is HTTP 403 wherever they come
// from (RequireLogin / RequirePermission, or an RPC: ser-auth's Refresh of a
// revoked session, a callers.rules refusal, CheckMemberUID, RequireRealm),
// so one code always comes with one status.
//
// 每个网关接口的响应壳。IDL 里不写它：方法返回数据结构体，handler 以 OK 或 Fail 结束，
// 壳在这里加一次。业务失败 HTTP 仍是 200，客户端看 code；例外是框架中间件自己带状态回的码：1004、1005
// 是 HTTP 401，1007 是 HTTP 403，不管来自中间件还是 RPC（ser-auth Refresh 遇到吊销的会话、callers.rules
// 拒绝、CheckMemberUID、RequireRealm），同一个码永远同一个状态。
type Envelope struct {
	Code int32  `json:"code"`
	Msg  string `json:"msg"`
	Data any    `json:"data,omitempty"`
}

// CodeOK is the code of a successful answer.
const CodeOK int32 = 0

// CodeUpstream is the code the gateway answers when a service it depends on
// failed for a reason that is not a business answer (down, timeout, bug).
const CodeUpstream int32 = 5001

// MsgUpstream is the message that goes with CodeUpstream.
const MsgUpstream = "服务暂不可用，请稍后重试"

// OK answers {code: 0, msg: "", data}. A method with nothing to return
// returns common.Empty in the IDL and the handler passes &common.Empty{}
// (or nil): both answer {code: 0, msg: ""} without data, so "nothing" looks
// the same everywhere.
func OK(c *app.RequestContext, data any) {
	if isEmptyStruct(data) {
		data = nil
	}
	c.JSON(consts.StatusOK, &Envelope{Code: CodeOK, Data: data})
}

// isEmptyStruct: nil, or a (pointer to a) struct without fields, such as
// the generated common.Empty.
func isEmptyStruct(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	for rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return true
		}
		rv = rv.Elem()
	}
	return rv.Kind() == reflect.Struct && rv.NumField() == 0
}

// Fail answers the error of an RPC call: a business answer
// (kerrors.NewBizStatusError in the service) goes through as its code and
// msg; anything else is logged with the request's trace and answered as
// 5001, because the client can do nothing with a transport error's text.
//
// 1004 / 1005 answer HTTP 401 like RequireLogin does and 1007 HTTP 403 like
// RequirePermission does; 1005 carries
// data.reason the same way: the reason the service put in the error's extra
// under "reason" (ser-auth passes authx.RevokeReason), "" when it gave none.
func Fail(ctx context.Context, c *app.RequestContext, err error) {
	code, msg := CodeOf(ctx, err)
	env := &Envelope{Code: code, Msg: msg}
	if code == CodeSessionRevoked {
		env.Data = map[string]string{"reason": bizExtra(err)["reason"]}
	}
	c.JSON(statusOf(code), env)
}

// FailCode answers a business failure the gateway decides itself (1004 /
// 1005 as HTTP 401 and 1007 as HTTP 403, like Fail).
func FailCode(c *app.RequestContext, code int32, msg string) {
	c.JSON(statusOf(code), &Envelope{Code: code, Msg: msg})
}

// statusOf is the HTTP status of a business code: 401 for the login state
// (1004, 1005), 403 for 1007, so an answer relayed from an RPC looks the
// same as one from RequireLogin / RequirePermission; 200 for everything else.
func statusOf(code int32) int {
	switch code {
	case CodeUnauthenticated, CodeSessionRevoked:
		return consts.StatusUnauthorized
	case CodeForbidden:
		return consts.StatusForbidden
	}
	return consts.StatusOK
}

// bizExtra is the extra map of a business error, nil when there is none.
func bizExtra(err error) map[string]string {
	var biz kerrors.BizStatusErrorIface
	if errors.As(err, &biz) {
		return biz.BizExtra()
	}
	if b, ok := kerrors.FromBizStatusError(err); ok {
		return b.BizExtra()
	}
	return nil
}

// CodeOf is Fail's translation without the answer: the business code and
// message of err, or 5001 after logging it. Handlers that need to decide
// something from the code use it.
func CodeOf(ctx context.Context, err error) (int32, string) {
	var biz kerrors.BizStatusErrorIface
	if errors.As(err, &biz) {
		return biz.BizStatusCode(), biz.BizMessage()
	}
	if b, ok := kerrors.FromBizStatusError(err); ok {
		return b.BizStatusCode(), b.BizMessage()
	}
	zlog.Ctx(ctx).Error("upstream call failed", zlog.Err(err))
	return CodeUpstream, MsgUpstream
}
