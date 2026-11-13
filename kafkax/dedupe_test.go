package kafkax

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/sezznaw/devkit-common/config"
	"github.com/sezznaw/devkit-common/zlog/zlogtest"
)

// TestDedupe: the same event id delivered twice reaches the handler once; a
// failing handler forgets the id so a replay runs; a different id runs.
func TestDedupe(t *testing.T) {
	zlogtest.Discard(t)
	cl := cluster(t, "events.order", "events.order.dlq")
	cfg := Config{Enabled: true, Brokers: cl.ListenAddrs(), MaxRetries: 1}
	c, err := Open(context.Background(), cfg.Static(), cfg, "order", "t")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	srv := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: srv.Addr()})
	defer rdb.Close()
	c.SetDedupe(rdb)

	var handled atomic.Int32
	var fail atomic.Bool
	c.Subscribe("events.order", func(ctx context.Context, ev *Event) error {
		if fail.Load() {
			return errors.New("bug")
		}
		handled.Add(1)
		return nil
	})
	if err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	produce := func(id string) {
		b, _ := json.Marshal(Event{ID: id, Type: "order.placed", TenantID: "t", Time: time.Now().UnixMilli(), Data: json.RawMessage(`{}`)})
		if err := c.prod.ProduceSync(context.Background(), &kgo.Record{Topic: "events.order", Key: []byte("A1"), Value: b}).FirstErr(); err != nil {
			t.Fatal(err)
		}
	}
	produce("evt-1")
	produce("evt-1") // redelivered
	produce("evt-2")
	deadline := time.Now().Add(10 * time.Second)
	for handled.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond) // a third run would show up here
	if handled.Load() != 2 {
		t.Fatalf("handled %d, want 2 (evt-1 once, evt-2 once)", handled.Load())
	}
	if !srv.Exists("dedupe:order:events.order:evt-1") {
		t.Fatal("dedupe key missing")
	}
	// a failure forgets the id: the replay (same id) runs the fixed handler
	fail.Store(true)
	produce("evt-3")
	deadline = time.Now().Add(10 * time.Second)
	for srv.Exists("dedupe:order:events.order:evt-3") || !c.dlqHas(t, "events.order.dlq") {
		if time.Now().After(deadline) {
			t.Fatal("evt-3 not parked / key not forgotten")
		}
		time.Sleep(50 * time.Millisecond)
	}
	fail.Store(false)
	produce("evt-3")
	deadline = time.Now().Add(10 * time.Second)
	for handled.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if handled.Load() != 3 {
		t.Fatalf("replayed evt-3 not handled: %d", handled.Load())
	}
}

// produceEvent writes one envelope with the given event id to topic.
func produceEvent(t *testing.T, c *Client, topic, id string) {
	t.Helper()
	b, _ := json.Marshal(Event{ID: id, Type: "order.placed", TenantID: "t", Time: time.Now().UnixMilli(), Data: json.RawMessage(`{}`)})
	if err := c.prod.ProduceSync(context.Background(), &kgo.Record{Topic: topic, Key: []byte("A1"), Value: b}).FirstErr(); err != nil {
		t.Fatal(err)
	}
}

