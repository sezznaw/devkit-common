package kafkax

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/sezznaw/devkit-common/config"
	"github.com/sezznaw/devkit-common/zlog/zlogtest"
)

// TestOutbox runs the relay against a real MySQL (MYSQL_TEST_DSN_ROOT, as
// the mysqlx integration test) and an in-process Kafka: a row queued in a
// committed transaction is published once with its event id; a row in a
// rolled-back transaction never is; a broken topic is retried, not lost.
func TestOutbox(t *testing.T) {
	root := os.Getenv("MYSQL_TEST_DSN_ROOT")
	if root == "" {
		t.Skip("MYSQL_TEST_DSN_ROOT not set")
	}
	zlogtest.Discard(t)
	admin, err := sql.Open("mysql", root)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	for _, q := range []string{"DROP DATABASE IF EXISTS kafkax_outbox", "CREATE DATABASE kafkax_outbox"} {
		if _, err := admin.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	defer admin.Exec("DROP DATABASE IF EXISTS kafkax_outbox")
	db, err := gorm.Open(mysql.Open(root+"kafkax_outbox?parseTime=true&charset=utf8mb4"), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(OutboxDDL).Error; err != nil {
		t.Fatal(err)
	}

	cl := cluster(t, "events.wallet")
	cfg := Config{Enabled: true, Brokers: cl.ListenAddrs()}
	c, err := Open(context.Background(), cfg.Static(), cfg, "wallet", "tenant_a")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	var mu sync.Mutex
	var got []Event
	c.Subscribe("events.wallet", func(ctx context.Context, ev *Event) error {
		mu.Lock()
		got = append(got, *ev)
		mu.Unlock()
		return nil
	})
	if err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	interval := config.Duration(50 * time.Millisecond)
	c.StartOutbox(context.Background(), db, OutboxConfig{PollInterval: interval, Batch: 10, PublishTimeout: config.Duration(time.Second)})
	defer c.StopOutbox()

	// committed: queued in the transaction, published by the relay
	type debited struct {
		UID    int64  `json:"uid"`
		Amount string `json:"amount"`
	}
	var queued OutboxRow
	err = db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("CREATE TABLE IF NOT EXISTS t (id INT PRIMARY KEY)").Error; err != nil {
			return err
		}
		if err := c.PublishTx(context.Background(), tx, "events.wallet", "7", "wallet.debited", debited{UID: 7, Amount: "12.34"}); err != nil {
			return err
		}
		return tx.Order("id DESC").First(&queued).Error
	})
	if err != nil {
		t.Fatal(err)
	}
	// rolled back: never published
	_ = db.Transaction(func(tx *gorm.DB) error {
		if err := c.PublishTx(context.Background(), tx, "events.wallet", "8", "wallet.debited", debited{UID: 8}); err != nil {
			return err
		}
		return errors.New("business rule failed")
	})
	// a topic that does not exist: retried, attempts counted, not lost
	badTopic := "events.nowhere"
	if err := db.Transaction(func(tx *gorm.DB) error {
		return c.PublishTx(context.Background(), tx, badTopic, "9", "x", debited{UID: 9})
	}); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n >= 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("published %d events, want 1 (the committed one): %+v", len(got), got)
	}
	ev := got[0]
	if ev.ID != queued.EventID || ev.Type != "wallet.debited" || ev.TenantID != "tenant_a" || ev.Key != "7" {
		t.Fatalf("envelope %+v, queued %+v", ev, queued)
	}
	var d debited
	if err := ev.Decode(&d); err != nil || d.UID != 7 || d.Amount != "12.34" {
		t.Fatalf("data %+v %v", d, err)
	}
	var sent OutboxRow
	if err := db.Where("event_id = ?", queued.EventID).First(&sent).Error; err != nil || sent.SentAt == nil || sent.Attempts != 1 {
		t.Fatalf("sent row %+v %v", sent, err)
	}
	var unsent int64
	db.Model(&OutboxRow{}).Where("sent_at IS NULL").Count(&unsent)
	if unsent != 1 {
		t.Fatalf("%d unsent rows, want 1 (the bad topic)", unsent)
	}
	var bad OutboxRow
	for time.Now().Before(deadline) {
		db.Where("topic = ?", badTopic).First(&bad)
		if bad.Attempts >= 1 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if bad.Attempts < 1 || bad.LastError == "" || !bad.NextAttemptAt.After(bad.CreatedAt) {
		t.Fatalf("bad topic row not retried with backoff: %+v", bad)
	}
	var rolledBack int64
	db.Model(&OutboxRow{}).Where("msg_key = ?", "8").Count(&rolledBack)
	if rolledBack != 0 {
		t.Fatal("rolled back row must not exist")
	}
}
