package nacosx

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"

	"gopkg.in/yaml.v3"

	"github.com/sezznaw/devkit-common/zlog"
)

// Value is a configuration that follows Nacos while the program runs.
//
//	type Dynamic struct {
//		Battle struct {
//			MaxLevel int `yaml:"max_level"`
//		} `yaml:"battle"`
//	}
//
//	dyn, err := nacosx.Watch[Dynamic](nc, "ser-user.yaml")
//	...
//	if level > dyn.Get().Battle.MaxLevel {
//
// Put into it only what may change while the program runs. A listen address or
// the address of a database belongs into the file the service starts from:
// here it would look as if changing it did something.
type Value[T any] struct {
	cur atomic.Pointer[T]

	mu    sync.Mutex // one change at a time, hooks included
	hooks []func(old, cur *T)
	doc   map[string]string // the content in effect, flattened, for the next diff
	sum   string            // of the content in effect
	w     *watch
}

// Validator is implemented by a configuration that can tell whether it makes
// sense. Watch asks before it puts a new version into effect; with a pointer
// receiver, Validate is also the place to fill in defaults.
type Validator interface {
	Validate() error
}

// Watch reads a configuration into a T and keeps it current. The data id says
// how it is parsed: ".json" as JSON, everything else as YAML.
//
// It fails when the configuration cannot be read, does not exist, does not
// parse or is not valid: a service should not start on half a configuration.
// After that a change that does not parse or is not valid is rejected, with an
// ERROR record, and the version before it stays in effect; so does the last
// version when the configuration is deleted. What did change is in the record
// of the change, key by key, with the values of passwords and the like hidden.
func Watch[T any](c *Client, dataID string, opts ...Option) (*Value[T], error) {
	v := &Value[T]{}
	v.mu.Lock() // a change that arrives before the first version is in waits
	defer v.mu.Unlock()
	sub := &subscriber{fn: v.changed, quiet: true}
	w, content, err := c.follow(dataID, c.options(opts).group, sub)
	if err != nil {
		return nil, err
	}
	v.w = w
	if err := v.load(content); err != nil {
		w.unfollow(sub)
		return nil, err
	}
	return v, nil
}

// load puts the first version into effect.
func (v *Value[T]) load(content string) error {
	w, c, dataID := v.w, v.w.c, v.w.dataID
	if isBlank(content) {
		if c.IsOffline() {
			return fmt.Errorf("nacosx: %s does not exist or is empty. Without Nacos, configurations are read from files of that directory, named like the data id", w.file())
		}
		return fmt.Errorf("nacosx: %s does not exist or is empty. Create it in the Nacos console", w.where())
	}
	cur, unknown, err := decode[T](dataID, content)
	if err != nil {
		return fmt.Errorf("nacosx: %s: %w", w.where(), err)
	}
	v.cur.Store(cur)
	v.doc, v.sum = flatten(dataID, content), sum(content)
	fields := append(w.fields(), zlog.Str("md5", short(v.sum)), zlog.Any("values", settings(v.doc)))
	zlog.Info("config loaded", fields...)
	v.warnUnknown(unknown)
	return nil
}

// Static returns a Value that holds v and never changes, for tests of code
// that takes a *Value.
func Static[T any](v *T) *Value[T] {
	s := &Value[T]{}
	s.cur.Store(v)
	return s
}

// Get returns the version in effect. It never returns nil, costs one atomic
// load, and what it returns is never written to again: a change is a new T.
// Therefore call Get where the value is needed rather than keeping the result,
// and within one piece of work call it once, so that the work sees one version
// from start to end.
func (v *Value[T]) Get() *T { return v.cur.Load() }

// OnChange registers fn to be called after a new version went into effect, for
// what has to be rebuilt rather than read: a rate limiter, a pool, a cache.
// The calls come one at a time, in the order of the changes; a panic in fn is
// logged. Most code needs no hook, only Get.
func (v *Value[T]) OnChange(fn func(old, cur *T)) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.hooks = append(v.hooks, fn)
}

func (v *Value[T]) changed(content string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	w := v.w
	if isBlank(content) {
		zlog.Warn("config deleted or emptied; the previous one stays in effect", w.fields()...)
		return
	}
	fields := append(w.fields(), zlog.Str("md5", short(v.sum)+"→"+short(sum(content))))
	cur, unknown, err := decode[T](w.dataID, content)
	if err != nil {
		zlog.Error("config change rejected; the previous one stays in effect", append(fields, zlog.Err(err))...)
		return
	}
	doc := flatten(w.dataID, content)
	changes := diff(v.doc, doc)
	old := v.cur.Swap(cur)
	v.doc, v.sum = doc, sum(content)
	if len(changes) == 0 {
		// Comments, order, white space.
		zlog.Info("config changed, all values as before", fields...)
	} else {
		zlog.Info("config changed", append(fields, zlog.Any("changes", changes))...)
	}
	v.warnUnknown(unknown)
	for _, fn := range v.hooks {
		v.hook(fn, old, cur)
	}
}