// TestDedupeCrashRedelivers: a consumer that dies in the middle of a handler
// (SIGKILL, OOM) must not take the event with it. The dead process is one
// that writes nothing to Redis any more (its client is closed) and never
// commits the offset; the next consumer of the group gets the record again
// and, once the dead one's claim has run out, handles it.
func TestDedupeCrashRedelivers(t *testing.T) {
	zlogtest.Discard(t)
	const topic, key = "events.order", "dedupe:order:events.order:evt-crash"
	cl := cluster(t, topic, topic+DLQSuffix)
	cfg := Config{Enabled: true, Brokers: cl.ListenAddrs(), MaxRetries: 1}
	srv := miniredis.RunT(t)

	a, err := Open(context.Background(), cfg.Static(), cfg, "order", "t")
	if err != nil {
		t.Fatal(err)
	}
	rdbA := redis.NewClient(&redis.Options{Addr: srv.Addr()})
	a.SetDedupe(rdbA)
	entered, killed := make(chan struct{}, 1), make(chan struct{})
	a.Subscribe(topic, func(ctx context.Context, ev *Event) error {
		entered <- struct{}{}
		<-killed // in the middle of its work when the process is killed
		return errors.New("killed")
	})
	if err := a.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	produceEvent(t, a, topic, "evt-crash")
	select {
	case <-entered:
	case <-time.After(15 * time.Second):
		t.Fatal("the handler of consumer A never ran")
	}
	// the kill: nothing more reaches Redis from A (its client is closed), and
	// A neither parks the event nor commits its offset (its consumer is
	// stopped before the handler comes back)
	rdbA.Close()
	a.stop()
	close(killed)
	a.Close()
	if !srv.Exists(key) {
		t.Fatal("the dead consumer's claim is gone: a killed process cannot have removed it")
	}

	b, err := Open(context.Background(), cfg.Static(), cfg, "order", "t")
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	rdbB := redis.NewClient(&redis.Options{Addr: srv.Addr()})
	defer rdbB.Close()
	b.SetDedupe(rdbB)
	var handled atomic.Int32
	b.Subscribe(topic, func(ctx context.Context, ev *Event) error {
		if ev.ID == "evt-crash" {
			handled.Add(1)
		}
		return nil
	})
	if err := b.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	// the claim of the dead consumer runs out (nobody renews it)
	srv.FastForward(time.Minute)
	waitFor(t, "consumer B to handle the event the dead consumer had claimed", func() bool { return handled.Load() == 1 })
	waitFor(t, "the event to be marked handled", func() bool { v, _ := srv.Get(key); return v == "1" })
	produceEvent(t, b, topic, "evt-crash") // a later redelivery is a duplicate
	produceEvent(t, b, topic, "evt-after")
	time.Sleep(time.Second)
	if handled.Load() != 1 {
		t.Fatalf("handler ran %d times for evt-crash, want 1", handled.Load())
	}
}

// dedupeFixture is a client with dedupe on a miniredis, for the tests that
// run records through process the way the consumer does.
type dedupeFixture struct {
	c   *Client
	srv *miniredis.Miniredis
}

const dedupeTopic = "events.order"

func newDedupeFixture(t *testing.T, cfg Config) *dedupeFixture {
	t.Helper()
	cl := cluster(t, dedupeTopic, dedupeTopic+DLQSuffix)
	cfg.Enabled, cfg.Brokers = true, cl.ListenAddrs()
	srv := miniredis.RunT(t)
	return &dedupeFixture{c: dedupeClient(t, cfg, srv), srv: srv}
}

// dedupeClient is one more consumer of the same service on the same Redis.
func dedupeClient(t *testing.T, cfg Config, srv *miniredis.Miniredis) *Client {
	t.Helper()
	c, err := Open(context.Background(), cfg.Static(), cfg, "order", "t")
	if err != nil {
		t.Fatal(err)
	}
	rdb := redis.NewClient(&redis.Options{Addr: srv.Addr()})
	c.SetDedupe(rdb)
	t.Cleanup(func() { c.Close(); rdb.Close() })
	return c
}

func dedupeKey(id string) string { return "dedupe:order:" + dedupeTopic + ":" + id }

func eventRecord(id string) *kgo.Record {
	b, _ := json.Marshal(Event{ID: id, Type: "order.placed", TenantID: "t", Time: time.Now().UnixMilli(), Data: json.RawMessage(`{}`)})
	return &kgo.Record{Topic: dedupeTopic, Key: []byte("A1"), Value: b}
}

// shortClaims lowers the floor of kafka.dedupe.inflight_ttl for one test.
func shortClaims(t *testing.T, floor time.Duration) {
	t.Helper()
	was := minInFlightTTL
	minInFlightTTL = floor
	t.Cleanup(func() { minInFlightTTL = was })
}

func TestDedupeConfig(t *testing.T) {
	if got := (DedupeConfig{}).inFlightTTL(); got != 60*time.Second {
		t.Errorf("default inflight_ttl %s, want 60s", got)
	}
	if got := (DedupeConfig{InFlightTTL: config.Duration(time.Second)}).inFlightTTL(); got != 5*time.Second {
		t.Errorf("inflight_ttl 1s must be raised to the floor of 5s, got %s", got)
	}
	if got := (DedupeConfig{InFlightTTL: config.Duration(90 * time.Second)}).inFlightTTL(); got != 90*time.Second {
		t.Errorf("inflight_ttl 90s: got %s", got)
	}
}

