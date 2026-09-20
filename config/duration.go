package config

import (
	"fmt"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration is a time.Duration that can be written in YAML as "3s", "500ms",
// "1m30s", or as a bare number of seconds.
type Duration time.Duration

// Std returns the value as a time.Duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// Or returns the value, or def when it is zero (not set in the config).
func (d Duration) Or(def time.Duration) time.Duration {
	if d == 0 {
		return def
	}
	return time.Duration(d)
}

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	var secs float64
	if err := n.Decode(&secs); err == nil {
		*d = Duration(time.Duration(secs * float64(time.Second)))
		return nil
	}
	var s string
	if err := n.Decode(&s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("line %d: invalid duration %q (use e.g. 3s, 500ms, 1m)", n.Line, s)
	}
	*d = Duration(v)
	return nil
}
