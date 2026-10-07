package kitexx

import (
	"errors"
	"testing"

	"github.com/cloudwego/kitex/pkg/kerrors"
)

func TestRetryOnlyUnsent(t *testing.T) {
	yes := []error{kerrors.ErrGetConnection, kerrors.ErrNoConnection, kerrors.ErrNoDestAddress, kerrors.ErrNoDestService}
	for _, e := range yes {
		if !retryOnlyUnsent(e, nil) {
			t.Errorf("%v should be retried", e)
		}
	}
	no := []error{kerrors.ErrRPCTimeout, kerrors.ErrRemoteOrNetwork, kerrors.NewBizStatusError(2001, "x"), errors.New("boom"), kerrors.ErrCircuitBreak}
	for _, e := range no {
		if retryOnlyUnsent(e, nil) {
			t.Errorf("%v must not be retried: the call may have run", e)
		}
	}
	var c RPCClientConfig
	if c.retries() != 1 || !c.breaker() {
		t.Error("defaults: one retry, breaker on")
	}
	zero, off := 0, false
	c = RPCClientConfig{Retries: &zero, Breaker: &off}
	if len(rpcClientOptions(Config{RPCClient: c})) != 2 {
		t.Error("retries 0 and breaker false leave only the two timeouts")
	}
}
