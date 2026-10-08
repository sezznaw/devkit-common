package kafkax

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/twmb/franz-go/pkg/kgo"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/sezznaw/devkit-common/config"
	"github.com/sezznaw/devkit-common/metricsx"
	"github.com/sezznaw/devkit-common/zlog"
)

// The outbox: an event that belongs with a database change is written to
// the `outbox` table in the same transaction as the change, and a relay in
// the same process publishes the rows to Kafka afterwards. Either both the
// change and the event happen, or neither; a crash between the two cannot
// lose the event or publish it for a change that rolled back. Publish
// (direct) stays for events without a transaction behind them.
//
// Delivery is at least once: a crash after producing but before marking
// the row sent publishes it again, with the same event id, which is why
// consumers deduplicate on ev.ID. Order is per key within a topic, as
// with Publish; rows are relayed in id order.
//
// outbox：和数据库变更同属一件事的事件，在变更的事务里写进 outbox 表，进程里的转发器随后
// 发到 Kafka。要么变更和事件都发生，要么都不发生；两者之间崩溃既不会丢事件，也不会为已
// 回滚的变更发事件。没有事务的事件仍用 Publish。至少一次投递，event id 不变，消费者按 ev.ID 去重。

// OutboxConfig is the `kafka.outbox` section: the relay that publishes
// outbox rows. On by default when the service has both Kafka and MySQL.
type OutboxConfig struct {
	// Enabled: false turns the relay off (a service that only consumes).
	// Default true when kafka and mysql are enabled.
	Enabled *bool `yaml:"enabled"`
	// PollInterval is how often the relay looks for unsent rows when
	// nothing nudged it. Default 500ms; PublishTx nudges it right away, so
	// the usual latency is the commit plus one round.
	PollInterval config.Duration `yaml:"poll_interval"`
	// Batch is how many rows one round takes. Default 100.
	Batch int `yaml:"batch"`
	// Retention is how long sent rows are kept for inspection before the
	// hourly cleanup deletes them. Default 168h (7 days).
	Retention config.Duration `yaml:"retention"`
	// PublishTimeout bounds one row's produce; a row that fails waits
	// 1s, 2s, 4s... up to 5 minutes before the next try (next_attempt_at),
	// so one broken topic does not hold the others back. Default 10s.
	PublishTimeout config.Duration `yaml:"publish_timeout"`
}

func (c OutboxConfig) enabled() bool { return c.Enabled == nil || *c.Enabled }

// EnabledOrDefault reports whether the relay should run: on unless
// kafka.outbox.enabled is false.
func (c OutboxConfig) EnabledOrDefault() bool  { return c.enabled() }
func (c OutboxConfig) interval() time.Duration { return c.PollInterval.Or(500 * time.Millisecond) }
func (c OutboxConfig) batch() int {
	if c.Batch <= 0 {
		return 100
	}
	return c.Batch
}
func (c OutboxConfig) retention() time.Duration      { return c.Retention.Or(168 * time.Hour) }
func (c OutboxConfig) publishTimeout() time.Duration { return c.PublishTimeout.Or(10 * time.Second) }

// backoff is the wait before the next try of a failed row: 1s, 2s, 4s ...
// capped at 5 minutes.
func backoff(attempts int) time.Duration {
	d := time.Second << uint(min(attempts, 10))
	if d > 5*time.Minute {
		d = 5 * time.Minute
	}
	return d
}

// OutboxRow is one row of the outbox table (OutboxDDL).
type OutboxRow struct {
	ID            int64      `gorm:"column:id;primaryKey;autoIncrement"`
	EventID       string     `gorm:"column:event_id"`
	Topic         string     `gorm:"column:topic"`
	Key           string     `gorm:"column:msg_key"`
	EventType     string     `gorm:"column:event_type"`
	Payload       []byte     `gorm:"column:payload"`
	Trace         string     `gorm:"column:trace"`
	CreatedAt     time.Time  `gorm:"column:created_at"`
	SentAt        *time.Time `gorm:"column:sent_at"`
	Attempts      int        `gorm:"column:attempts"`
	NextAttemptAt time.Time  `gorm:"column:next_attempt_at"`
	LastError     string     `gorm:"column:last_error"`
}

// TableName is "outbox" in the service's database.
func (OutboxRow) TableName() string { return "outbox" }

