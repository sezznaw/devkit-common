// Package metricsx is the Prometheus side of a service: one registry for
// the process (Go runtime and process collectors, the request metrics the
// kitexx and hertzx middlewares record, the pool statistics of MySQL, the
// command metrics of Redis, the client metrics of Kafka) and the HTTP
// listener that serves it on /metrics, on a port of its own so that it is
// never reachable through the service's ingress. A service fills in
// `metrics:` in its configuration; the deployment scrapes the port.
package metricsx

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Config is the `metrics:` section of a service configuration.
type Config struct {
	// Enabled serves /metrics. Off by default: a laptop running several
	// services would fight over the port, and nothing scrapes it there.
	Enabled bool `yaml:"enabled"`
	// Addr is where /metrics listens: a port ("9091", the default) or
	// "host:port". Kept apart from service.addr on purpose.
	Addr string `yaml:"addr"`
}

// DefaultPort is the metrics port when metrics.addr is empty; the service
// chart scrapes it.
const DefaultPort = 9091

// ListenAddr turns metrics.addr into an address: "" is ":9091", a bare
// number is ":<number>", anything else must be host:port.
func (c Config) ListenAddr() (string, error) {
	a := strings.TrimSpace(c.Addr)
	if a == "" {
		return ":" + strconv.Itoa(DefaultPort), nil
	}
	if p, err := strconv.Atoi(a); err == nil {
		if p < 1 || p > 65535 {
			return "", fmt.Errorf("metricsx: metrics.addr %q: a port is 1 to 65535", a)
		}
		return ":" + a, nil
	}
	if _, _, err := net.SplitHostPort(a); err != nil {
		return "", fmt.Errorf("metricsx: metrics.addr %q: a port (\"9091\") or \"host:port\"", a)
	}
	return a, nil
}

// Registry holds every metric of the process. Packages register their own
// collectors with it (promauto.With(Registry)); the default Prometheus
// registry is not used, so a library that registers there is not served.
var Registry = func() *prometheus.Registry {
	r := prometheus.NewRegistry()
	r.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	return r
}()

// DurationBuckets are the latency buckets of every duration histogram, 1ms
// to 10s: what a request, a query or a command to a cache takes.
var DurationBuckets = []float64{.001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10}

