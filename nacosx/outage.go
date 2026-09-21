package nacosx

import (
	"strings"
	"sync"
	"time"

	"github.com/sezznaw/devkit-common/zlog"
)

// outageState is what the process knows about the connection to Nacos, for
// the purpose of logging.
//
// While Nacos is away the SDK reports every attempt to reconnect, of each of
// its connections, and every request that fails because of it: measured
// against a stopped server, 119 records at warn and error in 25 seconds, all
// saying the same. What matters is said in three records of our own instead:
// "nacos connection lost" with the SDK's first complaint as the cause, a
// reminder every remindEvery, and "nacos connection restored" with how long it
// took. The SDK's records of the time in between are counted, not written;
// nacos.sdk_log_level: debug shows them all.
type outageState struct {
	mu         sync.Mutex
	down       bool
	since      time.Time
	reminded   time.Time
	suppressed int
	unhealthy  map[*Client]bool
}

var outage = &outageState{unhealthy: map[*Client]bool{}}

// remindEvery is how often a lasting outage is mentioned again.
var remindEvery = 30 * time.Second

// lostSigns are what the SDK says first when the connection breaks. They start
// the outage at once: asking the SDK for its state (watchConnection) notices
// up to healthInterval later, and by then a dozen records are out.
var lostSigns = []string{"fail to connect server", "request stream error", "client not connected"}

// swallow is asked about every record of the SDK at warn or above, and says
// whether it is to be left out.
func (o *outageState) swallow(msg string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.down {
		lost := false
		for _, sign := range lostSigns {
			lost = lost || strings.Contains(msg, sign)
		}
		if !lost {
			return false
		}
		o.begin(msg)
	}
	o.suppressed++
	return true
}

// begin is called with mu held.
func (o *outageState) begin(cause string) {
	if o.down {
		return
	}
	o.down, o.since, o.reminded, o.suppressed = true, time.Now(), time.Now(), 0
	fields := []zlog.Field{}
	if cause != "" {
		cause, _, _ = strings.Cut(cause, "\n")
		fields = append(fields, zlog.Str("cause", clipTo(cause, 300)))
	}
	zlog.Warn("nacos connection lost; instance lists and configurations stay as they are until it is back", fields...)
}

// report is how a client says, every healthInterval, whether its connection is
// up. The outage ends when no client says otherwise, and not before grace has
// passed: an outage that swallow began has to be seen by the clients first.
func (o *outageState) report(c *Client, healthy bool, grace time.Duration) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !healthy {
		o.unhealthy[c] = true
		o.begin("")
		if time.Since(o.reminded) >= remindEvery {
			o.reminded = time.Now()
			zlog.Warn("nacos is still unreachable", zlog.Dur("down_for", time.Since(o.since).Round(time.Second)), zlog.Int("sdk_records_hidden", o.suppressed))
		}
		return
	}
	delete(o.unhealthy, c)
	if o.down && len(o.unhealthy) == 0 && time.Since(o.since) > grace {
		o.down = false
		zlog.Info("nacos connection restored; the SDK registers and subscribes again by itself",
			zlog.Dur("down_for", time.Since(o.since).Round(100*time.Millisecond)), zlog.Int("sdk_records_hidden", o.suppressed))
	}
}

// forget takes a client that is closed out of the picture.
func (o *outageState) forget(c *Client) {
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.unhealthy, c)
}

func clipTo(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
