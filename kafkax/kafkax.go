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
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/sasl/scram"
	"github.com/twmb/franz-go/plugin/kotel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"

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
}

const (
	SourceStatic   = "static"
	SourcePlatform = "platform"
	RoleTenant     = "tenant"
	RolePlatform   = "platform"
	// DLQSuffix is appended to a topic for the messages its handler could not process.
	DLQSuffix = ".dlq"

	defaultRetries = 3
)

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
	c := &Client{cfg: cfg, target: t, service: service, tenant: tenant, tracer: tracer, hooks: hooks}
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
		zlog.Ctx(ctx).Error("event not published", zlog.Str("topic", topic), zlog.Str("type", eventType), zlog.Str("key", key), zlog.Err(err))
		return fmt.Errorf("kafkax: publish %s to %s: %w", eventType, topic, err)
	}
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
		if err := cl.CommitUncommittedOffsets(ctx); err != nil && ctx.Err() == nil {
			zlog.Error("commit failed", zlog.Str("topic", s.topic), zlog.Err(err))
		}
		cl.AllowRebalance()
	}
}

// handle processes one record: a span under the producer's trace, the
// envelope decoded, the handler retried, the message parked in the DLQ when
// it keeps failing. It never panics out and never blocks the partition
// forever: the worst case is a message in the DLQ and an error record.
func (c *Client) handle(ctx context.Context, r *kgo.Record, h Handler) {
	ctx, span := c.tracer.WithProcessSpan(r)
	defer span.End()
	start := time.Now()
	ev := &Event{Topic: r.Topic, Key: string(r.Key), Partition: r.Partition, Offset: r.Offset}
	log := zlog.Ctx(ctx).With(zlog.Str("topic", r.Topic), zlog.Str("key", ev.Key), zlog.Int("partition", r.Partition), zlog.Int("offset", r.Offset))
	if err := json.Unmarshal(r.Value, ev); err != nil {
		log.Error("event is not an envelope; parked", zlog.Err(err))
		c.park(ctx, r, err)
		return
	}
	log = log.With(zlog.Str("type", ev.Type), zlog.Str("event_id", ev.ID))
	span.SetAttributes(attribute.String("event.type", ev.Type), attribute.String("event.id", ev.ID))

	var err error
	for attempt := 1; attempt <= c.cfg.retries(); attempt++ {
		if err = safely(ctx, h, ev); err == nil {
			log.Info("event handled", zlog.Int("attempt", attempt), zlog.Dur("latency", time.Since(start)))
			return
		}
		if ctx.Err() != nil {
			return
		}
		log.Warn("handler failed", zlog.Int("attempt", attempt), zlog.Err(err))
		time.Sleep(time.Duration(attempt) * 200 * time.Millisecond)
	}
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
	log.Error("event parked in dlq after retries", zlog.Err(err))
	c.park(ctx, r, err)
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
