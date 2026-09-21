package zlog

import (
	"sort"
	"sync"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// dropReporter says what sampling left out. Without it the records are gone
// without a trace: the log of a flood looks like the log of a few errors, and
// nobody can tell how large it was.
//
// For every second in which records were dropped it writes, per level and
// message, one record of that level:
//
//	ERROR zlog: records dropped by sampling dropped_msg="redis down" dropped=4213
//
// There is no goroutine while nothing is dropped: the first drop of a window
// arms a timer, which reports and is gone. Sync and stop report at once, so
// that the count of the last second before an exit is not lost.
type dropReporter struct {
	out zapcore.Core // behind the sampler: a report is never sampled itself

	mu      sync.Mutex
	pending map[dropKey]int
	other   int // drops beyond maxDropKeys distinct messages in one window
	timer   *time.Timer
}

type dropKey struct {
	level zapcore.Level
	msg   string
}

const (
	dropWindow = time.Second
	// A flood is a few messages many times over. The bound is for the case
	// that it is not, e.g. formatted messages that all differ.
	maxDropKeys = 64

	dropReportMsg = "zlog: records dropped by sampling"
)

// hook is called by zap's sampler for every record it decides on, under load
// by definition; it does nothing for a record that is written.
func (d *dropReporter) hook(ent zapcore.Entry, dec zapcore.SamplingDecision) {
	if dec&zapcore.LogDropped == 0 {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.pending == nil {
		d.pending = make(map[dropKey]int)
	}
	k := dropKey{ent.Level, ent.Message}
	if _, known := d.pending[k]; known || len(d.pending) < maxDropKeys {
		d.pending[k]++
	} else {
		d.other++
	}
	if d.timer == nil {
		d.timer = time.AfterFunc(dropWindow, d.flush)
	}
}

// flush reports what has been dropped since the last report.
func (d *dropReporter) flush() {
	d.mu.Lock()
	pending, other := d.pending, d.other
	d.pending, d.other = nil, 0
	if d.timer != nil {
		d.timer.Stop()
		d.timer = nil
	}
	d.mu.Unlock()

	keys := make([]dropKey, 0, len(pending))
	for k := range pending {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { // the worst first, and a stable order
		if pending[keys[i]] != pending[keys[j]] {
			return pending[keys[i]] > pending[keys[j]]
		}
		return keys[i].msg < keys[j].msg
	})
	now := time.Now()
	for _, k := range keys {
		_ = d.out.Write(zapcore.Entry{Level: k.level, Time: now, Message: dropReportMsg},
			[]zap.Field{zap.String("dropped_msg", k.msg), zap.Int("dropped", pending[k])})
	}
	if other > 0 {
		_ = d.out.Write(zapcore.Entry{Level: zapcore.WarnLevel, Time: now, Message: dropReportMsg},
			[]zap.Field{zap.String("dropped_msg", "(other messages)"), zap.Int("dropped", other)})
	}
}
