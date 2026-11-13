// Package kafkax is the event bus (Redpanda, Kafka protocol) for a service,
// opened by the framework from the `kafka:` section the way mysqlx opens
// MySQL. A service publishes events with Client.Publish, which wraps the data
// in the standard envelope and carries the trace in the headers, and consumes
// with Client.Subscribe, which the framework runs after the server has
// started: every message a span of the producer's trace and a record of the
// log, a failing handler retried and then parked in <topic>.dlq, offsets
// committed after processing (at least once; a handler is to be idempotent).
package kafkax

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/sasl/scram"
	"github.com/twmb/franz-go/plugin/kotel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"

	"github.com/sezznaw/devkit-common/config"
	"github.com/sezznaw/devkit-common/metricsx"
	"github.com/sezznaw/devkit-common/zlog"
)

// Config is the `kafka:` section of a service configuration.
type Config struct {
	Enabled bool   `yaml:"enabled"`
	Source  string `yaml:"source"` // static (default) or platform (datasource row of kind kafka)
	Role    string `yaml:"role"`   // tenant (default) or platform

	// Brokers, Username, Password: the cluster, with source static. Default
	// broker 127.0.0.1:9092; no username means no SASL.
	Brokers  []string `yaml:"brokers"`
	Username string   `yaml:"username"`
	Password string   `yaml:"password"`

	// MaxRetries is how often a failing handler is retried before the message
	// goes to <topic>.dlq. Default 3.
	MaxRetries int `yaml:"max_retries"`

	// Outbox: the relay that publishes what PublishTx queued in the
	// database (events written in the same transaction as the change). On
	// by default when the service has MySQL too; see OutboxConfig.
	Outbox OutboxConfig `yaml:"outbox"`

	// Dedupe: a consumer skips an event id it already handled (Redis, 24h),
	// so the at-least-once delivery of Kafka and the outbox does not reach
	// the handler again once it has succeeded. On by default when the
	// service has Redis.
	Dedupe DedupeConfig `yaml:"dedupe"`
}

// DedupeConfig is the `kafka.dedupe` section.
type DedupeConfig struct {
	// Enabled: false turns the consumer-side dedupe off (a handler that is
	// idempotent by nature and wants the Redis round trip saved).
	Enabled *bool `yaml:"enabled"`
	// TTL is how long a handled event id is remembered. Default 24h: longer
	// than any redelivery (a rebalance, a replay from the outbox) takes.
	TTL config.Duration `yaml:"ttl"`
	// InFlightTTL is how long the claim of an event that is being handled
	// lasts without being renewed; the consumer renews it while the handler
	// runs. It is how long a redelivered event waits after the consumer that
	// was handling it died (kill -9, out of memory). Default 60s, at least 5s.
	InFlightTTL config.Duration `yaml:"inflight_ttl"`
}

// EnabledOrDefault reports whether dedupe should run: on unless
// kafka.dedupe.enabled is false.
func (c DedupeConfig) EnabledOrDefault() bool { return c.Enabled == nil || *c.Enabled }
func (c DedupeConfig) ttl() time.Duration     { return c.TTL.Or(24 * time.Hour) }
func (c DedupeConfig) inFlightTTL() time.Duration {
	if d := c.InFlightTTL.Or(defaultInFlightTTL); d > minInFlightTTL {
		return d
	}
	return minInFlightTTL
}

const (
	SourceStatic   = "static"
	SourcePlatform = "platform"
	RoleTenant     = "tenant"
	RolePlatform   = "platform"
	// DLQSuffix is appended to a topic for the messages its handler could not process.
	DLQSuffix = ".dlq"

	defaultRetries = 3

	defaultInFlightTTL = 60 * time.Second
	// claimPoll is how often a consumer looks again at an event another
	// consumer has in flight.
	claimPoll = 500 * time.Millisecond
)

