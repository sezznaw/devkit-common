package hertzx

import (
	"bytes"
	"context"
	"encoding/json"

	"github.com/cloudwego/hertz/pkg/app"

	"github.com/sezznaw/devkit-common/kitexx"
)

// RequestID puts the request_id of a request body into ctx
// (kitexx.WithRequestID), so the RPC clients check what the handler passes
// on: the same id or one derived from it (kitexx.ClientRequestID). A body
// without one (a read, a GET) leaves ctx as it is. New installs it.
func RequestID() app.HandlerFunc {
	return func(ctx context.Context, c *app.RequestContext) {
		body := bytes.TrimLeft(c.Request.Body(), " \t\r\n")
		if len(body) == 0 || body[0] != '{' {
			c.Next(ctx)
			return
		}
		var b struct {
			RequestID string `json:"request_id"`
		}
		if json.Unmarshal(body, &b) == nil && b.RequestID != "" {
			ctx = kitexx.WithRequestID(ctx, b.RequestID)
		}
		c.Next(ctx)
	}
}