// The request metrics. The middlewares in kitexx and hertzx record them;
// `code` is the business code of a BizStatusError, "ok", or "error".
var (
	RPCServerRequests = promauto.With(Registry).NewCounterVec(prometheus.CounterOpts{
		Name: "rpc_server_requests_total", Help: "RPC requests handled, by method and result code.",
	}, []string{"rpc_service", "rpc_method", "code"})
	RPCServerRejected = promauto.With(Registry).NewCounterVec(prometheus.CounterOpts{
		Name: "rpc_server_rejected_total", Help: "Requests an instance refused for its limits (code 5003), by method and reason: in_flight, qps, connections.",
	}, []string{"rpc_method", "reason"})
	RPCServerDuration = promauto.With(Registry).NewHistogramVec(prometheus.HistogramOpts{
		Name: "rpc_server_duration_seconds", Help: "Time to handle an RPC request.", Buckets: DurationBuckets,
	}, []string{"rpc_service", "rpc_method"})
	RPCClientRequests = promauto.With(Registry).NewCounterVec(prometheus.CounterOpts{
		Name: "rpc_client_requests_total", Help: "RPC calls made to other services, by service, method and result code.",
	}, []string{"rpc_service", "rpc_method", "code"})
	RPCClientDuration = promauto.With(Registry).NewHistogramVec(prometheus.HistogramOpts{
		Name: "rpc_client_duration_seconds", Help: "Time an RPC call to another service took.", Buckets: DurationBuckets,
	}, []string{"rpc_service", "rpc_method"})
	HTTPServerRequests = promauto.With(Registry).NewCounterVec(prometheus.CounterOpts{
		Name: "http_server_requests_total", Help: "HTTP requests handled, by method, route and status.",
	}, []string{"http_method", "http_route", "status"})
	HTTPServerDuration = promauto.With(Registry).NewHistogramVec(prometheus.HistogramOpts{
		Name: "http_server_duration_seconds", Help: "Time to handle an HTTP request.", Buckets: DurationBuckets,
	}, []string{"http_method", "http_route"})
	HTTPRequestsRejected = promauto.With(Registry).NewCounterVec(prometheus.CounterOpts{
		Name: "http_requests_rejected_total", Help: "Requests the gateway's guard refused, by reason: rate_limit (429), timeout (504).",
	}, []string{"reason"})
	HTTPClientRequests = promauto.With(Registry).NewCounterVec(prometheus.CounterOpts{
		Name: "http_client_requests_total", Help: "Calls to external HTTP APIs (httpx), by provider, method and status (\"error\": no response).",
	}, []string{"provider", "http_method", "status"})
	HTTPClientDuration = promauto.With(Registry).NewHistogramVec(prometheus.HistogramOpts{
		Name: "http_client_duration_seconds", Help: "Time a call to an external HTTP API took.", Buckets: DurationBuckets,
	}, []string{"provider", "http_method"})
	HTTPClientRejected = promauto.With(Registry).NewCounterVec(prometheus.CounterOpts{
		Name: "http_client_rejected_total", Help: "Calls to external APIs not sent: circuit_open (the provider is failing) or in_flight (max_concurrent reached and the context ended).",
	}, []string{"provider", "reason"})
	HTTPClientBreakerState = promauto.With(Registry).NewGaugeVec(prometheus.GaugeOpts{
		Name: "http_client_breaker_state", Help: "Circuit breaker of a provider: 0 closed (normal), 1 half-open (probing), 2 open (failing fast).",
	}, []string{"provider"})
	CallbacksReceived = promauto.With(Registry).NewCounterVec(prometheus.CounterOpts{
		Name: "callbacks_received_total", Help: "Provider callbacks received (webhookx), by provider, route and result: ok, duplicate, bad_source, bad_signature, too_large, archive_failed, dedupe_unavailable, handler_error.",
	}, []string{"provider", "http_route", "result"})
	S3Requests = promauto.With(Registry).NewCounterVec(prometheus.CounterOpts{
		Name: "s3_requests_total", Help: "Object storage calls (s3x), by operation and result: ok, not_found, error.",
	}, []string{"op", "status"})
	S3Duration = promauto.With(Registry).NewHistogramVec(prometheus.HistogramOpts{
		Name: "s3_request_duration_seconds", Help: "Time an object storage call took.", Buckets: DurationBuckets,
	}, []string{"op"})
	KafkaEventsPublished = promauto.With(Registry).NewCounterVec(prometheus.CounterOpts{
		Name: "kafka_events_published_total", Help: "Events published by kafkax, by topic and result.",
	}, []string{"topic", "status"})
	KafkaOutboxPublished = promauto.With(Registry).NewCounterVec(prometheus.CounterOpts{
		Name: "kafka_outbox_published_total", Help: "Outbox rows the relay published, by topic and result.",
	}, []string{"topic", "status"})
	KafkaOutboxPending = promauto.With(Registry).NewGauge(prometheus.GaugeOpts{
		Name: "kafka_outbox_pending", Help: "Outbox rows not yet published.",
	})
	KafkaOutboxOldestAge = promauto.With(Registry).NewGauge(prometheus.GaugeOpts{
		Name: "kafka_outbox_oldest_age_seconds", Help: "Age of the oldest unpublished outbox row; 0 when none.",
	})
	DelayTasksRun = promauto.With(Registry).NewCounterVec(prometheus.CounterOpts{
		Name: "delay_tasks_run_total", Help: "Delayed tasks run (delayx), by kind and result: ok, retry, failed, no_handler.",
	}, []string{"kind", "result"})
	DelayTasksPending = promauto.With(Registry).NewGauge(prometheus.GaugeOpts{
		Name: "delay_tasks_pending", Help: "Delayed tasks waiting for their time (or a retry).",
	})
	DelayTasksFailed = promauto.With(Registry).NewGauge(prometheus.GaugeOpts{
		Name: "delay_tasks_failed", Help: "Delayed tasks that used up their attempts and need a person; alert when above 0.",
	})
	DelayTasksOverdue = promauto.With(Registry).NewGauge(prometheus.GaugeOpts{
		Name: "delay_tasks_overdue_seconds", Help: "How late the oldest due-but-not-run delayed task is; 0 when none.",
	})
	KafkaEventsHandled = promauto.With(Registry).NewCounterVec(prometheus.CounterOpts{
		Name: "kafka_events_handled_total", Help: "Events consumed by kafkax, by topic and outcome: ok, retried, dlq.",
	}, []string{"topic", "result"})
	KafkaConsumerLag = promauto.With(Registry).NewGaugeVec(prometheus.GaugeOpts{
		Name: "kafka_consumer_lag", Help: "Records not yet consumed behind the end of the topic (largest partition), after each poll.",
	}, []string{"topic"})
	KafkaHandleDuration = promauto.With(Registry).NewHistogramVec(prometheus.HistogramOpts{
		Name: "kafka_event_handle_duration_seconds", Help: "Time a handler took for one event (all attempts).", Buckets: DurationBuckets,
	}, []string{"topic"})
	KafkaRecordsProduced = promauto.With(Registry).NewCounterVec(prometheus.CounterOpts{
		Name: "kafka_records_produced_total", Help: "Records written to brokers, by topic (franz-go hook).",
	}, []string{"topic"})
	KafkaRecordsFetched = promauto.With(Registry).NewCounterVec(prometheus.CounterOpts{
		Name: "kafka_records_fetched_total", Help: "Records read from brokers, by topic (franz-go hook).",
	}, []string{"topic"})
	KafkaBrokerConnects = promauto.With(Registry).NewCounterVec(prometheus.CounterOpts{
		Name: "kafka_broker_connects_total", Help: "Connections opened to brokers, by result.",
	}, []string{"status"})
	Locks = promauto.With(Registry).NewCounterVec(prometheus.CounterOpts{
		Name: "locks_total", Help: "Distributed locks (redisx.WithLock), by name and result: acquired, busy, lost, error.",
	}, []string{"name", "result"})
	LockWait = promauto.With(Registry).NewHistogramVec(prometheus.HistogramOpts{
		Name: "lock_wait_seconds", Help: "Time spent waiting for a distributed lock before acquiring it.", Buckets: DurationBuckets,
	}, []string{"name"})
	CacheRequests = promauto.With(Registry).NewCounterVec(prometheus.CounterOpts{
		Name: "cache_requests_total", Help: "Read-through cache lookups (redisx.Cache), by cache name and result: hit, miss, negative, error.",
	}, []string{"name", "result"})
	ReportQueries = promauto.With(Registry).NewCounterVec(prometheus.CounterOpts{
		Name: "report_queries_total", Help: "Queries on the report database (starrocksx), by result: ok, error.",
	}, []string{"status"})
	ReportQueryDuration = promauto.With(Registry).NewHistogram(prometheus.HistogramOpts{
		Name: "report_query_duration_seconds", Help: "Time a report query took.", Buckets: []float64{.01, .05, .1, .25, .5, 1, 2.5, 5, 10, 30},
	})
	RedisCommands = promauto.With(Registry).NewCounterVec(prometheus.CounterOpts{
		Name: "redis_commands_total", Help: "Redis commands sent, by command and result.",
	}, []string{"cmd", "status"})
	RedisDuration = promauto.With(Registry).NewHistogramVec(prometheus.HistogramOpts{
		Name: "redis_command_duration_seconds", Help: "Time a Redis command took.", Buckets: DurationBuckets,
	}, []string{"cmd"})
)