// TestDedupeMarksHandledOnlyAfterSuccess: while the handler runs the key is
// this consumer's claim with the in-flight TTL; "handled" (24h) is written
// only after the handler returned nil, and a later delivery is skipped.
func TestDedupeMarksHandledOnlyAfterSuccess(t *testing.T) {
	zlogtest.Discard(t)
	f := newDedupeFixture(t, Config{})
	key := dedupeKey("evt-1")
	runs := 0
	h := func(ctx context.Context, ev *Event) error {
		runs++
		if v, _ := f.srv.Get(key); v != "p:"+f.c.instance {
			t.Errorf("during the handler the key is %q, want the claim p:<instance>", v)
		}
		if ttl := f.srv.TTL(key); ttl != 60*time.Second {
			t.Errorf("claim TTL %s, want the in-flight TTL 60s", ttl)
		}
		return nil
	}
	ctx := context.Background()
	if err := f.c.process(ctx, eventRecord("evt-1"), h); err != nil {
		t.Fatal(err)
	}
	if v, _ := f.srv.Get(key); v != "1" || f.srv.TTL(key) != 24*time.Hour {
		t.Fatalf("after success the key is %q for %s, want 1 for 24h", v, f.srv.TTL(key))
	}
	if err := f.c.process(ctx, eventRecord("evt-1"), h); err != nil || runs != 1 {
		t.Fatalf("redelivery after success: err=%v, handler ran %d times, want 1", err, runs)
	}
	// a key written by a version before v0.82.0 ("1" before the handler ran) is a handled event
	f.srv.Set(dedupeKey("evt-old"), "1")
	f.srv.SetTTL(dedupeKey("evt-old"), time.Hour)
	if err := f.c.process(ctx, eventRecord("evt-old"), h); err != nil || runs != 1 {
		t.Fatalf("key of an older version: err=%v, handler ran %d times, want 1", err, runs)
	}
}

// TestDedupeHandlerErrorReleasesTheClaim: a handler error is retried, then
// the claim is given back so that the next delivery (or the replay from the
// dlq) runs the handler; a key that says "handled" by then is left alone.
func TestDedupeHandlerErrorReleasesTheClaim(t *testing.T) {
	zlogtest.Discard(t)
	f := newDedupeFixture(t, Config{MaxRetries: 2})
	key := dedupeKey("evt-1")
	runs, fail := 0, true
	h := func(ctx context.Context, ev *Event) error {
		runs++
		if fail {
			return errors.New("bug")
		}
		return nil
	}
	ctx := context.Background()
	if err := f.c.process(ctx, eventRecord("evt-1"), h); err == nil || runs != 2 {
		t.Fatalf("failing handler: err=%v after %d runs, want an error after 2", err, runs)
	}
	if f.srv.Exists(key) {
		t.Fatal("the claim of a failed event must be released")
	}
	fail = false
	if err := f.c.process(ctx, eventRecord("evt-1"), h); err != nil || runs != 3 {
		t.Fatalf("delivery after the failure: err=%v, %d runs, want 3", err, runs)
	}
	// another consumer took the event over and finished it while this one was still failing
	key2 := dedupeKey("evt-2")
	err := f.c.process(ctx, eventRecord("evt-2"), func(ctx context.Context, ev *Event) error {
		f.srv.Set(key2, "1")
		return errors.New("bug")
	})
	if v, _ := f.srv.Get(key2); err == nil || v != "1" {
		t.Fatalf("release removed a key it does not own: err=%v key=%q", err, v)
	}
}

