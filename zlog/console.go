package zlog

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"

	"go.uber.org/zap/buffer"
	"go.uber.org/zap/zapcore"
)

var bufPool = buffer.NewPool()

// consoleEncoder writes one record per line for people to read:
//
//	2006-01-02 15:04:05.000 INFO  handler/handler.go:27 player login uid=1001 ip=10.0.0.7
//
// zap's own console encoder prints the fields as a JSON object at the end of
// the line. Here they are key=value in the order they were added, the keys
// dimmed, so that the values stand out.
//
// A value of several lines, which is what the stack of an error or of a panic
// is, would be one long quoted line. It is printed below the record instead,
// as it is, so that the file:line of every frame is a link:
//
//	2006-01-02 15:04:05.000 ERROR handler/handler.go:27 save failed err=boom
//	    errVerbose:
//	      main.save
//	      	/Users/me/game/ser-auth/handler/handler.go:27
type consoleEncoder struct {
	color bool
	cwd   string

	// buf holds rendered fields, each as " key=value": those added by With,
	// or, for the short-lived encoder EncodeEntry makes, the line being built.
	buf *buffer.Buffer
	// ns prefixes keys after OpenNamespace: "req." turns "id" into "req.id".
	ns string
	// fieldColor is set by coloredField around the one field it colors. That
	// field is then written in this color, key included, instead of a dimmed
	// key and a plain value.
	fieldColor string
	// tail holds the values of several lines, to be printed below the record.
	tail *buffer.Buffer
}

func newConsoleEncoder(color bool) zapcore.Encoder {
	// Links are resolved against the directory the process was started in, so
	// it is read once, here, and a later os.Chdir does not break them.
	cwd, _ := os.Getwd()
	return &consoleEncoder{color: color, cwd: cwd, buf: bufPool.Get()}
}

func (e *consoleEncoder) Clone() zapcore.Encoder {
	c := *e
	c.buf = bufPool.Get()
	c.buf.Write(e.buf.Bytes())
	if e.tail != nil {
		c.tail = bufPool.Get()
		c.tail.Write(e.tail.Bytes())
	}
	return &c
}

func (e *consoleEncoder) EncodeEntry(ent zapcore.Entry, fields []zapcore.Field) (*buffer.Buffer, error) {
	line := bufPool.Get()

	e.dim(line, true)
	line.AppendTime(ent.Time, consoleTimeLayout)
	e.dim(line, false)
	line.AppendByte(' ')
	line.AppendString(levelText(ent.Level, e.color))
	if ent.LoggerName != "" {
		line.AppendByte(' ')
		line.AppendString(ent.LoggerName)
	}
	if ent.Caller.Defined {
		// The link needs white space on both sides; the color escapes around
		// it are gone by the time a console looks for links.
		line.AppendByte(' ')
		e.dim(line, true)
		line.AppendString(clickablePath(e.cwd, ent.Caller.File))
		line.AppendByte(':')
		line.AppendInt(int64(ent.Caller.Line))
		e.dim(line, false)
	}
	line.AppendByte(' ')
	line.AppendString(ent.Message)

	line.Write(e.buf.Bytes())
	w := &consoleEncoder{color: e.color, buf: line, ns: e.ns}
	for i := range fields {
		fields[i].AddTo(w)
	}
	if ent.Stack != "" {
		w.block(keyStack, ent.Stack)
	}
	line.AppendByte('\n')
	if e.tail != nil {
		line.Write(e.tail.Bytes())
	}
	if w.tail != nil {
		line.Write(w.tail.Bytes())
		w.tail.Free()
	}
	return line, nil
}

// block queues a value of several lines to be printed below the record.
func (e *consoleEncoder) block(k, v string) {
	if e.tail == nil {
		e.tail = bufPool.Get()
	}
	e.tail.AppendString("    ")
	e.dim(e.tail, true)
	e.tail.AppendString(e.ns)
	e.tail.AppendString(k)
	e.tail.AppendByte(':')
	e.dim(e.tail, false)
	e.tail.AppendByte('\n')
	for _, ln := range strings.Split(strings.TrimRight(v, "\n"), "\n") {
		e.tail.AppendString("      ")
		e.tail.AppendString(ln)
		e.tail.AppendByte('\n')
	}
}

func (e *consoleEncoder) dim(b *buffer.Buffer, on bool) {
	if !e.color {
		return
	}
	if on {
		b.AppendString(ansiDim)
	} else {
		b.AppendString(ansiReset)
	}
}

func (e *consoleEncoder) key(k string) {
	e.buf.AppendByte(' ')
	if e.fieldColor != "" {
		e.buf.AppendString(e.fieldColor)
		e.buf.AppendString(e.ns)
		e.buf.AppendString(k)
		e.buf.AppendByte('=')
		return // coloredField resets after the value
	}
	e.dim(e.buf, true)
	e.buf.AppendString(e.ns)
	e.buf.AppendString(k)
	e.buf.AppendByte('=')
	e.dim(e.buf, false)
}

