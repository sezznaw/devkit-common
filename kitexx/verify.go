package kitexx

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/sezznaw/devkit-common/kafkax"
	"github.com/sezznaw/devkit-common/nacosx"
	"github.com/sezznaw/devkit-common/verifyx"
	"github.com/sezznaw/devkit-common/zlog"
)

// VerifyFlag reports whether the process was started with --verify: run
// the post-deploy checks (app/verify.go plus the framework's own) and
// exit; no server, no registration. The deployment runs it as a PostSync
// Job after every release; `make verify` runs it on a developer's machine.
func VerifyFlag() bool {
	for _, a := range os.Args[1:] {
		if a == "--verify" {
			return true
		}
	}
	return false
}

// RunVerify runs the framework's checks and the service's (app.Checks) and
// returns the exit code: 0 when every check passed, 1 otherwise. The
// shutdown hooks run afterwards.
//
// Framework checks, each only when the configuration enables the thing:
// mysql (ping, and the migrations table is not dirty), redis (ping), kafka
// (connected at start-up; the outbox has no row older than two minutes
// still unsent), nacos (at least one instance of this service is
// registered, i.e. the release is serving), gateway (VERIFY_BASE_URL/ping
// answers 200).
func (rt *Runtime) RunVerify(checks []verifyx.Check) int {
	defer zlog.Sync()
	all := append(rt.frameworkChecks(), checks...)
	zlog.Info("verify starting", zlog.Str("service", rt.Config.Service.Name), zlog.Int("checks", int64(len(all))))
	err := verifyx.Run(context.Background(), all, verifyx.Options{Timeout: 30 * time.Second})
	RunShutdownHooks()
	nacosCloseShared()
	if err != nil {
		zlog.Error("verify", zlog.Err(err))
		return 1
	}
	return 0
}

func (rt *Runtime) frameworkChecks() []verifyx.Check {
	cfg := rt.Config
	var out []verifyx.Check
	if rt.DB != nil {
		out = append(out, verifyx.Check{Name: "mysql", Description: "the database answers and the migrations are clean", Run: func(ctx context.Context) error {
			sqlDB, err := rt.DB.DB()
			if err != nil {
				return err
			}
			if err := sqlDB.PingContext(ctx); err != nil {
				return err
			}
			var dirty int
			row := rt.DB.WithContext(ctx).Raw("SELECT COALESCE(MAX(dirty), 0) FROM schema_migrations").Row()
			if err := row.Scan(&dirty); err != nil {
				return nil // no migrations table: nothing to say
			}
			if dirty != 0 {
				return errors.New("schema_migrations is dirty: a migration failed half way")
			}
			return nil
		}})
	}
	if rt.Redis != nil {
		out = append(out, verifyx.Check{Name: "redis", Description: "redis answers", Run: func(ctx context.Context) error {
			return rt.Redis.Ping(ctx).Err()
		}})
	}
	if rt.Kafka != nil && rt.DB != nil && cfg.Kafka.Outbox.EnabledOrDefault() {
		out = append(out, verifyx.Check{Name: "outbox", Description: "no event older than two minutes is still unpublished", Run: func(ctx context.Context) error {
			var n int64
			err := rt.DB.WithContext(ctx).Model(&kafkax.OutboxRow{}).Where("sent_at IS NULL AND created_at < ?", time.Now().Add(-2*time.Minute)).Count(&n).Error
			if err != nil {
				return nil // no outbox table: the service does not use it
			}
			if n > 0 {
				return fmt.Errorf("%d events older than two minutes are still unpublished", n)
			}
			return nil
		}})
	}
	if rt.Delay != nil && rt.Delay.Handlers() > 0 {
		out = append(out, verifyx.Check{Name: "delay", Description: "no delayed task is more than two minutes overdue or failed for good", Run: func(ctx context.Context) error {
			late, err := rt.Delay.Overdue(ctx)
			if err != nil {
				return err
			}
			if late > 2*time.Minute {
				return fmt.Errorf("the oldest due task has waited %s", late.Round(time.Second))
			}
			n, err := rt.Delay.FailedCount(ctx)
			if err != nil {
				return err
			}
			if n > 0 {
				return fmt.Errorf("%d delayed tasks failed for good (status failed): fix and reschedule", n)
			}
			return nil
		}})
	}
	if !cfg.RegistryDisabled && cfg.Nacos.Registers() {
		out = append(out, verifyx.Check{Name: "nacos", Description: "this release is registered and serving", Run: func(ctx context.Context) error {
			cli, err := nacosx.Shared(cfg.Nacos)
			if err != nil {
				return err
			}
			deadline := time.Now().Add(20 * time.Second)
			for {
				list, err := cli.Instances(cfg.Service.Name)
				if err == nil && len(list) > 0 {
					zlog.Ctx(ctx).Info("instances", zlog.Int("count", int64(len(list))))
					return nil
				}
				if time.Now().After(deadline) {
					if err != nil {
						return err
					}
					return errors.New("no healthy instance registered in Nacos")
				}
				time.Sleep(2 * time.Second)
			}
		}})
	}
	if base := os.Getenv(verifyx.BaseURLEnv); base != "" {
		out = append(out, verifyx.Check{Name: "gateway-ping", Description: base + "/ping answers", Run: func(ctx context.Context) error {
			// The hook starts the moment the rollout is healthy; the Service's
			// endpoints may switch to the new pods a few seconds later.
			gw := verifyx.NewGateway("")
			var err error
			for attempt := 0; attempt < 15; attempt++ {
				if _, err = gw.Get(ctx, "/ping"); err == nil {
					return nil
				}
				select {
				case <-ctx.Done():
					return err
				case <-time.After(2 * time.Second):
				}
			}
			return err
		}})
	}
	return out
}

// GatewayURL is the gateway's own URL for app/verify.go: VERIFY_BASE_URL
// when the deployment set it, else the configured listen port on
// localhost (a developer's machine).
func (rt *Runtime) GatewayURL() string {
	if base := os.Getenv(verifyx.BaseURLEnv); base != "" {
		return base
	}
	addr, err := ListenAddr(rt.Config.Service.Addr)
	if err != nil {
		return "http://127.0.0.1:8080"
	}
	return "http://127.0.0.1:" + strconv.Itoa(addr.Port)
}
