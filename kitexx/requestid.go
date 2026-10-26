package kitexx

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/cloudwego/kitex/pkg/endpoint"

	"github.com/sezznaw/devkit-common/zlog"
)

// The request_id of the request being handled travels with ctx inside this
// process (never over the wire: each hop has its own field). A write a
// handler makes further down — an RPC whose request has a request_id — must
// carry that id or one derived from it, so a retry of the whole request
// repeats the same downstream ids and every service recognises the repeat.
// A fresh id (a UUID, a timestamp) would make the retry run downstream
// again: money moved twice, without any error.

type requestIDKey struct{}

// requestScope is the request being handled: its id, and the downstream
// calls already made with an id derived from it.
type requestScope struct {
	id   string
	mu   sync.Mutex
	made map[string]bool // "<service>.<method> <id>"
}

// WithRequestID makes id the request_id of the work ctx belongs to. The
// framework does it for an RPC whose request has a request_id and for a
// gateway request whose body has one; a job or an event consumer that makes
// write calls does it itself with an id that is the same on every rerun:
// kitexx.WithRequestID(ctx, "settle:"+matchID), kitexx.WithRequestID(ctx, "event:"+ev.ID).
func WithRequestID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, requestIDKey{}, &requestScope{id: id, made: map[string]bool{}})
}

// RequestID is the request_id of the request being handled, "" outside one.
// Pass it on to the write calls the handler makes.
func RequestID(ctx context.Context) string {
	if sc := scopeOf(ctx); sc != nil {
		return sc.id
	}
	return ""
}

// SubRequestID is the request_id for one of several write calls of the same
// method in one request (a stake and a fee, both a Debit): RequestID(ctx)
// plus ":" plus part, part naming the call ("fee", "stake", "item:3"). The
// same part on every retry, never a counter of attempts. "" outside a
// request, which the called service refuses (1003).
func SubRequestID(ctx context.Context, part string) string {
	id := RequestID(ctx)
	if id == "" {
		return ""
	}
	return id + ":" + part
}

func scopeOf(ctx context.Context) *requestScope {
	sc, _ := ctx.Value(requestIDKey{}).(*requestScope)
	return sc
}

// ClientRequestID checks the request_id of every outgoing call whose
// request has one, before it is sent:
//
//   - empty: refused here, with the method named (the service would answer 1003);
//   - inside a request with a request_id: it must be that id or derived from
//     it (SubRequestID); anything else (a new UUID) is refused, since a retry
//     of the request would run the call again;
//   - the same method with the same id twice in one request: the second is
//     refused, the service would only replay the first call's result.
//
// A call that fails may be made again with the same id: that is the retry
// idempotency is for. So may a call answered with a business code (1002 in
// progress, 5003 busy, ...): the service dropped the id. ClientOptions
// installs it.
func ClientRequestID() endpoint.Middleware {
	return func(next endpoint.Endpoint) endpoint.Endpoint {
		return func(ctx context.Context, req, resp any) error {
			r, ok := firstArgument(req).(requestWithID)
			if !ok {
				return next(ctx, req, resp)
			}
			service, method, _ := rpcNames(ctx)
			target := service + "." + method
			id := r.GetRequestId()
			if id == "" {
				return refuse(ctx, target, id, "takes a request_id and the call has none: pass the request_id of the request being handled (req.RequestId, kitexx.RequestID(ctx)), or kitexx.SubRequestID(ctx, part) when one request makes several such calls")
			}
			sc := scopeOf(ctx)
			if sc == nil {
				return next(ctx, req, resp)
			}
			if id != sc.id && !strings.HasPrefix(id, sc.id+":") {
				return refuse(ctx, target, id, fmt.Sprintf("got a request_id that is not derived from the request being handled (%s): a retry of this request would run the call again; pass kitexx.RequestID(ctx), or kitexx.SubRequestID(ctx, part)", sc.id))
			}
			key := target + " " + id
			sc.mu.Lock()
			twice := sc.made[key]
			sc.made[key] = true
			sc.mu.Unlock()
			if twice {
				return refuse(ctx, target, id, "is called a second time with the same request_id in one request: the service would return the first call's result and do nothing; give each call its own id with kitexx.SubRequestID(ctx, part)")
			}
			err := next(ctx, req, resp)
			if err != nil || bizFailed(ctx) {
				// not done: the same id may be tried again. Kitex returns nil
				// for a business answer and keeps it in the RPCInfo (bizCode)
				sc.mu.Lock()
				delete(sc.made, key)
				sc.mu.Unlock()
			}
			return err
		}
	}
}

func refuse(ctx context.Context, target, id, why string) error {
	err := fmt.Errorf("kitexx: %s %s", target, why)
	zlog.Ctx(ctx).Error("request_id refused before the call", zlog.Str("call", target), zlog.Str("request_id", id), zlog.Err(err))
	return err
}