// minInFlightTTL is the floor of kafka.dedupe.inflight_ttl: a claim is
// renewed every third of it, and one slow Redis round trip must not lose
// it. A variable for the tests.
var minInFlightTTL = 5 * time.Second

func (c Config) SourceName() string {
	if s := strings.TrimSpace(c.Source); s != "" {
		return s
	}
	return SourceStatic
}

func (c Config) RoleName() string {
	if r := strings.TrimSpace(c.Role); r != "" {
		return r
	}
	return RoleTenant
}

func (c Config) retries() int {
	if c.MaxRetries > 0 {
		return c.MaxRetries
	}
	return defaultRetries
}

// Validate rejects a configuration that cannot work, naming the setting.
func (c Config) Validate() error {
	if !c.Enabled {
		return nil
	}
	switch c.SourceName() {
	case SourceStatic, SourcePlatform:
	default:
		return fmt.Errorf("kafkax: kafka.source is %q; it must be %s or %s", c.Source, SourceStatic, SourcePlatform)
	}
	switch c.RoleName() {
	case RoleTenant, RolePlatform:
	default:
		return fmt.Errorf("kafkax: kafka.role is %q; it must be %s or %s", c.Role, RoleTenant, RolePlatform)
	}
	return nil
}

// Target is a cluster to connect to, once resolved.
type Target struct {
	Brokers  []string
	Username string
	Password string
	// RewriteAddr, when set, maps every broker address the client dials
	// (the seeds and the addresses brokers advertise) to another one. Takeover
	// mode sets it to the tunnel map: Redpanda advertises its in-cluster DNS
	// name, which a developer's machine cannot reach. [takeover]
	RewriteAddr func(addr string) string
}

// Static is the target of a static configuration.
func (c Config) Static() Target {
	brokers := c.Brokers
	if len(brokers) == 0 {
		brokers = []string{"127.0.0.1:9092"}
	}
	return Target{Brokers: brokers, Username: c.Username, Password: c.Password}
}

// Event is what travels on the bus: the standard envelope. Data is the
// event's own payload, JSON. Topic, Key, Partition and Offset are set on a
// received event and say where it came from.
type Event struct {
	ID       string          `json:"id"`
	Type     string          `json:"type"`
	TenantID string          `json:"tenant_id"`
	Time     int64           `json:"time"` // milliseconds since the epoch, UTC
	Data     json.RawMessage `json:"data"`

	Topic     string `json:"-"`
	Key       string `json:"-"`
	Partition int32  `json:"-"`
	Offset    int64  `json:"-"`
}

// Decode unmarshals Data into v.
func (e *Event) Decode(v any) error { return json.Unmarshal(e.Data, v) }

// Handler processes one event. An error means "retry, then park": the
// framework retries it, then sends the message to <topic>.dlq.
type Handler func(ctx context.Context, ev *Event) error

// Client is a producer and the consumers of a service.
type Client struct {
	cfg     Config
	target  Target
	service string
	tenant  string
	tracer  *kotel.Tracer
	hooks   []kgo.Hook
	prod    *kgo.Client

	mu   sync.Mutex
	subs []subscription
	cons []*kgo.Client
	stop context.CancelFunc
	wg   sync.WaitGroup

	dedupe   *redis.Client // nil: no consumer-side dedupe
	instance string        // this process, in the value of the claims it holds

	outboxState
}

