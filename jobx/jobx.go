// Package jobx is scheduled work for a service: a service lists its jobs
// (app.Jobs), the framework runs one per invocation (`<binary> --job=<name>`,
// which is what a Kubernetes CronJob does on schedule) with a root span, a
// log with the run id, a lock against overlapping runs, a timeout, a recovery
// from panics and an exit code, and prints the list (`--list-jobs`) for the
// deployment to turn into CronJobs. A job is a plain function; the schedule
// lives next to it in code.
package jobx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/robfig/cron/v3"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/sezznaw/devkit-common/zlog"
)

// Job is one scheduled task of a service.
type Job struct {
	// Name identifies the job: lowercase letters, digits and dashes (it is
	// a Kubernetes name), unique within the service.
	Name string
	// Schedule is a cron expression, five fields, in UTC: "*/5 * * * *".
	Schedule string
	// Timeout is how long one run may take; the context is cancelled then
	// and the run counts as failed. Default 10 minutes.
	Timeout time.Duration
	// Description is for people, shown in the list.
	Description string
	// Run does the work. It gets a context that carries the run's trace and
	// logger and is cancelled at the timeout: respect it, stop at a chunk
	// boundary, and make the work resumable, because a run can be retried.
	Run func(ctx context.Context) error
}

// Spec is what the deployment needs to know about a job: the list output.
type Spec struct {
	Name           string `json:"name"`
	Schedule       string `json:"schedule"`
	TimeoutSeconds int    `json:"timeout_seconds"`
	Description    string `json:"description,omitempty"`
}

const (
	defaultTimeout = 10 * time.Minute
	// LockTTLSlack is added to the timeout for the lock, so a run that hits
	// its timeout still owns the lock while it winds down.
	lockTTLSlack = time.Minute
	tracerName   = "github.com/sezznaw/devkit-common/jobx"
)

var nameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
var cronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

func (j Job) timeout() time.Duration {
	if j.Timeout > 0 {
		return j.Timeout
	}
	return defaultTimeout
}

// Validate checks every job: a valid name, unique, a valid schedule, a Run.
// It runs at start (every start, so a bad job is a start failure, not a
// surprise at 3 am) and before the list is printed.
func Validate(jobs []Job) error {
	seen := map[string]bool{}
	for _, j := range jobs {
		if !nameRE.MatchString(j.Name) {
			return fmt.Errorf("jobx: job name %q: lowercase letters, digits and dashes, at most 63 characters", j.Name)
		}
		if seen[j.Name] {
			return fmt.Errorf("jobx: job %q is listed twice", j.Name)
		}
		seen[j.Name] = true
		if _, err := cronParser.Parse(j.Schedule); err != nil {
			return fmt.Errorf("jobx: job %q: schedule %q: %w (five fields, e.g. \"*/5 * * * *\")", j.Name, j.Schedule, err)
		}
		if j.Run == nil {
			return fmt.Errorf("jobx: job %q has no Run", j.Name)
		}
	}
	return nil
}

// Specs is the list output.
func Specs(jobs []Job) []Spec {
	out := make([]Spec, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, Spec{Name: j.Name, Schedule: j.Schedule, TimeoutSeconds: int(j.timeout().Seconds()), Description: j.Description})
	}
	return out
}

// WriteList prints the jobs as JSON, {"jobs": [...]}, which is what the CI
// puts next to the image tag and the chart turns into CronJobs. JSON is YAML.
func WriteList(w io.Writer, jobs []Job) error {
	if err := Validate(jobs); err != nil {
		return err
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(map[string]any{"jobs": Specs(jobs)})
}

// Find returns the job of that name.
func Find(jobs []Job, name string) (Job, error) {
	for _, j := range jobs {
		if j.Name == name {
			return j, nil
		}
	}
	names := make([]string, 0, len(jobs))
	for _, j := range jobs {
		names = append(names, j.Name)
	}
	return Job{}, fmt.Errorf("jobx: no job %q; this service has %v", name, names)
}

// Locker is what keeps two runs of one job apart: Runtime.Redis, or nil
// when the service has no Redis (then CronJob's own concurrencyPolicy is the
// only guard, which covers the scheduled runs).
type Locker interface {
	SetNX(ctx context.Context, key string, value any, expiration time.Duration) *redis.BoolCmd
	Del(ctx context.Context, keys ...string) *redis.IntCmd
}

// ErrLocked: another run of the job holds the lock.
var ErrLocked = errors.New("jobx: another run of this job is in progress")

// Execute runs one job: a root span and a logger with the run id, the lock,
// the timeout, a recovered panic as a failure, and one record at the end
// with the result and the duration. The error is what the process exits
// with (non-nil = exit 1), which is how Kubernetes learns the run failed.
func Execute(ctx context.Context, service string, jobs []Job, name string, lock Locker) (err error) {
	if err := Validate(jobs); err != nil {
		return err
	}
	j, err := Find(jobs, name)
	if err != nil {
		return err
	}
	runID := uuid.NewString()
	ctx, span := otel.Tracer(tracerName).Start(ctx, "job "+j.Name, trace.WithNewRoot(),
		trace.WithAttributes(attribute.String("job.name", j.Name), attribute.String("job.run_id", runID), attribute.String("service.name", service)))
	defer span.End()
	// The trace id of the run: the span's when tracing is on, else the run
	// id without dashes (32 hex characters, the shape of a trace id), so the
	// records of one run can always be found by trace_id.
	traceID := strings.ReplaceAll(runID, "-", "")
	if sc := span.SpanContext(); sc.IsValid() {
		traceID = sc.TraceID().String()
	}
	ctx = zlog.CtxWith(ctx, zlog.Str("job", j.Name), zlog.Str("run_id", runID), zlog.Str("trace_id", traceID))
	log := zlog.Ctx(ctx)

	key := fmt.Sprintf("job:%s:%s", service, j.Name)
	if lock != nil {
		ok, lerr := lock.SetNX(ctx, key, runID, j.timeout()+lockTTLSlack).Result()
		if lerr != nil {
			log.Error("job lock unavailable; not running", zlog.Err(lerr))
			return fmt.Errorf("jobx: lock: %w", lerr)
		}
		if !ok {
			log.Warn("job skipped: another run holds the lock")
			return ErrLocked
		}
		defer lock.Del(context.WithoutCancel(ctx), key)
	} else {
		log.Warn("job runs without a lock: redis.enabled is false; the CronJob's concurrencyPolicy is the only guard")
	}

	log.Info("job started", zlog.Str("schedule", j.Schedule), zlog.Dur("timeout", j.timeout()))
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, j.timeout())
	defer cancel()
	err = runSafely(ctx, j)
	took := time.Since(start)
	switch {
	case err == nil:
		log.Info("job finished", zlog.Dur("took", took))
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		err = fmt.Errorf("jobx: job %q timed out after %s: %w", j.Name, j.timeout(), err)
		span.RecordError(err)
		span.SetStatus(codes.Error, "timeout")
		log.Error("job timed out", zlog.Dur("took", took), zlog.Err(err))
	default:
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		log.Error("job failed", zlog.Dur("took", took), zlog.Err(err))
	}
	return err
}

func runSafely(ctx context.Context, j Job) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("job panicked: %v", p)
		}
	}()
	return j.Run(ctx)
}