// TestDedupeClaimIsRenewedWhileTheHandlerRuns: a handler that takes longer
// than the in-flight TTL keeps its claim.
func TestDedupeClaimIsRenewedWhileTheHandlerRuns(t *testing.T) {
	zlogtest.Discard(t)
	shortClaims(t, 600*time.Millisecond)
	f := newDedupeFixture(t, Config{Dedupe: DedupeConfig{InFlightTTL: config.Duration(600 * time.Millisecond)}})
	key := dedupeKey("evt-1")
	err := f.c.process(context.Background(), eventRecord("evt-1"), func(ctx context.Context, ev *Event) error {
		for i := 0; i < 3; i++ { // 1.2s of the store's time in all: two TTLs
			f.srv.FastForward(400 * time.Millisecond)
			time.Sleep(350 * time.Millisecond) // a renewal (every 200ms) falls in here
			if v, _ := f.srv.Get(key); v != "p:"+f.c.instance {
				return errors.New("the claim ran out under a running handler: " + v)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestDedupeWaitsForTheConsumerThatHasTheEvent: an event in flight in
// another consumer is not run next to it. When the other one finishes, it
// is a duplicate; when the other one gives it back, it runs here.
func TestDedupeWaitsForTheConsumerThatHasTheEvent(t *testing.T) {
	zlogtest.Discard(t)
	f := newDedupeFixture(t, Config{})
	var runs atomic.Int32
	h := func(ctx context.Context, ev *Event) error { runs.Add(1); return nil }
	start := func(id string) chan error {
		f.srv.Set(dedupeKey(id), "p:other")
		f.srv.SetTTL(dedupeKey(id), time.Minute)
		done := make(chan error, 1)
		go func() { done <- f.c.process(context.Background(), eventRecord(id), h) }()
		select {
		case err := <-done:
			t.Fatalf("%s: process returned (%v) while another consumer had the event in flight", id, err)
		case <-time.After(700 * time.Millisecond):
		}
		if runs.Load() != 0 {
			t.Fatalf("%s: the handler ran next to the consumer that has the event", id)
		}
		return done
	}
	wait := func(done chan error) {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("process still waiting after the other consumer was done")
		}
	}
	// the other consumer finishes: a duplicate
	done := start("evt-1")
	f.srv.Set(dedupeKey("evt-1"), "1")
	wait(done)
	if runs.Load() != 0 {
		t.Fatalf("handled by the other consumer, yet the handler ran %d times", runs.Load())
	}
	// the other consumer fails and gives the event back: it runs here
	done = start("evt-2")
	f.srv.Del(dedupeKey("evt-2"))
	wait(done)
	if v, _ := f.srv.Get(dedupeKey("evt-2")); runs.Load() != 1 || v != "1" {
		t.Fatalf("released by the other consumer: handler ran %d times, key %q; want 1 and 1", runs.Load(), v)
	}
}

// TestDedupeTakesOverAClaimThatIsNeverGivenUp: a claim that is still there
// after twice the in-flight TTL (a consumer that hangs but keeps renewing)
// does not block the partition for good: the event is taken over, loudly.
func TestDedupeTakesOverAClaimThatIsNeverGivenUp(t *testing.T) {
	logs := zlogtest.Capture(t)
	shortClaims(t, 400*time.Millisecond)
	f := newDedupeFixture(t, Config{Dedupe: DedupeConfig{InFlightTTL: config.Duration(400 * time.Millisecond)}})
	key := dedupeKey("evt-1")
	f.srv.Set(key, "p:other") // miniredis keeps it until told otherwise: a claim renewed for ever
	runs := 0
	begin := time.Now()
	err := f.c.process(context.Background(), eventRecord("evt-1"), func(ctx context.Context, ev *Event) error { runs++; return nil })
	if err != nil || runs != 1 {
		t.Fatalf("err=%v, handler ran %d times, want 1", err, runs)
	}
	if waited := time.Since(begin); waited < 800*time.Millisecond {
		t.Errorf("taken over after %s, before twice the in-flight TTL (800ms)", waited)
	}
	if v, _ := f.srv.Get(key); v != "1" {
		t.Errorf("key %q after the take-over, want 1", v)
	}
	if !logs.Has("WARN", "event taken over after waiting: its claim was still being renewed") {
		t.Errorf("the take-over is not logged:\n%s", logs)
	}
}

// TestDedupeStopEndsTheWait: a consumer that is stopped while it waits for
// another consumer's claim leaves the event where it is: not handled, not
// parked, the claim untouched.
func TestDedupeStopEndsTheWait(t *testing.T) {
	zlogtest.Discard(t)
	f := newDedupeFixture(t, Config{})
	key := dedupeKey("evt-1")
	f.srv.Set(key, "p:other")
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	runs := 0
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.c.handle(ctx, eventRecord("evt-1"), func(ctx context.Context, ev *Event) error { runs++; return nil })
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("stopping does not end the wait for another consumer's claim")
	}
	if v, _ := f.srv.Get(key); runs != 0 || v != "p:other" {
		t.Fatalf("handler ran %d times, key %q; want 0 and the other consumer's claim", runs, v)
	}
	if f.c.dlqHas(t, dedupeTopic+DLQSuffix) {
		t.Fatal("an event that was only waited for must not be parked")
	}
}

// TestDedupeRedisDownHandlesAnyway: without the store the event is handled
// (at least once), as before.
func TestDedupeRedisDownHandlesAnyway(t *testing.T) {
	logs := zlogtest.Capture(t)
	f := newDedupeFixture(t, Config{})
	f.srv.Close()
	runs := 0
	if err := f.c.process(context.Background(), eventRecord("evt-1"), func(ctx context.Context, ev *Event) error { runs++; return nil }); err != nil || runs != 1 {
		t.Fatalf("err=%v, handler ran %d times, want 1", err, runs)
	}
	if !logs.Has("WARN", "dedupe check failed; handling the event anyway") {
		t.Errorf("not logged:\n%s", logs)
	}
}

// TestDedupeTwoConsumersRaceForOneEvent: two consumers that get the same
// event at the same moment (a consumer that was thrown out of the group
// while its handler was still running, and the one that got its partition)
// never run the handler side by side. When the first succeeds the second
// skips the event; when the first fails the second runs it afterwards.
func TestDedupeTwoConsumersRaceForOneEvent(t *testing.T) {
	zlogtest.Discard(t)
	f := newDedupeFixture(t, Config{MaxRetries: 1})
	a, b := f.c, dedupeClient(t, f.c.cfg, f.srv)
	var running, most, runs atomic.Int32
	var failFirst atomic.Bool
	h := func(ctx context.Context, ev *Event) error {
		n := running.Add(1)
		defer running.Add(-1)
		for {
			m := most.Load()
			if n <= m || most.CompareAndSwap(m, n) {
				break
			}
		}
		time.Sleep(30 * time.Millisecond)
		if runs.Add(1) == 1 && failFirst.Load() {
			return errors.New("bug")
		}
		return nil
	}
	race := func(id string) (errs int) {
		runs.Store(0)
		out := make(chan error, 2)
		for _, c := range []*Client{a, b} {
			go func() { out <- c.process(context.Background(), eventRecord(id), h) }()
		}
		for i := 0; i < 2; i++ {
			if err := <-out; err != nil {
				errs++
			}
		}
		return errs
	}
	for _, id := range []string{"evt-1", "evt-2", "evt-3", "evt-4", "evt-5"} {
		if errs := race(id); errs != 0 || runs.Load() != 1 {
			t.Fatalf("%s: %d errors, handler ran %d times; want it to run once", id, errs, runs.Load())
		}
	}
	failFirst.Store(true)
	for _, id := range []string{"evt-6", "evt-7", "evt-8"} {
		if errs := race(id); errs != 1 || runs.Load() != 2 {
			t.Fatalf("%s: %d errors, handler ran %d times; want the first run to fail and the other consumer to run it once more", id, errs, runs.Load())
		}
		if v, _ := f.srv.Get(dedupeKey(id)); v != "1" {
			t.Fatalf("%s: key %q after the second run succeeded, want 1", id, v)
		}
	}
	if most.Load() != 1 {
		t.Fatalf("the handler ran %d times side by side for one event id", most.Load())
	}
}

// dlqHas reports whether the dlq topic has at least one record.
func (c *Client) dlqHas(t *testing.T, topic string) bool {
	cl, err := kgo.NewClient(append(c.baseOpts(), kgo.ConsumeTopics(topic), kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()))...)
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	f := cl.PollFetches(ctx)
	return f.NumRecords() > 0
}
