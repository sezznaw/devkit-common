package kitexx

import (
	"context"
	"strconv"

	"github.com/bytedance/gopkg/cloud/metainfo"

	"github.com/sezznaw/devkit-common/authx"
)

// The identity a gateway established travels to the RPC services as
// persistent metainfo (TTHeader), like the trace id: a handler anywhere in
// the call chain asks UID(ctx) and gets the member the request is for,
// without the field being in the request.
const (
	UIDKey   = "UID"
	RealmKey = "REALM"
	SIDKey   = "SID"
)

// WithIdentity puts id into ctx for this process and for every RPC made
// with ctx. The gateway's RequireLogin calls it.
func WithIdentity(ctx context.Context, id authx.Identity) context.Context {
	ctx = metainfo.WithPersistentValue(ctx, UIDKey, strconv.FormatInt(id.UID, 10))
	ctx = metainfo.WithPersistentValue(ctx, RealmKey, string(id.Realm))
	return metainfo.WithPersistentValue(ctx, SIDKey, id.SID)
}

// UID is the principal of the request (the logged-in member at the player
// gateway), or false on a request that came without a login (a public
// endpoint, a job, a consumer).
func UID(ctx context.Context) (int64, bool) {
	v, ok := metainfo.GetPersistentValue(ctx, UIDKey)
	if !ok {
		return 0, false
	}
	uid, err := strconv.ParseInt(v, 10, 64)
	return uid, err == nil && uid > 0
}

// Realm of the request's principal: member, admin or agent; "" without one.
func Realm(ctx context.Context) authx.Realm {
	v, _ := metainfo.GetPersistentValue(ctx, RealmKey)
	return authx.Realm(v)
}

// MustUID is UID for a handler whose endpoint requires a login (the
// gateway guarantees it): it panics when there is none, which means the
// endpoint was marked @public by mistake.
func MustUID(ctx context.Context) int64 {
	uid, ok := UID(ctx)
	if !ok {
		panic("kitexx: no login in the request context; the endpoint is public, or the gateway's RequireLogin is not installed")
	}
	return uid
}
