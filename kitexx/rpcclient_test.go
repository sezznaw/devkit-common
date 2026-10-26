package kitexx

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/kitex/client"
	"github.com/cloudwego/kitex/client/genericclient"
	"github.com/cloudwego/kitex/pkg/circuitbreak"
	"github.com/cloudwego/kitex/pkg/generic"
	"github.com/cloudwego/kitex/pkg/kerrors"
	"github.com/cloudwego/kitex/pkg/utils"

	"github.com/sezznaw/devkit-common/zlog/zlogtest"
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

// TestEveryClientHasItsOwnBreaker: the options of one rpcClientOptions call
// give every client they are applied to a circuit-breaker suite of its own.
// Before, all clients of the process shared one, which a client's Close or
// finalizer closed for all of them (TestClientsComeAndGoWithoutARace).
func TestEveryClientHasItsOwnBreaker(t *testing.T) {
	opts := rpcClientOptions(Config{})
	a, b := breakerSuiteOf(opts), breakerSuiteOf(opts)
	if a == nil || b == nil || a == b {
		t.Fatalf("two clients made from the same options, two suites: %p %p", a, b)
	}
	off := false
	if breakerSuiteOf(rpcClientOptions(Config{RPCClient: RPCClientConfig{Breaker: &off}})) != nil {
		t.Fatal("breaker: false, no suite")
	}
}

// breakerSuiteOf is the circuit-breaker suite opts give one client. The
// options are applied to a bare client.Options, where the timeout options
// panic (they need the RPC config Kitex makes in NewClient) and are skipped.
func breakerSuiteOf(opts []client.Option) *circuitbreak.CBSuite {
	var o client.Options
	for _, opt := range opts {
		func() {
			defer func() { _ = recover() }()
			opt.F(&o, &utils.Slice{})
		}()
	}
	return o.CBSuite
}

// TestClientsComeAndGoWithoutARace makes clients the way services do (one
// ClientOptions slice for several clients, as a gateway's deps; app.Setup
// again and again, as handler tests) while earlier ones are closed or
// collected. Kitex closes a client's circuit-breaker suite in Close and in
// the finalizer of a collected client (kitex v0.16.3 client/client.go);
// while every client of the process shared one suite, that Close raced with
// the next client setting the suite up, and it stopped the breakers of the
// clients still in use. The race shows only under go test -race.
func TestClientsComeAndGoWithoutARace(t *testing.T) {
	zlogtest.Discard(t)
	const svc = "answer-race"
	addr := startAnswer(t, svc)
	var cfg Config
	cfg.RegistryDisabled = true
	cfg.Service.Name = "caller"
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var closing sync.WaitGroup
	defer closing.Wait()
	for round := 0; round < 10; round++ {
		opts, err := ClientOptions(cfg)
		if err != nil {
			t.Fatal(err)
		}
		opts = append(opts, client.WithHostPorts(addr))
		// a Kitex client that is closed while the next round makes its clients
		g := answerGeneric(t)
		kc, err := client.NewClient(generic.ServiceInfoWithGeneric(g), append([]client.Option{client.WithGeneric(g), client.WithDestService(svc)}, opts...)...)
		if err != nil {
			t.Fatal(err)
		}
		// and two made from the same options and used, then left to the
		// garbage collector, whose finalizers close the Kitex clients behind them
		for i := 0; i < 2; i++ {
			cli, err := genericclient.NewClient(svc, answerGeneric(t), opts...)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := cli.GenericCall(ctx, "Answer", `{"what":"ok"}`); err != nil {
				t.Fatalf("round %d: %v", round, err)
			}
		}
		closing.Add(1)
		go func() {
			defer closing.Done()
			if err := kc.(interface{ Close() error }).Close(); err != nil {
				t.Error(err)
			}
		}()
		runtime.GC()
	}
}
