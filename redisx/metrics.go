package redisx

import (
	"context"
	"errors"
	"net"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/sezznaw/devkit-common/metricsx"
)

// metricsHook records redis_commands_total{cmd, status} and
// redis_command_duration_seconds{cmd} for every command; a pipeline counts
// as one command "pipeline". status is "ok", "nil" (a key that is not there,
// which is not a failure) or "error". Open installs it.
type metricsHook struct{}

func (metricsHook) DialHook(next redis.DialHook) redis.DialHook {
	return func(ctx context.Context, network, addr string) (net.Conn, error) { return next(ctx, network, addr) }
}

func (metricsHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		start := time.Now()
		err := next(ctx, cmd)
		record(cmd.Name(), start, err)
		return err
	}
}

func (metricsHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		start := time.Now()
		err := next(ctx, cmds)
		record("pipeline", start, err)
		return err
	}
}

func record(name string, start time.Time, err error) {
	status := "ok"
	switch {
	case err == nil:
	case errors.Is(err, redis.Nil):
		status = "nil"
	default:
		status = "error"
	}
	metricsx.RedisCommands.WithLabelValues(name, status).Inc()
	metricsx.RedisDuration.WithLabelValues(name).Observe(time.Since(start).Seconds())
}
