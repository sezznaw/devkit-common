package main

import "github.com/sezznaw/devkit-common/zlog"

func savePlayer(uid int64) {
	l := zlog.With(zlog.Str("component", "repo"))
	l.Info("child logger carries its fields", zlog.Int("uid", uid))
}
