package kitexx

import (
	"context"
	"os"

	"github.com/sezznaw/devkit-common/zlog"
)

// DLQFlag reports whether the process was started with --dlq and returns
// what follows it: the dead-letter tool's command (list, show, replay,
// drop) and its arguments. main.go runs it after app.Setup (the
// subscriptions name the dead-letter topics) and exits.
func DLQFlag() ([]string, bool) {
	for i, a := range os.Args[1:] {
		if a == "--dlq" {
			return os.Args[i+2:], true
		}
	}
	return nil, false
}

// RunDLQ runs the dead-letter tool (rt.Kafka.DLQ) and returns the exit
// code. In a deployment: `kubectl exec deploy/<service> -- <binary> --dlq
// list` runs it inside the service's own pod, with its configuration.
func (rt *Runtime) RunDLQ(args []string) int {
	defer zlog.Sync()
	code := 0
	if rt.Kafka == nil {
		os.Stderr.WriteString("kafka.enabled is false: this service has no dead-letter queues\n")
		code = 1
	} else if err := rt.Kafka.DLQ(context.Background(), args, os.Stdout); err != nil {
		os.Stderr.WriteString("dlq: " + err.Error() + "\n")
		code = 1
	}
	RunShutdownHooks()
	nacosCloseShared()
	return code
}