// RegisterDBStats serves the connection pool statistics of a database/sql
// pool as go_sql_* (open, in use, idle, waits). name tells the pools apart.
func RegisterDBStats(db *sql.DB, name string) {
	Register(collectors.NewDBStatsCollector(db, name))
}

// Register adds a collector; a collector registered twice (two pools of one
// name, a test that opens twice) is not an error, the first one stays.
func Register(c prometheus.Collector) {
	if err := Registry.Register(c); err != nil {
		var are prometheus.AlreadyRegisteredError
		if !errors.As(err, &are) {
			panic(err)
		}
	}
}

// Handler serves the registry: what Serve mounts on /metrics, exposed for
// a program that already has an HTTP server of its own.
func Handler() http.Handler {
	return promhttp.HandlerFor(Registry, promhttp.HandlerOpts{})
}

// Serve listens on cfg's address and serves /metrics (and /healthz, a 200
// for probes) until the returned function is called. The listener is
// opened here, so a port that is taken is this call's error.
func Serve(cfg Config) (addr string, shutdown func(context.Context) error, err error) {
	addr, err = cfg.ListenAddr()
	if err != nil {
		return "", nil, err
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return "", nil, fmt.Errorf("metricsx: listen on %s: %w", addr, err)
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	return ln.Addr().String(), srv.Shutdown, nil
}