// SetDedupe gives the consumers the Redis their dedupe keys live in
// ("dedupe:<service>:<topic>:<event id>"). The runtime calls it when the
// service has Redis and kafka.dedupe.enabled is not false; an event id whose
// handler succeeded is then not handed to the handler again for
// kafka.dedupe.ttl, even when Kafka delivers it twice. A handler that fails
// (and parks the event in the dlq) forgets the id, so a replay runs it
// again.
//
// The key has two states. While the handler runs it holds a claim,
// "p:<instance>", that lasts kafka.dedupe.inflight_ttl and is renewed by the
// consumer that holds it; only when the handler has returned nil does it
// become "1" (handled) for kafka.dedupe.ttl. A consumer killed in the middle
// of a handler therefore leaves a claim that runs out, and the redelivered
// event is handled by whoever gets it next: until v0.82.0 the key was
// written as handled before the handler ran, and such an event was skipped
// as a duplicate and lost. So a handler can run again for an event whose
// first run was cut short: handlers are to be idempotent.
//
// 去重键有两个状态：handler 运行期间是「处理中」占位 p:<实例>（kafka.dedupe.inflight_ttl，
// 持有它的消费者续期），handler 返回 nil 之后才变成「已处理」1（kafka.dedupe.ttl）。消费者在
// handler 中途被杀，占位到期，重投的事件会被再处理一次（v0.82.0 之前键在 handler 之前就写成
// 已处理，这样的事件被当成重复跳过、丢失）。所以 handler 必须能重跑。
func (c *Client) SetDedupe(rdb *redis.Client) { c.dedupe = rdb }

const (
	// inFlight starts the value of a dedupe key while the event is being
	// handled; any other value says it was handled (the "1" of this version
	// and of the versions that wrote the key before the handler ran).
	inFlight = "p:"
	handled  = "1"
)

// The claim scripts touch the key only while it still holds this consumer's
// claim: a claim that ran out and was taken by another consumer, or that
// another consumer turned into "handled", is neither extended nor deleted.
var (
	renewClaim   = redis.NewScript(`if redis.call("get", KEYS[1]) == ARGV[1] then return redis.call("pexpire", KEYS[1], ARGV[2]) else return 0 end`)
	releaseClaim = redis.NewScript(`if redis.call("get", KEYS[1]) == ARGV[1] then return redis.call("del", KEYS[1]) else return 0 end`)
	// takeOverClaim replaces another consumer's claim (ARGV[1]) by this
	// consumer's (ARGV[2]), and nothing else: not a key that turned to
	// "handled" in the meantime.
	takeOverClaim = redis.NewScript(`if redis.call("get", KEYS[1]) == ARGV[1] then redis.call("set", KEYS[1], ARGV[2], "px", ARGV[3]) return 1 else return 0 end`)
)

// claim is the right to handle one event, held in Redis until confirm
// (handled) or release (not handled; whoever gets the event next runs it).
type claim struct {
	key, token string
	stop       func() // ends the renewal
}

