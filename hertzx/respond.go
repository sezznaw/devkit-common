package hertzx

import (
	"context"
	"errors"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
	"github.com/cloudwego/kitex/pkg/kerrors"

	"github.com/sezznaw/devkit-common/zlog"
)

// Envelope is what every gateway endpoint answers: {code, msg, data}. The
// IDL does not spell it out; a method returns its data struct and the
// handler ends with OK or Fail, which add the envelope once, here. HTTP
// status stays 200 for a business failure; the client looks at code.
//
// 每个网关接口的响应壳。IDL 里不写它：方法返回数据结构体，handler 以 OK 或 Fail 结束，
// 壳在这里加一次。业务失败 HTTP 仍是 200，客户端看 code。
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

// OK answers {code: 0, msg: "", data}; a nil data (or a method with nothing
// to return) answers {code: 0, msg: ""}.
func OK(c *app.RequestContext, data any) {
	c.JSON(consts.StatusOK, &Envelope{Code: CodeOK, Data: data})
}

// Fail answers the error of an RPC call: a business answer
// (kerrors.NewBizStatusError in the service) goes through as its code and
// msg; anything else is logged with the request's trace and answered as
// 5001, because the client can do nothing with a transport error's text.
func Fail(ctx context.Context, c *app.RequestContext, err error) {
	code, msg := CodeOf(ctx, err)
	c.JSON(consts.StatusOK, &Envelope{Code: code, Msg: msg})
}

// FailCode answers a business failure the gateway decides itself.
func FailCode(c *app.RequestContext, code int32, msg string) {
	c.JSON(consts.StatusOK, &Envelope{Code: code, Msg: msg})
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
