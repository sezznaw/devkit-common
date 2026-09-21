package nacosx

import (
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/nacos-group/nacos-sdk-go/v2/vo"

	"github.com/sezznaw/devkit-common/zlog"
)

// Option adjusts one call of Get, OnChange or Watch.
type Option func(*options)

type options struct {
	group string
}

// Group names the group of a configuration that is not in the group of the
// client (Config.Group).
func Group(name string) Option { return func(o *options) { o.group = name } }

func (c *Client) options(opts []Option) options {
	o := options{group: c.cfg.GroupName()}
	for _, fn := range opts {
		fn(&o)
	}
	return o
}

// watch follows one configuration for all who asked for it.
//
// The SDK runs a listener as `go listener(...)`: two changes in quick
// succession are two goroutines with nothing to keep them in order, a panic in
// one of them ends the process, and the listener is also called right after
// registering when the snapshot on disk differs from the server. So there is
// one SDK listener per configuration, ours, and it serializes (mu), drops what
// is not a change (sum), asks the server for the content instead of trusting
// the order of the pushes, and shields the process from the subscribers.
type watch struct {
	c      *Client
	dataID string
	group  string

	mu        sync.Mutex
	listening bool
	sum       string // of the content the subscribers saw last
	content   string
	subs      []*subscriber
}

type subscriber struct {
	fn func(content string)
	// quiet subscribers write the record of a change themselves, because they
	// know more about it: what changed, and whether it was accepted.
	quiet bool
}

// where names the configuration in records and errors.
func (w *watch) where() string {
	if w.c.IsOffline() {
		return w.file()
	}
	return fmt.Sprintf("data id %q (group %s, namespace %s)", w.dataID, w.group, w.c.cfg.namespaceName())
}

func (w *watch) file() string { return filepath.Join(w.c.offlineDir, w.dataID) }

func (w *watch) fields() []zlog.Field {
	if w.c.IsOffline() {
		return []zlog.Field{zlog.Str("file", w.file())}
	}
	return []zlog.Field{zlog.Str("data_id", w.dataID), zlog.Str("group", w.group)}
}

// Get returns the content of a configuration as it is now. A configuration
// that does not exist is "", not an error: whether that is a problem is for
// the caller to say.
func (c *Client) Get(dataID string, opts ...Option) (string, error) {
	w := &watch{c: c, dataID: dataID, group: c.options(opts).group}
	return w.fetch()
}

func (w *watch) fetch() (string, error) {
	if w.dataID == "" {
		return "", errors.New("nacosx: the data id is empty")
	}
	if w.c.IsOffline() {
		b, err := os.ReadFile(w.file())
		if err != nil && !os.IsNotExist(err) {
			return "", fmt.Errorf("nacosx: %w", err)
		}
		return string(b), nil
	}
	cc, err := w.c.ConfigClient()
	if err != nil {
		return "", err
	}
	content, err := cc.GetConfig(vo.ConfigParam{DataId: w.dataID, Group: w.group})
	if err != nil {
		return "", fmt.Errorf("nacosx: read %s: %w", w.where(), err)
	}
	return content, nil
}

// OnChange calls fn with the new content whenever the configuration changes,
// until the client is closed. It is not called for the content of now: read
// that with Get. The calls for one configuration come one after the other, in
// the order of the changes, and a panic in fn is logged instead of ending the
// process. A configuration that is deleted arrives as "".
//
// For configuration with a structure, Watch does the parsing, the validation
// and the record of what changed.
func (c *Client) OnChange(dataID string, fn func(content string), opts ...Option) error {
	_, _, err := c.follow(dataID, c.options(opts).group, &subscriber{fn: fn})
	return err
}