// claim takes the right to handle the event: (claim, false, nil). An event
// that was handled before is (nil, true, nil). An event another consumer
// has in flight is waited for: it becomes "handled" (a duplicate), or its
// claim is released or runs out (this consumer handles it); a claim that is
// still being renewed after twice the in-flight TTL is taken over, so that
// one key cannot block the partition for good. stop ends the wait when the
// consumer is stopping: its error is returned and the event stays where it
// is. A Redis error lets the event through without a claim (at least once,
// as without dedupe) and is logged.
func (c *Client) claim(ctx, stop context.Context, topic, id string, log *zlog.Logger) (*claim, bool, error) {
	if c.dedupe == nil || id == "" {
		return nil, false, nil
	}
	key, token, ttl := "dedupe:"+c.service+":"+topic+":"+id, inFlight+c.instance, c.cfg.Dedupe.inFlightTTL()
	unavailable := func(err error) (*claim, bool, error) {
		log.Warn("dedupe check failed; handling the event anyway", zlog.Err(err))
		return nil, false, nil
	}
	deadline, waited := time.Now().Add(2*ttl), false
	for {
		ok, err := c.dedupe.SetNX(ctx, key, token, ttl).Result()
		if err != nil {
			return unavailable(err)
		}
		if ok {
			if waited {
				log.Warn("event taken over: the consumer that had claimed it did not finish")
			}
			return c.hold(key, token, ttl), false, nil
		}
		v, err := c.dedupe.Get(ctx, key).Result()
		switch {
		case errors.Is(err, redis.Nil):
			continue // released or run out between the two commands: claim again
		case err != nil:
			return unavailable(err)
		case !strings.HasPrefix(v, inFlight):
			return nil, true, nil // handled
		case v == token:
			// this process's own claim, left by a run whose outcome did not
			// reach Redis: nobody else has the event, run it
			n, err := renewClaim.Run(ctx, c.dedupe, []string{key}, token, ttl.Milliseconds()).Int()
			if err != nil {
				return unavailable(err)
			}
			if n == 0 {
				continue // run out between the two commands: claim again
			}
			return c.hold(key, token, ttl), false, nil
		}
		// In flight in another consumer. Alive, it finishes (the key turns
		// to handled, or is released); dead, its claim runs out.
		if !time.Now().Before(deadline) {
			// only the claim just seen is replaced: if its holder finished
			// or gave it back in this instant, look again
			n, err := takeOverClaim.Run(ctx, c.dedupe, []string{key}, v, token, ttl.Milliseconds()).Int()
			if err != nil {
				return unavailable(err)
			}
			if n == 0 {
				continue
			}
			log.Warn("event taken over after waiting: its claim was still being renewed", zlog.Dur("waited", 2*ttl))
			return c.hold(key, token, ttl), false, nil
		}
		if !waited {
			waited = true
			log.Info("event in flight in another consumer; waiting", zlog.Dur("at_most", 2*ttl))
		}
		t := time.NewTimer(claimPoll)
		select {
		case <-stop.Done():
			t.Stop()
			return nil, false, stop.Err()
		case <-t.C:
		}
	}
}

// hold keeps a claim alive while the handler runs: every third of the TTL
// it is extended, as long as it is still this consumer's. A process that
// is killed stops renewing, and the claim runs out within the TTL.
func (c *Client) hold(key, token string, ttl time.Duration) *claim {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(ttl / 3)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				// A failed renewal is tried again at the next tick; a claim
				// that is lost all the same costs a second run of the
				// handler only when the event is redelivered meanwhile.
				_ = renewClaim.Run(ctx, c.dedupe, []string{key}, token, ttl.Milliseconds()).Err()
			}
		}
	}()
	return &claim{key: key, token: token, stop: func() { cancel(); <-done }}
}

// confirm marks the event handled, after the handler returned nil.
// Unconditional: the event is handled, whoever holds the key by now. When
// the write fails the claim runs out by itself and a redelivery runs the
// handler again.
func (c *Client) confirm(ctx context.Context, cl *claim, log *zlog.Logger) {
	if cl == nil {
		return
	}
	cl.stop()
	if err := c.dedupe.Set(context.WithoutCancel(ctx), cl.key, handled, c.cfg.Dedupe.ttl()).Err(); err != nil {
		log.Warn("handled event not marked in the dedupe store; a redelivery runs the handler again", zlog.Err(err))
	}
}

// release gives the claim back, after the handler failed: the event's next
// delivery (a redelivery, a replay from the dlq) runs the handler. Only this
// consumer's own claim is removed, never another's or a "handled".
func (c *Client) release(ctx context.Context, cl *claim) {
	if cl == nil {
		return
	}
	cl.stop()
	_ = releaseClaim.Run(context.WithoutCancel(ctx), c.dedupe, []string{cl.key}, cl.token).Err()
}

type subscription struct {
	topic   string
	handler Handler
}