// OutboxDDL is the table every database that uses PublishTx needs; the
// project's migrations create it (idl README, 事件规范).
const OutboxDDL = `CREATE TABLE IF NOT EXISTS outbox (
  id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  event_id    CHAR(36)     NOT NULL COMMENT '信封 id，消费者按它去重',
  topic       VARCHAR(128) NOT NULL,
  msg_key     VARCHAR(255) NOT NULL DEFAULT '' COMMENT '分区键（实体 id）',
  event_type  VARCHAR(128) NOT NULL,
  payload     JSON         NOT NULL,
  trace       VARCHAR(255) NOT NULL DEFAULT '' COMMENT 'traceparent，关联发起请求的 trace',
  created_at  DATETIME(3)  NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  sent_at     DATETIME(3)  NULL,
  attempts    INT          NOT NULL DEFAULT 0,
  next_attempt_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) COMMENT '失败后退避到这个时间再试',
  last_error  VARCHAR(512) NOT NULL DEFAULT '',
  KEY idx_outbox_unsent (sent_at, next_attempt_at, id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci`

// PublishTx writes the event into the outbox table within tx (a *gorm.DB
// inside Transaction, or repo.WithTx's handle) so that it is published
// if and only if the transaction commits. The relay sends it right after.
//
// PublishTx 在事务 tx 里把事件写进 outbox 表：事务提交才会发出，回滚就没有。转发器随后发送。
func (c *Client) PublishTx(ctx context.Context, tx *gorm.DB, topic, key, eventType string, data any) error {
	if tx == nil {
		return errors.New("kafkax: PublishTx needs the transaction's *gorm.DB")
	}
	payload, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("kafkax: encode %s: %w", eventType, err)
	}
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	now := time.Now()
	row := &OutboxRow{EventID: uuid.NewString(), Topic: topic, Key: key, EventType: eventType, Payload: payload, Trace: carrier.Get("traceparent"), CreatedAt: now, NextAttemptAt: now}
	if err := tx.WithContext(ctx).Create(row).Error; err != nil {
		return fmt.Errorf("kafkax: outbox insert %s: %w", eventType, err)
	}
	zlog.Ctx(ctx).Info("event queued", zlog.Str("topic", topic), zlog.Str("type", eventType), zlog.Str("key", key), zlog.Str("event_id", row.EventID))
	c.nudge()
	return nil
}

func (c *Client) nudge() {
	c.outboxMu.Lock()
	ch := c.outboxNudge
	c.outboxMu.Unlock()
	if ch == nil {
		return
	}
	select {
	case ch <- struct{}{}:
	default:
	}
}

// StartOutbox runs the relay until ctx ends: every round takes up to
// cfg.Batch unsent rows (FOR UPDATE SKIP LOCKED, so replicas share the
// work), publishes them, marks them sent; a failed row keeps its attempt
// count and error and is retried next round. Sent rows older than the
// retention are deleted hourly. The runtime starts it when the service has
// Kafka and MySQL; StopOutbox waits for the current round.
func (c *Client) StartOutbox(ctx context.Context, db *gorm.DB, cfg OutboxConfig) {
	ctx, cancel := context.WithCancel(ctx)
	c.outboxMu.Lock()
	c.outboxNudge = make(chan struct{}, 1)
	c.outboxStop = cancel
	c.outboxCfg = cfg
	nudge := c.outboxNudge
	c.outboxMu.Unlock()
	c.outboxWG.Add(1)
	go func() {
		defer c.outboxWG.Done()
		ticker := time.NewTicker(cfg.interval())
		defer ticker.Stop()
		cleanup := time.NewTicker(time.Hour)
		defer cleanup.Stop()
		zlog.Info("outbox relay on", zlog.Dur("poll_interval", cfg.interval()), zlog.Int("batch", int64(cfg.batch())), zlog.Dur("retention", cfg.retention()))
		for {
			for {
				n, err := c.relayOnce(ctx, db, cfg.batch())
				if err != nil && ctx.Err() == nil {
					zlog.Error("outbox relay round", zlog.Err(err))
				}
				if n < cfg.batch() {
					break
				}
			}
			c.observeOutbox(ctx, db)
			select {
			case <-ctx.Done():
				return
			case <-nudge:
			case <-ticker.C:
			case <-cleanup.C:
				c.cleanupOutbox(ctx, db, cfg.retention())
			}
		}
	}()
}

// StopOutbox stops the relay and waits for the round in progress.
func (c *Client) StopOutbox() {
	c.outboxMu.Lock()
	stop := c.outboxStop
	c.outboxStop = nil
	c.outboxMu.Unlock()
	if stop != nil {
		stop()
	}
	c.outboxWG.Wait()
}

