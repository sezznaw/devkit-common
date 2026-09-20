package zlog

import (
	"math"
	"time"
	"unsafe"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// Field is one key with its value, made by Str, Int, Float, Bool, Dur, Time,
// Err or Any. A color method makes it stand out on the console:
//
//	zlog.Info("player login", zlog.Int("uid", uid), zlog.Int("age", 15).Blue())
//
// The color exists on a colored console only: JSON records and consoles with
// NO_COLOR set are the same with or without it.
type Field struct {
	z     zap.Field
	color fieldColor
}

type fieldColor uint8

const (
	noColor fieldColor = iota
	red
	green
	yellow
	blue
	purple
	cyan
	gray
)

var fieldColorCodes = [...]string{
	red:    "\x1b[31m",
	green:  "\x1b[32m",
	yellow: "\x1b[33m",
	blue:   "\x1b[34m",
	// Not one of the 8 basic colors: their "magenta" is a pink in most themes.
	// This is color 135 (#af5fff) of the 256-color palette.
	purple: "\x1b[38;5;135m",
	cyan:   "\x1b[36m",
	gray:   ansiDim,
}

func (f Field) Red() Field    { f.color = red; return f }
func (f Field) Green() Field  { f.color = green; return f }
func (f Field) Yellow() Field { f.color = yellow; return f }
func (f Field) Blue() Field   { f.color = blue; return f }
func (f Field) Purple() Field { f.color = purple; return f }
func (f Field) Cyan() Field   { f.color = cyan; return f }
func (f Field) Gray() Field   { f.color = gray; return f }

// coloredField tells the console encoder the color and then adds the field
// as usual. zap hands encoders the fields one by one with no room for
// anything but key and value, hence the detour through an inlined object.
type coloredField Field

func (c coloredField) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	ce, ok := enc.(*consoleEncoder)
	if !ok || !ce.color {
		c.z.AddTo(enc)
		return nil
	}
	ce.fieldColor = fieldColorCodes[c.color]
	c.z.AddTo(enc)
	ce.fieldColor = ""
	ce.buf.AppendString(ansiReset)
	return nil
}

// The keys that every record has. keyService is there in JSON only.
const (
	keyTime    = "time"
	keyLevel   = "level"
	keyCaller  = "caller"
	keyMsg     = "msg"
	keyService = "service"

	clashPrefix = "fields."
)

// fieldKey returns the key a field is written under. "level" is the obvious
// name for the level of a player, and zap writes whatever it is given: the
// JSON record would have "level" twice. Parsers then keep the last one, so
// that the record is no longer found by its level, or, like Elasticsearch,
// refuse the record. Such a field becomes "fields.level", which is what logrus
// does. The console shows the same name, so that it is known before anybody
// searches for it in a collector.
func fieldKey(k string) string {
	switch k {
	case keyTime, keyLevel, keyCaller, keyMsg, keyService:
		return clashPrefix + k
	}
	return k
}

// zap converts fields for a core. Only a colored console pays for colors.
func (c *core) zap(fields []Field) []zap.Field {
	if len(fields) == 0 {
		return nil
	}
	out := make([]zap.Field, len(fields))
	for i, f := range fields {
		f.z.Key = fieldKey(f.z.Key)
		if f.color != noColor && c.colored {
			out[i] = zap.Inline(coloredField(f))
		} else {
			out[i] = f.z
		}
	}
	return out
}

type integer interface {
	~int | ~int8 | ~int16 | ~int32 | ~int64 | ~uint | ~uint8 | ~uint16 | ~uint32 | ~uint64 | ~uintptr
}

type float interface{ ~float32 | ~float64 }

// Str is a string field. It also takes types whose underlying type is string,
// such as type State string.
func Str[T ~string](key string, v T) Field { return Field{z: zap.String(key, string(v))} }

// Int is an integer field for every integer type, so the caller does not have
// to know whether an id is an int, an int32 or an int64.
func Int[T integer](key string, v T) Field {
	if v > 0 && uint64(v) > math.MaxInt64 {
		return Field{z: zap.Uint64(key, uint64(v))}
	}
	return Field{z: zap.Int64(key, int64(v))}
}

// Float is a floating-point field for float32 and float64. A float32 is kept
// as one: converted to float64, 0.1 would print as 0.10000000149011612.
func Float[T float](key string, v T) Field {
	if unsafe.Sizeof(v) == 4 {
		return Field{z: zap.Float32(key, float32(v))}
	}
	return Field{z: zap.Float64(key, float64(v))}
}

func Bool(key string, v bool) Field { return Field{z: zap.Bool(key, v)} }

// Dur is a duration field, printed as "1.5s".
func Dur(key string, v time.Duration) Field { return Field{z: zap.Duration(key, v)} }

func Time(key string, v time.Time) Field { return Field{z: zap.Time(key, v)} }

// Err is the error of a record, always under the key "err" so that one query
// finds the errors of every service. A nil error adds no field.
func Err(err error) Field { return Field{z: zap.NamedError("err", err)} }

// Any is a field for everything else: structs, slices, maps, or a value whose
// type the caller does not want to look up. Values zap has no typed field for
// are rendered as JSON. A value wrapped in a color, zlog.Blue(v), gives the
// field that color.
func Any(key string, v any) Field {
	if ca, ok := v.(coloredArg); ok {
		return Field{z: zap.Any(key, ca.v), color: ca.color}
	}
	return Field{z: zap.Any(key, v)}
}