// unknownField is how yaml.v3 and encoding/json name a key without a field.
var unknownField = regexp.MustCompile(`(?:line (\d+): )?(?:field (\S+) not found|unknown field "([^"]+)")`)

// warnUnknown names the keys that T has no field for. The decoders say it in
// terms of Go types, which whoever edits a configuration in the Nacos console
// neither knows nor needs.
func (v *Value[T]) warnUnknown(unknown error) {
	if unknown == nil {
		return
	}
	var keys []string
	for _, m := range unknownField.FindAllStringSubmatch(unknown.Error(), -1) {
		key := m[2] + m[3]
		if m[1] != "" {
			key += " (line " + m[1] + ")"
		}
		keys = append(keys, key)
	}
	fields := v.w.fields()
	if len(keys) == 0 {
		fields = append(fields, zlog.Err(unknown))
	} else {
		fields = append(fields, zlog.Str("keys", strings.Join(keys, ", ")).Yellow())
	}
	zlog.Warn("config has keys the program does not know; a typo?", fields...)
}

func (v *Value[T]) hook(fn func(old, cur *T), old, cur *T) {
	defer func() {
		if r := recover(); r != nil {
			zlog.Error("config change hook panicked", append(v.w.fields(),
				zlog.Any("panic", r), zlog.Str("panic_stack", string(debug.Stack())))...)
		}
	}()
	fn(old, cur)
}

func isJSON(dataID string) bool { return strings.EqualFold(filepath.Ext(dataID), ".json") }

// decode parses content into a new T and validates it. Keys that T has no
// field for do not make it fail, the rest of the configuration is as good as
// without them, but they are reported (unknown): a key with a typo is
// otherwise a setting that silently does nothing.
func decode[T any](dataID, content string) (cur *T, unknown, err error) {
	parse := func(strict bool) (*T, error) {
		out := new(T)
		var err error
		if isJSON(dataID) {
			dec := json.NewDecoder(strings.NewReader(content))
			if strict {
				dec.DisallowUnknownFields()
			}
			err = dec.Decode(out)
		} else {
			dec := yaml.NewDecoder(strings.NewReader(content))
			dec.KnownFields(strict)
			err = dec.Decode(out)
		}
		return out, err
	}
	cur, err = parse(true)
	if err != nil {
		strictErr := err
		if cur, err = parse(false); err != nil {
			return nil, nil, err
		}
		unknown = strictErr
	}
	if val, ok := any(cur).(Validator); ok {
		if err := val.Validate(); err != nil {
			return nil, nil, fmt.Errorf("not valid: %w", err)
		}
	}
	return cur, unknown, nil
}

// flatten turns a document into "a.b.c" -> value, which is what two versions
// are compared by. Lists are values: what changed inside one is seldom easier
// to read than the two lists. A document that does not parse is empty here;
// it is reported where it is decoded.
func flatten(dataID, content string) map[string]string {
	var doc any
	if isJSON(dataID) {
		dec := json.NewDecoder(strings.NewReader(content))
		dec.UseNumber()
		_ = dec.Decode(&doc)
	} else {
		_ = yaml.Unmarshal([]byte(content), &doc)
	}
	out := map[string]string{}
	var walk func(prefix string, v any)
	walk = func(prefix string, v any) {
		switch m := v.(type) {
		case map[string]any:
			for k, e := range m {
				walk(join(prefix, k), e)
			}
		case map[any]any:
			for k, e := range m {
				walk(join(prefix, fmt.Sprint(k)), e)
			}
		default:
			if prefix == "" {
				prefix = "(value)" // the document is a scalar or a list
			}
			out[prefix] = leaf(v)
		}
	}
	walk("", doc)
	return out
}

func join(prefix, k string) string {
	if prefix == "" {
		return k
	}
	return prefix + "." + k
}

func leaf(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case string:
		return x
	case []any:
		var b bytes.Buffer
		enc := json.NewEncoder(&b)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(x); err == nil {
			return strings.TrimSpace(b.String())
		}
	}
	return fmt.Sprint(v)
}