// Open connects the producer and checks the cluster answers. service names
// the consumer group and tenant goes into every event.
func Open(ctx context.Context, t Target, cfg Config, service, tenant string) (*Client, error) {
	if len(t.Brokers) == 0 {
		return nil, fmt.Errorf("kafkax: brokers are required")
	}
	tracer := kotel.NewTracer()
	hooks := kotel.NewKotel(kotel.WithTracer(tracer)).Hooks()
	hooks = append(hooks, metricsHook{})
	c := &Client{cfg: cfg, target: t, service: service, tenant: tenant, tracer: tracer, hooks: hooks, instance: uuid.NewString()}
	prod, err := kgo.NewClient(append(c.baseOpts(),
		kgo.RequiredAcks(kgo.AllISRAcks()),
		kgo.ProducerBatchCompression(kgo.SnappyCompression(), kgo.NoCompression()),
	)...)
	if err != nil {
		return nil, fmt.Errorf("kafkax: client: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := prod.Ping(pingCtx); err != nil {
		prod.Close()
		return nil, fmt.Errorf("kafkax: ping %v: %w (is the cluster up, are kafka.brokers right?)", t.Brokers, err)
	}
	c.prod = prod
	return c, nil
}

func (c *Client) baseOpts() []kgo.Opt {
	opts := []kgo.Opt{
		kgo.SeedBrokers(c.target.Brokers...),
		kgo.WithHooks(c.hooks...),
		kgo.ClientID(c.service),
	}
	if c.target.Username != "" {
		opts = append(opts, kgo.SASL(scram.Auth{User: c.target.Username, Pass: c.target.Password}.AsSha256Mechanism()))
	}
	if rw := c.target.RewriteAddr; rw != nil {
		d := &net.Dialer{Timeout: 10 * time.Second}
		opts = append(opts, kgo.Dialer(func(ctx context.Context, network, host string) (net.Conn, error) {
			return d.DialContext(ctx, network, rw(host))
		}))
	}
	return opts
}

// Publish sends one event: data wrapped in the envelope (a new id, the
// type, the tenant, now), the key deciding the partition (the entity's id,
// so its events stay in order), the trace of ctx in the headers. It returns
// when the cluster has acknowledged the write.
func (c *Client) Publish(ctx context.Context, topic, key, eventType string, data any) error {
	payload, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("kafkax: encode %s: %w", eventType, err)
	}
	ev := Event{ID: uuid.NewString(), Type: eventType, TenantID: c.tenant, Time: time.Now().UnixMilli(), Data: payload}
	value, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("kafkax: encode envelope: %w", err)
	}
	rec := &kgo.Record{
		Topic:   topic,
		Key:     []byte(key),
		Value:   value,
		Context: ctx, // kotel starts the produce span under ctx and puts traceparent into the headers
		Headers: []kgo.RecordHeader{
			{Key: "content-type", Value: []byte("application/json")},
			{Key: "event-type", Value: []byte(eventType)},
		},
	}
	start := time.Now()
	if err := c.prod.ProduceSync(ctx, rec).FirstErr(); err != nil {
		metricsx.KafkaEventsPublished.WithLabelValues(topic, "error").Inc()
		zlog.Ctx(ctx).Error("event not published", zlog.Str("topic", topic), zlog.Str("type", eventType), zlog.Str("key", key), zlog.Err(err))
		return fmt.Errorf("kafkax: publish %s to %s: %w", eventType, topic, err)
	}
	metricsx.KafkaEventsPublished.WithLabelValues(topic, "ok").Inc()
	zlog.Ctx(ctx).Info("event published", zlog.Str("topic", topic), zlog.Str("type", eventType), zlog.Str("key", key), zlog.Str("event_id", ev.ID), zlog.Dur("latency", time.Since(start)))
	return nil
}

// Subscribe registers a handler for a topic; the framework starts consuming
// (Start) once the server is up. The consumer group is the service's name:
// one service, one group, one handler per topic.
func (c *Client) Subscribe(topic string, h Handler) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.subs = append(c.subs, subscription{topic: topic, handler: h})
}

