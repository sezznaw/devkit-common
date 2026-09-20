package zlog

import (
	"fmt"
	"io"
	"slices"
)

// Red, Green, Yellow, Blue, Purple, Cyan and Gray give one argument of Infof
// and friends a color on the console:
//
//	zlog.Infof("player %d login from %s", zlog.Blue(uid), zlog.Red(ip))
//
// The verb, with its flags, width and precision, applies to the value as if it
// were not wrapped. Like the colors of fields, these exist on a colored console
// only: the message of a JSON record is the same with or without them.
func Red(v any) fmt.Formatter    { return colorArg(v, red) }
func Green(v any) fmt.Formatter  { return colorArg(v, green) }
func Yellow(v any) fmt.Formatter { return colorArg(v, yellow) }
func Blue(v any) fmt.Formatter   { return colorArg(v, blue) }
func Purple(v any) fmt.Formatter { return colorArg(v, purple) }
func Cyan(v any) fmt.Formatter   { return colorArg(v, cyan) }
func Gray(v any) fmt.Formatter   { return colorArg(v, gray) }

type coloredArg struct {
	v     any
	color fieldColor
}

// colorArg keeps a single layer, the outer color winning, so that taking the
// color off again is one step.
func colorArg(v any, c fieldColor) coloredArg {
	if inner, ok := v.(coloredArg); ok {
		v = inner.v
	}
	return coloredArg{v: v, color: c}
}

func (c coloredArg) Format(f fmt.State, verb rune) {
	io.WriteString(f, fieldColorCodes[c.color])
	fmt.Fprintf(f, fmt.FormatString(f, verb), c.v)
	io.WriteString(f, ansiReset)
}

// plainArgs takes the colors off. The caller's slice is left as it is.
func plainArgs(args []any) []any {
	out := args
	for i, a := range args {
		if ca, ok := a.(coloredArg); ok {
			if &out[0] == &args[0] {
				out = slices.Clone(args)
			}
			out[i] = ca.v
		}
	}
	return out
}
