package kitexx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cloudwego/kitex/pkg/endpoint"
	"github.com/cloudwego/kitex/pkg/kerrors"
	"github.com/cloudwego/kitex/pkg/serviceinfo"
	"github.com/redis/go-redis/v9"

	"github.com/sezznaw/devkit-common/zlog"
)

// Business codes the idempotency middleware answers with; registered in the
// idl repository's errors/common.thrift.
const (
	// CodeRequestInProgress: the same request_id is being processed right now.
	CodeRequestInProgress int32 = 1002
	// CodeRequestIDRequired: the request has a request_id field and left it empty.
	CodeRequestIDRequired int32 = 1003
	// CodeIdempotencyUnavailable: the store (Redis) is not there; the request
	// is refused rather than possibly executed twice.
	CodeIdempotencyUnavailable int32 = 5002
)

// IdempotencyTTL is how long a request_id is remembered: a retry later than
// this is a new request.
const IdempotencyTTL = 24 * time.Hour

// requestWithID is what a request struct with a `request_id` field looks like
// to the middleware: the getter Kitex generates for it. A method whose
// request has the field is idempotent, and only those.
type requestWithID interface{ GetRequestId() string }

// Idempotency makes a method whose request carries a request_id safe to
// retry: the first call with an id runs and its result is kept for
// IdempotencyTTL; a second call with the same id gets the kept result back
// without running again; a call while the first is still running is refused
// (1002). A request with the field left empty is refused (1003). It is what
// the API conventions ask of money and state-changing requests.
//
// The store is Runtime.Redis; a service that has such methods must enable
// Redis, or Runtime.Run refuses to start it (and, should one run anyway,
// every such request is refused with 5002): executing a transfer twice is
// worse than refusing it. Methods without the field pass through
// untouched. An error from the handler drops the id, so the caller can retry;
// so does a business error, which Kitex keeps in the RPCInfo (see bizFailed).
func (rt *Runtime) Idempotency() endpoint.Middleware {
	rdb := rt.Redis
	service := rt.Config.Service.Name
	return func(next endpoint.Endpoint) endpoint.Endpoint {
		return func(ctx context.Context, req, resp any) error {
			r, ok := firstArgument(req).(requestWithID)
			if !ok {
				return next(ctx, req, resp)
			}
			id := r.GetRequestId()
			if id == "" {
				return BizError(ctx, kerrors.NewBizStatusError(CodeRequestIDRequired, "request_id is required"))
			}
			// the writes this handler makes downstream carry this id (ClientRequestID)
			ctx = WithRequestID(ctx, id)
			if rdb == nil {
				zlog.Ctx(ctx).Error("idempotent request but redis.enabled is false", zlog.Str("request_id", id))
				return BizError(ctx, kerrors.NewBizStatusError(CodeIdempotencyUnavailable, "idempotency store is not configured"))
			}
			key := idempotencyKey(ctx, service, id)

			claimed, err := rdb.SetNX(ctx, key, pending, IdempotencyTTL).Result()
			if err != nil {
				zlog.Ctx(ctx).Error("idempotency store", zlog.Str("request_id", id), zlog.Err(err))
				return BizError(ctx, kerrors.NewBizStatusError(CodeIdempotencyUnavailable, "idempotency store unavailable"))
			}
			if !claimed {
				kept, err := rdb.Get(ctx, key).Bytes()
				if err != nil && !errors.Is(err, redis.Nil) {
					return BizError(ctx, kerrors.NewBizStatusError(CodeIdempotencyUnavailable, "idempotency store unavailable"))
				}
				if errors.Is(err, redis.Nil) || string(kept) == pending {
					// Nil: the first call failed between our SetNX and its Del;
					// treat it as in progress, the caller retries in a moment.
					return BizError(ctx, kerrors.NewBizStatusError(CodeRequestInProgress, "request with this request_id is in progress"))
				}
				if err := setSuccess(resp, kept); err != nil {
					zlog.Ctx(ctx).Error("idempotency: kept result does not fit", zlog.Str("request_id", id), zlog.Err(err))
					return BizError(ctx, kerrors.NewBizStatusError(CodeIdempotencyUnavailable, "idempotency store unavailable"))
				}
				zlog.Ctx(ctx).Info("replayed", zlog.Str("request_id", id))
				return nil
			}

			if err := next(ctx, req, resp); err != nil {
				rdb.Del(ctx, key)
				return err
			}
			// Kitex hands a business error from the handler to the client
			// through the RPCInfo (SetBizStatusErr) and returns nil to the
			// middlewares around the handler. Such a call did not succeed:
			// drop the id like any other error so a retry runs again, instead
			// of keeping an empty result that a retry would get as a success.
			if bizFailed(ctx) {
				rdb.Del(ctx, key)
				return nil
			}
			data, err := json.Marshal(success(resp))
			if err != nil {
				zlog.Ctx(ctx).Error("idempotency: cannot keep result", zlog.Str("request_id", id), zlog.Err(err))
				return nil // the call succeeded; a retry will run again, which is the lesser evil
			}
			if err := rdb.Set(ctx, key, data, IdempotencyTTL).Err(); err != nil {
				zlog.Ctx(ctx).Error("idempotency: cannot keep result", zlog.Str("request_id", id), zlog.Err(err))
			}
			return nil
		}
	}
}