// Start begins consuming every subscribed topic, one consumer per topic, in
// the background; Close stops them.
func (c *Client) Start(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.subs) == 0 {
		return nil
	}
	ctx, c.stop = context.WithCancel(ctx)
	for _, s := range c.subs {
		cl, err := kgo.NewClient(append(c.baseOpts(),
			kgo.ConsumerGroup(c.service),
			kgo.ConsumeTopics(s.topic),
			kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()),
			kgo.DisableAutoCommit(),
			kgo.BlockRebalanceOnPoll(),
		)...)
		if err != nil {
			c.stop()
			return fmt.Errorf("kafkax: consumer of %s: %w", s.topic, err)
		}
		c.cons = append(c.cons, cl)
		c.wg.Add(1)
		go c.consume(ctx, cl, s)
		zlog.Info("consuming", zlog.Str("topic", s.topic), zlog.Str("group", c.service))
	}
	return nil
}

func (c *Client) consume(ctx context.Context, cl *kgo.Client, s subscription) {
	defer c.wg.Done()
	// BlockRebalanceOnPoll: a rebalance (and Close) waits until the records
	// of the last poll are processed; on the way out, let it through.
	defer cl.AllowRebalance()
	for {
		fetches := cl.PollFetches(ctx)
		if fetches.IsClientClosed() || ctx.Err() != nil {
			return
		}
		fetches.EachError(func(topic string, p int32, err error) {
			zlog.Error("fetch failed", zlog.Str("topic", topic), zlog.Int("partition", p), zlog.Err(err))
		})
		fetches.EachRecord(func(r *kgo.Record) {
			c.handle(ctx, r, s.handler)
		})
		// lag: how far behind the end of each partition this poll left us
		// (the largest partition's figure is the topic's gauge)
		var lag int64
		fetches.EachPartition(func(p kgo.FetchTopicPartition) {
			if n := len(p.Records); n > 0 {
				if l := p.HighWatermark - p.Records[n-1].Offset - 1; l > lag {
					lag = l
				}
			}
		})
		if !fetches.Empty() {
			metricsx.KafkaConsumerLag.WithLabelValues(s.topic).Set(float64(lag))
		}
		if err := cl.CommitUncommittedOffsets(ctx); err != nil && ctx.Err() == nil {
			zlog.Error("commit failed", zlog.Str("topic", s.topic), zlog.Err(err))
		}
		cl.AllowRebalance()
	}
}

// handle is what the consumer does with one record: process it, and park
// it in the DLQ when it keeps failing. It never panics out and never blocks
// the partition forever: the worst case is a message in the DLQ and an
// error record. A shutdown in the middle parks nothing: the record is
// redelivered.
func (c *Client) handle(ctx context.Context, r *kgo.Record, h Handler) {
	err := c.process(ctx, r, h)
	if err == nil || ctx.Err() != nil {
		return
	}
	zlog.Ctx(ctx).Error("event parked in dlq", zlog.Str("topic", r.Topic), zlog.Str("key", string(r.Key)), zlog.Int("partition", r.Partition), zlog.Int("offset", r.Offset), zlog.Err(err))
	metricsx.KafkaEventsHandled.WithLabelValues(r.Topic, "dlq").Inc()
	c.park(ctx, r, err)
}

