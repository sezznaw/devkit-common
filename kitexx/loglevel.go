package kitexx

import (
	"fmt"
	"strings"

	"github.com/sezznaw/devkit-common/nacosx"
	"github.com/sezznaw/devkit-common/zlog"
)

// levelSource is the part of *nacosx.Client that following the level needs.
type levelSource interface {
	Get(dataID string, opts ...nacosx.Option) (string, error)
	OnChange(dataID string, fn func(content string), opts ...nacosx.Option) error
}

// watchLogLevel applies the level stored in a Nacos configuration now and
// whenever it changes. An empty configuration leaves the level alone, so the
// data id can exist before anybody needs it, or not exist at all.
func watchLogLevel(src levelSource, dataID string) error {
	apply := func(content string) {
		level := strings.ToLower(strings.TrimSpace(content))
		if level == "" {
			return
		}
		zlog.SetLevel(level)
		zlog.Warn("log level set from Nacos", zlog.Str("data_id", dataID), zlog.Str("to", level))
	}
	content, err := src.Get(dataID)
	if err != nil {
		return fmt.Errorf("kitexx: read the log level: %w", err)
	}
	apply(content)
	if err := src.OnChange(dataID, apply); err != nil {
		return fmt.Errorf("kitexx: follow the log level: %w", err)
	}
	// Part of how the service logs, next to the "logger configured" of zlog.
	zlog.Info("log level follows a Nacos configuration", zlog.Str("data_id", dataID))
	return nil
}
