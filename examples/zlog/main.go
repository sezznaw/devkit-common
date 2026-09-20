// Command zlog prints one record per level, to check by eye how the console
// format looks where a service really runs, and that a click on file:line
// opens that line:
//
//	go run ./examples/zlog
//	go run ./examples/zlog -format json   # the same records as a collector gets them
//
// In GoLand use Run on this file. The test runner window is no substitute: it
// shows the color escapes as text instead of rendering them.
package main

import (
	"errors"
	"flag"
	"os"
	"time"

	"github.com/sezznaw/devkit-common/zlog"
)

func main() {
	format := flag.String("format", zlog.FormatConsole, "console or json")
	flag.Parse()
	zlog.Init(zlog.Options{Level: "debug", Format: *format, Service: "demo"})
	defer zlog.Sync()

	zlog.Debug("cache miss", zlog.Str("key", "player:1001"))
	zlog.Info("player login", zlog.Int("uid", 1001), zlog.Str("ip", "10.0.0.7"))
	zlog.Warn("slow query", zlog.Dur("took", 1200*time.Millisecond))
	zlog.Error("save player failed", zlog.Int("uid", 1001), zlog.Err(errors.New("redis: i/o timeout")))
	zlog.Infof("printf style: %d players online", 42)

	// A field can be given a color to make it stand out. Console only: run
	// with -format json and the records are the same as without.
	zlog.Info("player created", zlog.Int("uid", 1001), zlog.Int("age", 15).Blue(), zlog.Str("class", "mage").Purple())
	zlog.Infof("printf arguments take a color too: player %d from %s, win rate %.1f%%", zlog.Blue(1001), zlog.Purple("10.0.0.7"), zlog.Green(61.803))
	zlog.Info("palette",
		zlog.Str("a", "Red").Red(), zlog.Str("b", "Green").Green(), zlog.Str("c", "Yellow").Yellow(),
		zlog.Str("d", "Blue").Blue(), zlog.Str("e", "Purple").Purple(), zlog.Str("f", "Cyan").Cyan(),
		zlog.Str("g", "Gray").Gray(), zlog.Str("h", "no color"))

	savePlayer(1001) // logs from repo.go

	// Code outside the working directory (the module cache, a sibling
	// checkout) is reported with its absolute path. A logger built after
	// moving away shows that form for this very file.
	if err := os.Chdir(os.TempDir()); err == nil {
		far := zlog.New(zlog.Options{Level: "debug", Format: *format, Service: "demo"})
		far.Info("outside the working directory: absolute path")
	}

	// Last, because Fatal ends the process with exit status 1.
	zlog.Fatal("cannot start", zlog.Str("reason", "demo of the fatal color"))
}