// str writes a value so that it reads as one word: quoted when it is empty or
// contains a space, a quote, '=' or anything that does not print.
func (e *consoleEncoder) str(s string) {
	quote := s == ""
	for _, r := range s {
		if r == ' ' || r == '"' || r == '=' || !unicode.IsPrint(r) {
			quote = true
			break
		}
	}
	if quote {
		e.buf.AppendString(strconv.Quote(s))
	} else {
		e.buf.AppendString(s)
	}
}

// asJSON writes arrays, objects and values zap has no typed field for.
func (e *consoleEncoder) asJSON(k string, v any) {
	e.key(k)
	if b, err := json.Marshal(v); err == nil {
		e.buf.Write(b)
	} else {
		e.str(fmt.Sprintf("%+v", v))
	}
}

func (e *consoleEncoder) AddArray(k string, v zapcore.ArrayMarshaler) error {
	m := zapcore.NewMapObjectEncoder()
	if err := m.AddArray(k, v); err != nil {
		return err
	}
	e.asJSON(k, m.Fields[k])
	return nil
}

func (e *consoleEncoder) AddObject(k string, v zapcore.ObjectMarshaler) error {
	m := zapcore.NewMapObjectEncoder()
	if err := m.AddObject(k, v); err != nil {
		return err
	}
	e.asJSON(k, m.Fields[k])
	return nil
}

func (e *consoleEncoder) AddReflected(k string, v any) error {
	if b, ok := v.(Blocker); ok {
		if text := b.LogBlock(e.color); text != "" {
			e.block(k, text)
			return nil
		}
	}
	e.asJSON(k, v)
	return nil
}

func (e *consoleEncoder) OpenNamespace(k string) { e.ns += k + "." }

func (e *consoleEncoder) AddString(k, v string) {
	if strings.Contains(v, "\n") {
		e.block(k, v)
		return
	}
	e.key(k)
	e.str(v)
}
func (e *consoleEncoder) AddByteString(k string, v []byte) { e.AddString(k, string(v)) }
func (e *consoleEncoder) AddBool(k string, v bool)         { e.key(k); e.buf.AppendBool(v) }
func (e *consoleEncoder) AddInt64(k string, v int64)       { e.key(k); e.buf.AppendInt(v) }
func (e *consoleEncoder) AddUint64(k string, v uint64)     { e.key(k); e.buf.AppendUint(v) }
func (e *consoleEncoder) AddFloat64(k string, v float64)   { e.key(k); e.buf.AppendFloat(v, 64) }
func (e *consoleEncoder) AddFloat32(k string, v float32)   { e.key(k); e.buf.AppendFloat(float64(v), 32) }

func (e *consoleEncoder) AddDuration(k string, v time.Duration) {
	e.key(k)
	e.buf.AppendString(v.String())
}
func (e *consoleEncoder) AddTime(k string, v time.Time) {
	e.key(k)
	e.buf.AppendTime(v, jsonTimeLayout)
}

func (e *consoleEncoder) AddBinary(k string, v []byte) {
	e.key(k)
	e.buf.AppendString(base64.StdEncoding.EncodeToString(v))
}

func (e *consoleEncoder) AddComplex128(k string, v complex128) {
	e.key(k)
	e.buf.AppendString(strconv.FormatComplex(v, 'f', -1, 128))
}

func (e *consoleEncoder) AddComplex64(k string, v complex64) {
	e.key(k)
	e.buf.AppendString(strconv.FormatComplex(complex128(v), 'f', -1, 64))
}

func (e *consoleEncoder) AddInt(k string, v int)         { e.AddInt64(k, int64(v)) }
func (e *consoleEncoder) AddInt32(k string, v int32)     { e.AddInt64(k, int64(v)) }
func (e *consoleEncoder) AddInt16(k string, v int16)     { e.AddInt64(k, int64(v)) }
func (e *consoleEncoder) AddInt8(k string, v int8)       { e.AddInt64(k, int64(v)) }
func (e *consoleEncoder) AddUint(k string, v uint)       { e.AddUint64(k, uint64(v)) }
func (e *consoleEncoder) AddUint32(k string, v uint32)   { e.AddUint64(k, uint64(v)) }
func (e *consoleEncoder) AddUint16(k string, v uint16)   { e.AddUint64(k, uint64(v)) }
func (e *consoleEncoder) AddUint8(k string, v uint8)     { e.AddUint64(k, uint64(v)) }
func (e *consoleEncoder) AddUintptr(k string, v uintptr) { e.AddUint64(k, uint64(v)) }
