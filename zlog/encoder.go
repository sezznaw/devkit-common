package zlog

import (
	"os"
	"path/filepath"
	"strings"

	"go.uber.org/zap/zapcore"
)

// Time layouts. People read the console: local time, a space instead of "T",
// no zone. Collectors parse the JSON: RFC 3339 with the zone offset, because a
// time without one gets guessed as UTC or as the collector's zone. Both carry
// the date and milliseconds.
const (
	consoleTimeLayout = "2006-01-02 15:04:05.000"
	jsonTimeLayout    = "2006-01-02T15:04:05.000Z07:00"
)

// ANSI escape sequences. One color per level, so a level is recognisable
// without reading the word; the timestamp is dimmed to keep the eye on the
// level and the message.
const (
	ansiReset = "\x1b[0m"
	ansiDim   = "\x1b[90m"

	colorDebug = "\x1b[36m"      // cyan
	colorInfo  = "\x1b[32m"      // green
	colorWarn  = "\x1b[33m"      // yellow
	colorError = "\x1b[1;31m"    // bold red
	colorFatal = "\x1b[1;97;41m" // bold white on red
)

// colorEnabled reports whether console output is colored. It deliberately
// does not test for a terminal: the run window of GoLand and other IDEs is
// not a TTY but renders ANSI colors. Set NO_COLOR (https://no-color.org) to
// switch colors off, for example when redirecting console output to a file.
func colorEnabled() bool {
	return os.Getenv("NO_COLOR") == ""
}

func levelColor(l zapcore.Level) string {
	switch {
	case l <= zapcore.DebugLevel:
		return colorDebug
	case l == zapcore.InfoLevel:
		return colorInfo
	case l == zapcore.WarnLevel:
		return colorWarn
	case l == zapcore.ErrorLevel:
		return colorError
	default: // DPanic, Panic, Fatal
		return colorFatal
	}
}

// levelText pads to the width of "DEBUG" / "ERROR" / "FATAL" so that messages
// line up. The padding stays outside the colored part.
func levelText(l zapcore.Level, color bool) string {
	name := l.CapitalString()
	pad := ""
	for i := len(name); i < 5; i++ {
		pad += " "
	}
	if !color {
		return name + pad
	}
	return levelColor(l) + name + ansiReset + pad
}

// callerPath is where a record comes from, as the console prints it: short
// enough to leave the line to the message, and still saying which file it is.
//
//   - Inside the working directory: relative to it, the convention of compiler
//     errors. "handler/handler.go" is the service's own code.
//   - A sibling of the working directory: from the directory both are in, which
//     for a service is the project. "common/nacosx/services.go" is the common
//     library checked out next to the service, "ser-user/handler/handler.go"
//     another service. Relative to the working directory that would start with
//     "../", which reads worse and is no shorter where it matters.
//   - The module cache: from the module on, "github.com/cloudwego/kitex@v0.16.3/
//     pkg/remote/...". Everything before it is the same on every line and says
//     nothing about the record.
//   - Anything else stays absolute, and so does every path when the working
//     directory is unknown.
//
// GoLand's run window, VS Code and terminals link the first form; GoLand also
// finds the others when the project directory is what is open, because it looks
// a relative path up by its end. A file that is not absolute comes from a
// -trimpath build and is kept as is.
func callerPath(cwd, file string) string {
	if !filepath.IsAbs(file) {
		return file
	}
	slashed := filepath.ToSlash(file)
	if i := strings.LastIndex(slashed, "/pkg/mod/"); i >= 0 {
		return slashed[i+len("/pkg/mod/"):]
	}
	if cwd == "" {
		return file
	}
	rel, err := filepath.Rel(cwd, file)
	if err != nil {
		return file
	}
	rel = filepath.ToSlash(rel)
	switch {
	case rel == "..":
		return file
	case !strings.HasPrefix(rel, "../"):
		return rel
	}
	// One step up and down again is a sibling; further away is somewhere else.
	if sibling := strings.TrimPrefix(rel, "../"); !strings.HasPrefix(sibling, "../") && strings.Contains(sibling, "/") {
		return sibling
	}
	return file
}

func jsonEncoderConfig() zapcore.EncoderConfig {
	return zapcore.EncoderConfig{
		TimeKey:    keyTime,
		LevelKey:   keyLevel,
		CallerKey:  keyCaller,
		MessageKey: keyMsg,
		// keyStack is also what the klog bridge of kitexx uses for a panic.
		StacktraceKey: keyStack,
		LineEnding:    zapcore.DefaultLineEnding,
		EncodeTime:    zapcore.TimeEncoderOfLayout(jsonTimeLayout),
		EncodeLevel:   zapcore.CapitalLevelEncoder,
		// "handler/handler.go:27": with the service field that is enough to
		// find the line, and unlike the full path it does not depend on the
		// machine or the flags the binary was built with.
		EncodeCaller:   zapcore.ShortCallerEncoder,
		EncodeDuration: zapcore.StringDurationEncoder,
	}
}