// bizFailed reports whether this call was answered with a business error,
// which Kitex keeps in the RPCInfo instead of returning it (bizCode): by the
// handler, on the server; by the service called, on the client.
func bizFailed(ctx context.Context) bool {
	_, failed := bizCode(ctx, nil)
	return failed
}

// checkIdempotencyStore refuses to start a service that has idempotent
// methods (a request_id field) and no Redis: every call of those would be
// refused with 5002, found at the first click instead of at deploy time.
func (rt *Runtime) checkIdempotencyStore(svcs map[string]*serviceinfo.ServiceInfo) error {
	if rt.Redis != nil {
		return nil
	}
	methods := idempotentMethods(svcs)
	if len(methods) == 0 {
		return nil
	}
	return fmt.Errorf("kitexx: %s take a request_id (idempotent) and need Redis, which is off; set redis.enabled: true in conf/<env>.yaml", strings.Join(methods, ", "))
}

// idempotentMethods lists, sorted, the methods whose request has a request_id
// field: the ones the Idempotency middleware handles.
func idempotentMethods(svcs map[string]*serviceinfo.ServiceInfo) []string {
	var names []string
	for _, svc := range svcs {
		for name, mi := range svc.Methods {
			if mi == nil {
				continue
			}
			if _, ok := firstArgument(mi.NewArgs()).(requestWithID); ok {
				names = append(names, name)
			}
		}
	}
	sort.Strings(names)
	return names
}

// pending marks a request_id whose first call is running.
const pending = "\x00pending"

// firstArgument unwraps the request struct from the *Args Kitex passes to a
// server middleware; anything else is returned as it is.
func firstArgument(req any) any {
	if a, ok := req.(interface{ GetFirstArgument() any }); ok {
		return a.GetFirstArgument()
	}
	return req
}

// success reads the Success field of the *Result Kitex passes to a server
// middleware (the handler's response), nil when there is none.
func success(resp any) any {
	if r, ok := resp.(interface{ GetResult() any }); ok {
		return r.GetResult()
	}
	return nil
}

// setSuccess fills the *Result with a kept response: a new value of the
// Success field's type, decoded from data.
func setSuccess(resp any, data []byte) error {
	rv := reflect.ValueOf(resp)
	if rv.Kind() != reflect.Pointer || rv.Elem().Kind() != reflect.Struct {
		return fmt.Errorf("result is %T, not a pointer to a struct", resp)
	}
	f := rv.Elem().FieldByName("Success")
	if !f.IsValid() || f.Kind() != reflect.Pointer {
		return fmt.Errorf("result %T has no Success pointer field", resp)
	}
	v := reflect.New(f.Type().Elem())
	if err := json.Unmarshal(data, v.Interface()); err != nil {
		return err
	}
	f.Set(v)
	return nil
}

// idempotencyKey is the Redis key a request's answer is kept under. A
// login's request is its own: the same request_id from another login must
// not be answered with this one's result (nor skip the handler for it), so
// the key carries the login's uid. Requests without a login (jobs,
// consumers, service calls) share the plain key.
// idempotencyKey 是请求结果在 Redis 里的键。带登录者的请求按登录者分键：别的登录者拿同一个 request_id 不能拿到这个人的结果、
// 也不能跳过 handler。没有登录者的请求（任务、消费者、服务间调用）用原来的键。
func idempotencyKey(ctx context.Context, service, requestID string) string {
	_, method, _ := rpcNames(ctx)
	key := fmt.Sprintf("idem:%s:%s:%s", service, method, requestID)
	if uid, ok := UID(ctx); ok {
		key += ":u" + strconv.FormatInt(uid, 10)
	}
	return key
}