// process runs one record through the handler the way consumption does: a
// span under the producer's trace, the envelope decoded, the dedupe claim,
// the handler retried. nil means the record is done (handled, or a
// duplicate); an error is what remains after the retries, for the caller to
// decide (the consumer parks the record, the dead-letter tool keeps it
// pending). The event id is marked handled only after the handler returned
// nil; a failure releases the claim, so the record runs when it comes
// again. The dead-letter tool calls it with the record it rebuilt from the
// dlq, so a replay is the handler running in this process.
func (c *Client) process(ctx context.Context, r *kgo.Record, h Handler) error {
	stopping := ctx // the caller's, cancelled when the consumer stops; the span's context below is the record's
	ctx, span := c.tracer.WithProcessSpan(r)
	defer span.End()
	start := time.Now()
	ev := &Event{Topic: r.Topic, Key: string(r.Key), Partition: r.Partition, Offset: r.Offset}
	log := zlog.Ctx(ctx).With(zlog.Str("topic", r.Topic), zlog.Str("key", ev.Key), zlog.Int("partition", r.Partition), zlog.Int("offset", r.Offset))
	if err := json.Unmarshal(r.Value, ev); err != nil {
		log.Error("event is not an envelope", zlog.Err(err))
		return fmt.Errorf("event is not an envelope: %w", err)
	}
	defer func() { metricsx.KafkaHandleDuration.WithLabelValues(r.Topic).Observe(time.Since(start).Seconds()) }()
	log = log.With(zlog.Str("type", ev.Type), zlog.Str("event_id", ev.ID))
	span.SetAttributes(attribute.String("event.type", ev.Type), attribute.String("event.id", ev.ID))
	claimed, dup, err := c.claim(ctx, stopping, r.Topic, ev.ID, log)
	if err != nil {
		return err // stopping while another consumer had the event in flight: it is redelivered
	}
	if dup {
		metricsx.KafkaEventsHandled.WithLabelValues(r.Topic, "duplicate").Inc()
		log.Info("duplicate event skipped (already handled)")
		return nil
	}

	for attempt := 1; attempt <= c.cfg.retries(); attempt++ {
		if err = safely(ctx, h, ev); err == nil {
			c.confirm(ctx, claimed, log) // only now: a consumer killed before this line leaves a claim that runs out
			metricsx.KafkaEventsHandled.WithLabelValues(r.Topic, "ok").Inc()
			log.Info("event handled", zlog.Int("attempt", attempt), zlog.Dur("latency", time.Since(start)))
			return nil
		}
		if ctx.Err() != nil {
			c.release(ctx, claimed) // shutting down: the event is redelivered and must run
			return err
		}
		metricsx.KafkaEventsHandled.WithLabelValues(r.Topic, "retried").Inc()
		log.Warn("handler failed", zlog.Int("attempt", attempt), zlog.Err(err))
		time.Sleep(time.Duration(attempt) * 200 * time.Millisecond)
	}
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
	log.Error("handler failed after retries", zlog.Err(err))
	c.release(ctx, claimed) // a replay from the dlq must run
	return err
}

func safely(ctx context.Context, h Handler, ev *Event) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("handler panicked: %v", p)
		}
	}()
	return h(ctx, ev)
}

// park copies the record to <topic>.dlq with the error in a header.
func (c *Client) park(ctx context.Context, r *kgo.Record, cause error) {
	dlq := &kgo.Record{
		Topic:   r.Topic + DLQSuffix,
		Key:     r.Key,
		Value:   r.Value,
		Context: ctx,
		Headers: append(r.Headers,
			kgo.RecordHeader{Key: "error", Value: []byte(cause.Error())},
			kgo.RecordHeader{Key: "source-topic", Value: []byte(r.Topic)},
			kgo.RecordHeader{Key: "consumer", Value: []byte(c.service)}),
	}
	if err := c.prod.ProduceSync(ctx, dlq).FirstErr(); err != nil {
		zlog.Ctx(ctx).Error("cannot park event in dlq; it is lost", zlog.Str("topic", r.Topic), zlog.Str("key", string(r.Key)), zlog.Err(err))
	}
}

// Close stops the consumers (waiting for the message in hand) and closes
// the producer.
func (c *Client) Close() error {
	c.mu.Lock()
	stop, cons := c.stop, c.cons
	c.mu.Unlock()
	if stop != nil {
		stop()
	}
	done := make(chan struct{})
	go func() { c.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		zlog.Warn("kafka consumers did not stop in time")
	}
	for _, cl := range cons {
		cl.Close()
	}
	if c.prod != nil {
		c.prod.Close()
	}
	return nil
}

// ErrNotEnabled is what a service gets when it publishes without kafka.enabled.
var ErrNotEnabled = errors.New("kafkax: kafka.enabled is false in conf/<env>.yaml")
