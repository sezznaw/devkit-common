package kitexx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/kitex/client"
	"github.com/cloudwego/kitex/client/genericclient"
	"github.com/cloudwego/kitex/pkg/generic"
	"github.com/cloudwego/kitex/pkg/kerrors"
	"github.com/cloudwego/kitex/pkg/rpcinfo"
	"github.com/cloudwego/kitex/pkg/transmeta"
	"github.com/cloudwego/kitex/server"
	"github.com/cloudwego/kitex/server/genericserver"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/sezznaw/devkit-common/metricsx"
	"github.com/sezznaw/devkit-common/zlog/zlogtest"
)

// The tests of this file run real Kitex, a server and a client over TTHeader
// on a loopback port, because what a middleware gets to see of a call is
// decided by Kitex, not by us: a business error is moved into the RPCInfo
// (invokeHandleEndpoint on the server, the TTHeader meta handler on the
// client) and the middlewares get nil. A test that fakes that is only as
// right as our idea of Kitex; these check the idea. Kitex's generic JSON
// call stands in for generated code.

// answerIDL is the service the tests call: "ok" is answered, "biz" with
// business code 2001 (what a handler returns with errcode.Err), anything
// else with a plain error.
const answerIDL = `namespace go answer
struct AnswerReq { 1: string what }
struct AnswerResp { 1: string said }
service Answer { AnswerResp Answer(1: AnswerReq req) }
`

type answerHandler struct{}

func (*answerHandler) GenericCall(_ context.Context, _ string, request any) (any, error) {
	var req struct {
		What string `json:"what"`
	}
	if s, ok := request.(string); !ok || json.Unmarshal([]byte(s), &req) != nil {
		return nil, errors.New("not the request of Answer")
	}
	switch req.What {
	case "ok":
		return `{"said":"ok"}`, nil
	case "biz":
		return nil, kerrors.NewBizStatusError(2001, "nothing here")
	}
	return nil, errors.New("boom")
}

func answerGeneric(t testing.TB) generic.Generic {
	t.Helper()
	p, err := generic.NewThriftContentProvider(answerIDL, nil)
	if err != nil {
		t.Fatal(err)
	}
	g, err := generic.JSONThriftGeneric(p)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// startAnswer runs the Answer service as service on a loopback port, with
// opts after the TTHeader meta handler, and returns its address.
func startAnswer(t testing.TB, service string, opts ...server.Option) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	svr := genericserver.NewServer(&answerHandler{}, answerGeneric(t), append([]server.Option{
		server.WithListener(ln),
		server.WithServerBasicInfo(&rpcinfo.EndpointBasicInfo{ServiceName: service}),
		server.WithMetaHandler(transmeta.ServerTTHeaderHandler),
		server.WithExitWaitTime(100 * time.Millisecond),
	}, opts...)...)
	stopped := make(chan struct{})
	go func() { defer close(stopped); _ = svr.Run() }()
	t.Cleanup(func() { _ = svr.Stop(); <-stopped })
	return ln.Addr().String()
}

// answerClient is a client of service at addr made the way a service makes
// its clients (ClientOptions), naming itself caller.
func answerClient(t testing.TB, addr, service, caller string) genericclient.Client {
	t.Helper()
	var cfg Config
	cfg.RegistryDisabled = true
	cfg.Service.Name = caller
	opts, err := ClientOptions(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cli, err := genericclient.NewClient(service, answerGeneric(t), append(opts, client.WithHostPorts(addr))...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	return cli
}

// counts reads the counters of service's Answer method for the codes given.
func counts(vec *prometheus.CounterVec, service string, codes ...string) map[string]float64 {
	out := map[string]float64{}
	for _, c := range codes {
		out[c] = testutil.ToFloat64(vec.WithLabelValues(service, "Answer", c))
	}
	return out
}

// TestBusinessCodesThroughRealKitex: the server has the framework's
// metrics, request log and callers middlewares, the client is made with
// ClientOptions. A business code from the handler (2001) and one from a
// middleware (callers: 1007) are counted and logged as those codes on both
// sides; before, Kitex's nil made them "ok" and an INFO line without a code.
func TestBusinessCodesThroughRealKitex(t *testing.T) {
	logs := zlogtest.Capture(t)
	const svc = "answer-codes"
	addr := startAnswer(t, svc,
		server.WithMiddleware(ServerMetrics()),
		server.WithMiddleware(LoggingMiddleware()),
		server.WithMiddleware(Callers(svc, CallersConfig{Rules: []CallerRule{{Methods: []string{"Answer"}, Allow: []string{"friend"}}}})))
	friend := answerClient(t, addr, svc, "friend")
	stranger := answerClient(t, addr, svc, "stranger")

	codes := []string{"ok", "2001", "1007", "error"}
	serverBefore := counts(metricsx.RPCServerRequests, svc, codes...)
	clientBefore := counts(metricsx.RPCClientRequests, svc, codes...)

	call := func(cli genericclient.Client, what string) error {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err := cli.GenericCall(ctx, "Answer", `{"what":"`+what+`"}`)
		return err
	}
	if err := call(friend, "ok"); err != nil {
		t.Fatalf("ok: %v", err)
	}
	if b, ok := kerrors.FromBizStatusError(call(friend, "biz")); !ok || b.BizStatusCode() != 2001 {
		t.Fatalf("the caller gets the handler's business code: %v", b)
	}
	if err := call(friend, "boom"); err == nil {
		t.Fatal("a plain error from the handler must fail the call")
	} else if _, biz := kerrors.FromBizStatusError(err); biz {
		t.Fatalf("a plain error is not a business code: %v", err)
	}
	if b, ok := kerrors.FromBizStatusError(call(stranger, "ok")); !ok || b.BizStatusCode() != CodeForbidden {
		t.Fatalf("callers refuses the stranger with 1007: %v", b)
	}

	want := map[string]float64{"ok": 1, "2001": 1, "1007": 1, "error": 1}
	for side, got := range map[string]map[string]float64{
		"server": counts(metricsx.RPCServerRequests, svc, codes...),
		"client": counts(metricsx.RPCClientRequests, svc, codes...),
	} {
		before := serverBefore
		if side == "client" {
			before = clientBefore
		}
		for _, c := range codes {
			if d := got[c] - before[c]; d != want[c] {
				t.Errorf("%s code=%q counted %v times, want %v", side, c, d, want[c])
			}
		}
	}

	// The request log: one "rpc" record per call, a business answer with
	// its code, never its message (a validation message quotes the value it
	// refused, a password among them).
	var rpc []string
	for _, r := range logs.Records() {
		if r.Msg == "rpc" {
			rpc = append(rpc, fmt.Sprintf("%s code=%v err=%v", r.Level, r.Fields["code"], r.Fields["err"] != nil))
		}
	}
	sort.Strings(rpc)
	if want := []string{"ERROR code=<nil> err=true", "INFO code=1007 err=false", "INFO code=2001 err=false", "INFO code=<nil> err=false"}; !slices.Equal(rpc, want) {
		t.Errorf("request log: %q, want %q\n%s", rpc, want, logs)
	}
	if strings.Contains(logs.String(), "nothing here") {
		t.Errorf("the business message must not be logged:\n%s", logs)
	}
}
