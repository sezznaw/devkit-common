package hertzx

import (
	"context"
	"regexp"
	"strings"

	"github.com/cloudwego/hertz/pkg/app"

	"github.com/sezznaw/devkit-common/kitexx"
	"github.com/sezznaw/devkit-common/zlog"
)

// The request headers a client describes itself with. The device type is
// the client's platform as client.device.types lists it; the device id is
// an identifier the client made up for itself. Both are properties of the
// caller, not of any one operation, which is why they are headers and not
// request-body fields: one place reads them, every service gets them.
//
// 客户端描述自己的两个请求头。设备类型是客户端平台（client.device.types 里列的值），
// 设备号是客户端自己生成的标识。它们是调用方的属性而不是某个操作的参数，所以是请求头
// 而不是请求体字段：一处读取，每个服务都拿得到。
const (
	DeviceHeader   = "X-Device-Type"
	DeviceIDHeader = "X-Device-Id"
)

// deviceCtxKey is where the middleware leaves the device type for the
// request log, which runs before it and logs after it.
const deviceCtxKey = "hertzx.device"

// deviceIDRe is what a device id may look like; anything else is refused
// so that a free-form value never reaches logs or a database column.
var deviceIDRe = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

// Client reads what the request says about its client and puts it into the
// context as persistent metainfo (kitexx.Device, DeviceID, ClientIP in this
// process and in every RPC made with ctx). The IP is always taken
// (c.ClientIP: the proxy's X-Forwarded-For, else the connection). With
// client.device.types set, X-Device-Type must be one of them (matched
// ignoring case, stored as listed) and, with client.device.required, must be
// present; otherwise the answer is the envelope with code 1001, like a body
// that fails validation. X-Device-Id is optional and must be 1 to 128 of
// [A-Za-z0-9._:-]. The framework's own paths (/ping, /docs, /openapi.*) are
// never refused. The request log gets a `device` field.
func Client(cfg kitexx.ClientConfig) app.HandlerFunc {
	d := cfg.Device
	return func(ctx context.Context, c *app.RequestContext) {
		cl := kitexx.Client{IP: c.ClientIP()}
		framework := isFrameworkPath(string(c.Path()))
		if d.Enabled() {
			raw := strings.TrimSpace(string(c.GetHeader(DeviceHeader)))
			switch dev, ok := d.Canonical(raw); {
			case ok:
				cl.Device = dev
			case raw != "" && !framework:
				refuseParam(c, DeviceHeader+": 不合要求")
				return
			case d.Required && !framework:
				refuseParam(c, DeviceHeader+": 必填")
				return
			}
		}
		if id := strings.TrimSpace(string(c.GetHeader(DeviceIDHeader))); id != "" {
			if !deviceIDRe.MatchString(id) {
				if !framework {
					refuseParam(c, DeviceIDHeader+": 不合要求")
					return
				}
			} else {
				cl.DeviceID = id
			}
		}
		ctx = kitexx.WithClient(ctx, cl)
		if cl.Device != "" {
			c.Set(deviceCtxKey, cl.Device)
			ctx = zlog.CtxWith(ctx, zlog.Str("device", cl.Device))
		}
		c.Next(ctx)
	}
}

// refuseParam answers like Bind does for a body that fails validation: the
// envelope with code 1001 and the name of what is wrong.
func refuseParam(c *app.RequestContext, msg string) {
	c.AbortWithStatusJSON(200, map[string]any{"code": kitexx.CodeBadParam, "msg": msg})
}