// follow adds a subscriber and returns the watch together with the content of
// the moment the subscription starts: a change after that content is
// delivered, none is lost in between and none is delivered twice.
func (c *Client) follow(dataID, group string, s *subscriber) (*watch, string, error) {
	key := group + "\x00" + dataID
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, "", errors.New("nacosx: the client is closed")
	}
	w, ok := c.watches[key]
	if !ok {
		w = &watch{c: c, dataID: dataID, group: group}
		c.watches[key] = w
	}
	c.mu.Unlock()

	w.mu.Lock()
	defer w.mu.Unlock()
	content, err := w.fetch()
	if err != nil {
		return nil, "", err
	}
	if w.listening {
		// Those who follow already hear of it first, should this look have
		// found something new before the push did.
		w.apply(content)
		w.subs = append(w.subs, s)
		return w, content, nil
	}
	w.content, w.sum = content, sum(content)
	if err := w.listen(); err != nil {
		return nil, "", err
	}
	w.listening = true
	w.subs = append(w.subs, s)
	return w, content, nil
}

// unfollow takes a subscriber out again. The SDK listener stays: the next
// subscriber finds it in place.
func (w *watch) unfollow(s *subscriber) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for i, sub := range w.subs {
		if sub == s {
			w.subs = append(w.subs[:i:i], w.subs[i+1:]...)
			return
		}
	}
}

func (w *watch) listen() error {
	if w.c.IsOffline() {
		go w.pollFile(filePollInterval)
		return nil
	}
	cc, err := w.c.ConfigClient()
	if err != nil {
		return err
	}
	err = cc.ListenConfig(vo.ConfigParam{
		DataId: w.dataID,
		Group:  w.group,
		OnChange: func(_, _, _, pushed string) {
			w.changed(pushed)
		},
	})
	if err != nil {
		return fmt.Errorf("nacosx: listen to %s: %w", w.where(), err)
	}
	return nil
}

// changed is the one listener the SDK knows for this configuration.
func (w *watch) changed(pushed string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	content := pushed
	if !w.c.IsOffline() {
		// Of two pushes that overtook each other the older one would win.
		// What the server says now cannot be out of date.
		if now, err := w.fetch(); err == nil {
			content = now
		}
	}
	w.apply(content)
}

// apply tells the subscribers when content is not what they saw last. The
// caller holds mu.
func (w *watch) apply(content string) {
	s := sum(content)
	if s == w.sum {
		return
	}
	prev := w.sum
	w.content, w.sum = content, s
	loud := false
	for _, sub := range w.subs {
		loud = loud || !sub.quiet
	}
	if loud {
		fields := append(w.fields(), zlog.Str("md5", short(prev)+"→"+short(s)), zlog.Int("bytes", len(content)))
		if content == "" {
			zlog.Warn("config deleted", fields...)
		} else {
			zlog.Info("config changed", fields...)
		}
	}
	for _, sub := range w.subs {
		w.deliver(sub, content)
	}
}

func (w *watch) deliver(sub *subscriber, content string) {
	defer func() {
		if r := recover(); r != nil {
			zlog.Error("config change handler panicked", append(w.fields(),
				zlog.Any("panic", r), zlog.Str("panic_stack", string(debug.Stack())))...)
		}
	}()
	sub.fn(content)
}

// filePollInterval is how often the file of an offline configuration is looked
// at. It is a development aid; a second is soon enough after saving a file.
var filePollInterval = time.Second

func (w *watch) pollFile(every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	var mod time.Time
	var size int64 = -1
	if fi, err := os.Stat(w.file()); err == nil {
		mod, size = fi.ModTime(), fi.Size()
	}
	for {
		select {
		case <-w.c.stop:
			return
		case <-t.C:
		}
		fi, err := os.Stat(w.file())
		switch {
		case err != nil && size == -1:
			continue // still not there
		case err != nil:
			mod, size = time.Time{}, -1
		case fi.ModTime().Equal(mod) && fi.Size() == size:
			continue
		default:
			mod, size = fi.ModTime(), fi.Size()
		}
		if content, err := w.fetch(); err == nil {
			w.changed(content)
		}
	}
}

func sum(content string) string {
	if content == "" {
		return ""
	}
	h := md5.Sum([]byte(content))
	return hex.EncodeToString(h[:])
}

// short is enough of a checksum to tell two versions apart in a record.
func short(sum string) string {
	if sum == "" {
		return "(none)"
	}
	return sum[:8]
}

func isBlank(content string) bool { return strings.TrimSpace(content) == "" }
