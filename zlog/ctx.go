package zlog

import "context"

type ctxKey struct{}

// Ctx returns the logger of a request: the one put into ctx by CtxWith or
// NewCtx, which carries the fields of the request (trace_id, method, ...), or
// the default logger when there is none.
//
//	zlog.Ctx(ctx).Info("saved", zlog.Int("uid", uid))
func Ctx(ctx context.Context) *Logger {
	if ctx != nil {
		if l, ok := ctx.Value(ctxKey{}).(*Logger); ok {
			return l
		}
	}
	return followStd
}

// CtxWith returns a context whose logger also writes the fields: everything
// logged with zlog.Ctx further down the call chain carries them.
//
//	ctx = zlog.CtxWith(ctx, zlog.Int("uid", uid))
func CtxWith(ctx context.Context, fields ...Field) context.Context {
	return NewCtx(ctx, Ctx(ctx).With(fields...))
}

// NewCtx returns a context with l as its logger.
func NewCtx(ctx context.Context, l *Logger) context.Context {
	return context.WithValue(ctx, ctxKey{}, l)
}