// relayOnce publishes one batch inside a transaction that locks the rows it
// takes; returns how many rows it looked at.
func (c *Client) relayOnce(ctx context.Context, db *gorm.DB, batch int) (int, error) {
	n := 0
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var rows []OutboxRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).
			Where("sent_at IS NULL AND next_attempt_at <= ?", time.Now()).Order("id").Limit(batch).Find(&rows).Error; err != nil {
			return err
		}
		n = len(rows)
		for i := range rows {
			row := &rows[i]
			if err := c.produceRow(ctx, row); err != nil {
				metricsx.KafkaOutboxPublished.WithLabelValues(row.Topic, "error").Inc()
				msg := err.Error()
				if len(msg) > 500 {
					msg = msg[:500]
				}
				if e := tx.Model(row).Updates(map[string]any{"attempts": row.Attempts + 1, "last_error": msg, "next_attempt_at": time.Now().Add(backoff(row.Attempts + 1))}).Error; e != nil {
					return e
				}
				zlog.Ctx(ctx).Error("outbox publish failed", zlog.Str("topic", row.Topic), zlog.Str("type", row.EventType), zlog.Str("event_id", row.EventID), zlog.Int("attempts", int64(row.Attempts+1)), zlog.Err(err))
				continue
			}
			now := time.Now()
			if e := tx.Model(row).Updates(map[string]any{"sent_at": now, "attempts": row.Attempts + 1, "last_error": ""}).Error; e != nil {
				return e
			}
			metricsx.KafkaOutboxPublished.WithLabelValues(row.Topic, "ok").Inc()
		}
		return nil
	})
	return n, err
}

// produceRow sends one row with the same envelope as Publish, under the
// trace of the request that queued it.
func (c *Client) produceRow(ctx context.Context, row *OutboxRow) error {
	ev := Event{ID: row.EventID, Type: row.EventType, TenantID: c.tenant, Time: row.CreatedAt.UnixMilli(), Data: row.Payload}
	value, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	pctx := ctx
	if row.Trace != "" {
		pctx = otel.GetTextMapPropagator().Extract(ctx, propagation.MapCarrier{"traceparent": row.Trace})
	}
	rec := &kgo.Record{
		Topic:   row.Topic,
		Key:     []byte(row.Key),
		Value:   value,
		Context: pctx,
		Headers: []kgo.RecordHeader{
			{Key: "content-type", Value: []byte("application/json")},
			{Key: "event-type", Value: []byte(row.EventType)},
			{Key: "outbox-id", Value: []byte(fmt.Sprint(row.ID))},
		},
	}
	c.outboxMu.Lock()
	timeout := c.outboxCfg.publishTimeout()
	c.outboxMu.Unlock()
	pctx, cancel := context.WithTimeout(pctx, timeout)
	defer cancel()
	rec.Context = pctx
	if err := c.prod.ProduceSync(pctx, rec).FirstErr(); err != nil {
		return err
	}
	zlog.Ctx(pctx).Info("event published", zlog.Str("topic", row.Topic), zlog.Str("type", row.EventType), zlog.Str("key", row.Key), zlog.Str("event_id", row.EventID), zlog.Dur("queued", time.Since(row.CreatedAt)))
	return nil
}

func (c *Client) observeOutbox(ctx context.Context, db *gorm.DB) {
	var pending int64
	if err := db.WithContext(ctx).Model(&OutboxRow{}).Where("sent_at IS NULL").Count(&pending).Error; err != nil {
		return
	}
	metricsx.KafkaOutboxPending.Set(float64(pending))
	var oldest struct{ CreatedAt *time.Time }
	db.WithContext(ctx).Model(&OutboxRow{}).Select("MIN(created_at) AS created_at").Where("sent_at IS NULL").Scan(&oldest)
	if oldest.CreatedAt == nil {
		metricsx.KafkaOutboxOldestAge.Set(0)
	} else {
		metricsx.KafkaOutboxOldestAge.Set(time.Since(*oldest.CreatedAt).Seconds())
	}
}

func (c *Client) cleanupOutbox(ctx context.Context, db *gorm.DB, retention time.Duration) {
	res := db.WithContext(ctx).Where("sent_at IS NOT NULL AND sent_at < ?", time.Now().Add(-retention)).Limit(10000).Delete(&OutboxRow{})
	if res.Error != nil {
		zlog.Error("outbox cleanup", zlog.Err(res.Error))
		return
	}
	if res.RowsAffected > 0 {
		zlog.Info("outbox cleanup", zlog.Int("deleted", res.RowsAffected))
	}
}

// outboxState lives on the Client; declared here to keep the outbox in one file.
type outboxState struct {
	outboxMu    sync.Mutex
	outboxNudge chan struct{}
	outboxStop  context.CancelFunc
	outboxCfg   OutboxConfig
	outboxWG    sync.WaitGroup
}
