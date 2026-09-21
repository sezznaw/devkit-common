package nacosx

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/sezznaw/devkit-common/zlog"
)

// Change is one key of a configuration that is not what it was.
type Change struct {
	// Op is "added", "removed" or "changed".
	Op   string `json:"op"`
	Key  string `json:"key"`
	From string `json:"from,omitempty"`
	To   string `json:"to,omitempty"`
}

// Changes is what a record of a changed configuration carries: an array in
// JSON, and on the console a line per key below the record:
//
//	~ battle.max_level   60 → 80
//	+ battle.double_exp  true
//	- legacy.flag        false
type Changes []Change

const masked = "***"

// diff compares two flattened documents. The values of keys that look like
// secrets are hidden; that such a key changed is still said.
func diff(old, cur map[string]string) Changes {
	var out Changes
	for k, to := range cur {
		from, was := old[k]
		switch {
		case !was:
			out = append(out, Change{Op: "added", Key: k, To: mask(k, to)})
		case from != to:
			out = append(out, Change{Op: "changed", Key: k, From: mask(k, from), To: mask(k, to)})
		}
	}
	for k, from := range old {
		if _, still := cur[k]; !still {
			out = append(out, Change{Op: "removed", Key: k, From: mask(k, from)})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

func (c Changes) LogBlock(colored bool) string {
	width := 0
	for _, ch := range c {
		width = max(width, len(ch.Key))
	}
	var b strings.Builder
	for _, ch := range c {
		sign, value := "~", ch.From+" → "+ch.To
		paint := zlog.Yellow
		switch ch.Op {
		case "added":
			sign, value, paint = "+", ch.To, zlog.Green
		case "removed":
			sign, value, paint = "-", ch.From, zlog.Red
		}
		line := fmt.Sprintf("%s %-*s  %s", sign, width, ch.Key, value)
		if colored {
			line = fmt.Sprint(paint(line))
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// settings is a flattened configuration as a record carries it: an object in
// JSON, and on the console a line per key below the record, sorted.
type settings map[string]string

// maxSettingLines keeps the record of a large configuration readable on the
// console. JSON always has all of it.
const maxSettingLines = 60

func (s settings) sorted() []string {
	keys := make([]string, 0, len(s))
	for k := range s {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func (s settings) LogBlock(bool) string {
	keys := s.sorted()
	width := 0
	for _, k := range keys {
		width = max(width, len(k))
	}
	var b strings.Builder
	for i, k := range keys {
		if i == maxSettingLines {
			fmt.Fprintf(&b, "... and %d more\n", len(keys)-i)
			break
		}
		fmt.Fprintf(&b, "%-*s  %s\n", width, k, mask(k, s[k]))
	}
	return b.String()
}

func (s settings) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, k := range s.sorted() {
		if i > 0 {
			b.WriteByte(',')
		}
		key, _ := json.Marshal(k)
		val, _ := json.Marshal(mask(k, s[k]))
		b.Write(key)
		b.WriteByte(':')
		b.Write(val)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// secretWords mark a key whose value stays out of the log. Better one value
// too many hidden than a password in a log collector.
var secretWords = []string{"password", "passwd", "pwd", "secret", "token", "credential", "private", "apikey", "accesskey", "signature"}

func isSecret(name string) bool {
	n := strings.ToLower(strings.NewReplacer("_", "", "-", "").Replace(name))
	for _, w := range secretWords {
		if strings.Contains(n, w) {
			return true
		}
	}
	// "key", "app_key", "aes-key", but not "keyword" or "hotkeys".
	last := name[strings.LastIndexAny(name, ".")+1:]
	l := strings.ToLower(last)
	return l == "key" || strings.HasSuffix(l, "_key") || strings.HasSuffix(l, "-key") || strings.HasSuffix(last, "Key")
}

func mask(key, value string) string {
	if isSecret(key) {
		return masked
	}
	// A list is one value here, and the secrets of its items are inside it.
	if strings.HasPrefix(value, "[") {
		for _, part := range strings.FieldsFunc(value, func(r rune) bool { return r == '"' }) {
			if len(part) < 40 && isSecret(part) {
				return masked
			}
		}
	}
	return value
}
