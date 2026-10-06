package kitexx

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/sezznaw/devkit-common/jobx"
	"github.com/sezznaw/devkit-common/zlog"
)

// JobFlags reads the two job arguments off the command line, before the
// configuration is loaded: --list-jobs (print the jobs and exit, what the CI
// does with the freshly built binary) and --job=<name> (run that job and
// exit, what a CronJob does). Anything else on the command line is left to
// the service.
func JobFlags() (name string, list bool) {
	for _, a := range os.Args[1:] {
		switch {
		case a == "--list-jobs":
			list = true
		case strings.HasPrefix(a, "--job="):
			name = strings.TrimPrefix(a, "--job=")
		}
	}
	return
}

// ListJobs prints the jobs to stdout as the deployment wants them and
// returns the exit code. It needs no configuration: the jobs are listed
// with an empty Runtime, so app.Jobs must only register, never connect.
func ListJobs(jobs []jobx.Job) int {
	if err := jobx.WriteList(os.Stdout, jobs); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

// RunJob runs one job in this process (the server is not started and the
// service is not registered; what the configuration enables is open) and
// returns the exit code. The shutdown hooks run afterwards, so spans and
// logs are flushed and connections closed.
func (rt *Runtime) RunJob(name string) int {
	defer zlog.Sync()
	jobs := rt.jobs
	if err := jobx.Validate(jobs); err != nil {
		zlog.Error("jobs", zlog.Err(err))
		RunShutdownHooks()
		return 1
	}
	var lock jobx.Locker
	if rt.Redis != nil {
		lock = rt.Redis
	}
	err := jobx.Execute(context.Background(), rt.Config.Service.Name, jobs, name, lock)
	RunShutdownHooks()
	nacosCloseShared()
	if err != nil {
		return 1
	}
	return 0
}

// SetJobs hands the service's jobs to the Runtime (main does it with
// app.Jobs(rt)); they are validated at every start.
func (rt *Runtime) SetJobs(jobs []jobx.Job) error {
	if err := jobx.Validate(jobs); err != nil {
		return err
	}
	rt.jobs = jobs
	if len(jobs) > 0 {
		names := make([]string, 0, len(jobs))
		for _, j := range jobs {
			names = append(names, j.Name+" @ "+j.Schedule)
		}
		zlog.Info("jobs registered", zlog.Any("jobs", names))
	}
	return nil
}
