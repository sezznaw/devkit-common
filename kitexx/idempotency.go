package kitexx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/cloudwego/kitex/pkg/endpoint"
	"github.com/cloudwego/kitex/pkg/kerrors"
	"github.com/redis/go-redis/v9"

	"github.com/sezznaw/devkit-common/zlog"
)

// Business codes the idempotency middleware answers with; registered in the
// idl repository's errors.md.
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
// Redis, or every such request is refused (5002): executing a transfer twice
// is worse than refusing it. Methods without the field pass through
// untouched. An error from the handler drops the id, so the caller can retry.
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
				return kerrors.NewBizStatusError(CodeRequestIDRequired, "request_id is required")
			}
			if rdb == nil {
				zlog.Ctx(ctx).Error("idempotent request but redis.enabled is false", zlog.Str("request_id", id))
				return kerrors.NewBizStatusError(CodeIdempotencyUnavailable, "idempotency store is not configured")
			}
			_, method, _ := rpcNames(ctx)
			key := fmt.Sprintf("idem:%s:%s:%s", service, method, id)

			claimed, err := rdb.SetNX(ctx, key, pending, IdempotencyTTL).Result()
			if err != nil {
				zlog.Ctx(ctx).Error("idempotency store", zlog.Str("request_id", id), zlog.Err(err))
				return kerrors.NewBizStatusError(CodeIdempotencyUnavailable, "idempotency store unavailable")
			}
			if !claimed {
				kept, err := rdb.Get(ctx, key).Bytes()
				if err != nil && !errors.Is(err, redis.Nil) {
					return kerrors.NewBizStatusError(CodeIdempotencyUnavailable, "idempotency store unavailable")
				}
				if errors.Is(err, redis.Nil) || string(kept) == pending {
					// Nil: the first call failed between our SetNX and its Del;
					// treat it as in progress, the caller retries in a moment.
					return kerrors.NewBizStatusError(CodeRequestInProgress, "request with this request_id is in progress")
				}
				if err := setSuccess(resp, kept); err != nil {
					zlog.Ctx(ctx).Error("idempotency: kept result does not fit", zlog.Str("request_id", id), zlog.Err(err))
					return kerrors.NewBizStatusError(CodeIdempotencyUnavailable, "idempotency store unavailable")
				}
				zlog.Ctx(ctx).Info("replayed", zlog.Str("request_id", id))
				return nil
			}

			if err := next(ctx, req, resp); err != nil {
				rdb.Del(ctx, key)
				return err
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
