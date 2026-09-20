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

// clickablePath returns file in the form that GoLand's run window, VS Code
// and terminals turn into a link, and that is never ambiguous: relative to the
// working directory when the file is inside it (the convention of compiler
// errors, short for the service's own code), otherwise absolute (the module
// cache, a sibling checkout). A name such as "handler/handler.go" alone would
// be a guess as soon as two services of one project have that file.
//
// A file that is not absolute comes from a -trimpath build and is kept as is.
func clickablePath(cwd, file string) string {
	if cwd == "" || !filepath.IsAbs(file) {
		return file
	}
	rel, err := filepath.Rel(cwd, file)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return file
	}
	return filepath.ToSlash(rel)
}

func jsonEncoderConfig() zapcore.EncoderConfig {
	return zapcore.EncoderConfig{
		TimeKey:     keyTime,
		LevelKey:    keyLevel,
		CallerKey:   keyCaller,
		MessageKey:  keyMsg,
		LineEnding:  zapcore.DefaultLineEnding,
		EncodeTime:  zapcore.TimeEncoderOfLayout(jsonTimeLayout),
		EncodeLevel: zapcore.CapitalLevelEncoder,
		// "handler/handler.go:27": with the service field that is enough to
		// find the line, and unlike the full path it does not depend on the
		// machine or the flags the binary was built with.
		EncodeCaller:   zapcore.ShortCallerEncoder,
		EncodeDuration: zapcore.StringDurationEncoder,
	}
}
