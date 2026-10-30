package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Thrift fields are present unless declared `optional`, and the generated Go
// structs always serialize them, so in a *response* schema every field that
// is not optional is `required`: a front end gets `uid: number` instead of
// `uid?: number`.
//
// A *request* schema is a contract the gateway has to keep, and Hertz keeps
// less than that: a missing or `null` JSON member binds to the zero value and
// is refused only when the field carries an `api.vd` rule that rejects it, or
// when the Thrift says `required`. Documenting every non-optional request
// field as `required` therefore promised more than the gateway checked (a QA
// round filed it as a defect, sportsbook TY-107, 2026-10-21). So on the
// request side a field is `required` only when its api.vd rejects the zero
// value (zeroPasses evaluates the expression with `$` empty) or the Thrift
// says `required`; every other request field is `nullable: true`, which is
// what the gateway does with it (null = not sent). A schema reached from any
// request body is a request schema. One that a response uses too (common.Money
// under a filter and under a balance) is split: the request side gets a copy
// named "<name>Request" carrying the nullable marks, the original stays a
// response schema with exact `required`, so that a client generator does not
// type every response amount as nullable because a request somewhere may omit it.

// field is one struct field of the Thrift sources.
type field struct {
	name string
	kind string // "", "optional" or "required": the Thrift requiredness
	vd   string // the api.vd expression, "" when the field has none
}

var (
	nsRE     = regexp.MustCompile(`(?m)^\s*namespace\s+go\s+([\w.]+)`)
	structRE = regexp.MustCompile(`(?m)^\s*struct\s+(\w+)\s*\{`)
	fieldRE  = regexp.MustCompile(`^\s*\d+\s*:\s*(optional\s+|required\s+)?[\w.<>, ]+?\s+(\w+)`)
	vdRE     = regexp.MustCompile(`api\.vd\s*=\s*"((?:[^"\\]|\\.)*)"`)
)

// structFields reads every .thrift under dir (recursively: the service's
// directory and its siblings such as common/) and returns, per
// "<namespace>.<Struct>", its fields in declaration order.
func structFields(dir string) map[string][]field {
	out := map[string][]field{}
	_ = filepath.WalkDir(dir, func(p string, e os.DirEntry, err error) error {
		if err != nil || e.IsDir() || !strings.HasSuffix(e.Name(), ".thrift") {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		src := string(b)
		ns := ""
		if m := nsRE.FindStringSubmatch(src); m != nil {
			ns = m[1]
		}
		cur := ""
		for _, line := range strings.Split(src, "\n") {
			if m := structRE.FindStringSubmatch(line); m != nil {
				cur = m[1]
				out[ns+"."+cur] = nil
				continue
			}
			if strings.HasPrefix(strings.TrimSpace(line), "}") {
				cur = ""
				continue
			}
			if cur == "" {
				continue
			}
			if m := fieldRE.FindStringSubmatch(line); m != nil {
				f := field{name: m[2], kind: strings.TrimSpace(m[1])}
				if v := vdRE.FindStringSubmatch(line); v != nil {
					f.vd = v[1]
				}
				out[ns+"."+cur] = append(out[ns+"."+cur], f)
			}
		}
		return nil
	})
	return out
}

// requestNote is appended to info.description so that a reader of the
// document knows the convention without reading this file.
const requestNote = "请求字段：列在 required 里的必须有值（缺失、null、空值都回 code 1001）；其余字段可以省略，传 null 等同未传。响应字段：列在 required 里的一定会出现。"

// Required sets `required` on every components.schemas entry whose name is
// "<ns>.<Struct>" or "<ns>.<Struct>Body" (the plugin's names, prefixed by
// prefixSchemas) from the Thrift declaration: response schemas by Thrift
// requiredness, request schemas by what the gateway refuses (see the top of
// this file), with `nullable: true` on the request fields it does not.
// Schemas without properties (common.Empty) and schemas that already carry
// `required` are left alone.
func Required(top *yaml.Node, structs map[string][]field) {
	comps := mapGet(top, "components")
	if comps == nil {
		return
	}
	schemas := mapGet(comps, "schemas")
	if schemas == nil {
		return
	}
	request := requestSchemas(top, schemas)
	original := splitShared(top, schemas, request)
	for i := 0; i+1 < len(schemas.Content); i += 2 {
		name, schema := schemas.Content[i].Value, schemas.Content[i+1]
		if o, ok := original[name]; ok {
			name = o
		}
		fields, ok := lookup(structs, name)
		props := mapGet(schema, "properties")
		if !ok || len(fields) == 0 || props == nil || mapGet(schema, "required") != nil {
			continue
		}
		seq := &yaml.Node{Kind: yaml.SequenceNode}
		for _, f := range fields {
			p := mapGet(props, f.name)
			if p == nil {
				continue
			}
			switch {
			case request[schemas.Content[i].Value] && requiredOnRequest(name, f):
				seq.Content = append(seq.Content, scalar(f.name))
			case request[schemas.Content[i].Value]:
				setNullable(p)
			case f.kind != "optional":
				seq.Content = append(seq.Content, scalar(f.name))
			}
		}
		if len(seq.Content) > 0 {
			schema.Content = append(schema.Content, scalar("required"), seq)
		}
	}
	if info := mapGet(top, "info"); info != nil {
		if d := mapGet(info, "description"); d != nil {
			if !strings.Contains(d.Value, requestNote) {
				d.Value = strings.TrimRight(d.Value, "\n") + "\n\n" + requestNote
			}
		} else {
			mapSet(info, "description", requestNote)
		}
	}
}

// lookup finds the Thrift struct behind a schema name: "<ns>.<Struct>" or
// "<ns>.<Struct>Body" (the plugin's name for a request body), else a struct
// of that name in another namespace, since prefixSchemas renames the shared
// structs of common/ into the domain's namespace (common.PageReq is
// member.PageReq in the document). Of several namespaces, common wins.
func lookup(structs map[string][]field, name string) ([]field, bool) {
	if f, ok := structs[name]; ok {
		return f, true
	}
	if f, ok := structs[strings.TrimSuffix(name, "Body")]; ok {
		return f, true
	}
	bare := name[strings.LastIndex(name, ".")+1:]
	bare = strings.TrimSuffix(bare, "Body")
	var found []field
	n := 0
	for k, f := range structs {
		if k[strings.LastIndex(k, ".")+1:] != bare {
			continue
		}
		if strings.HasPrefix(k, "common.") {
			return f, true
		}
		found = f
		n++
	}
	return found, n == 1
}

// requiredOnRequest says whether the gateway refuses a request that leaves
// the field out: the Thrift says `required` (hz emits the `required` tag and
// Hertz checks the member is there), or the field is not optional and its
// api.vd rejects the zero value. An expression zeroPasses cannot read is
// documented as required and reported, so that the writer adds a supported
// form rather than the document silently promising less.
func requiredOnRequest(schema string, f field) bool {
	switch f.kind {
	case "required":
		return true
	case "optional":
		return false
	}
	if f.vd == "" {
		return false
	}
	passes, err := zeroPasses(f.vd)
	if err != nil {
		fmt.Fprintf(os.Stderr, "apidoc: %s.%s: cannot evaluate api.vd %q (%v); documenting the field as required\n", schema, f.name, f.vd, err)
		return true
	}
	return !passes
}

// setNullable marks a property schema `nullable: true`. A bare `$ref` cannot
// carry siblings in OpenAPI 3.0, so it is wrapped in `allOf` first.
func setNullable(p *yaml.Node) {
	if p.Kind != yaml.MappingNode || mapGet(p, "nullable") != nil {
		return
	}
	if ref := mapGet(p, "$ref"); ref != nil {
		mapDel(p, "$ref")
		p.Content = append([]*yaml.Node{scalar("allOf"), {Kind: yaml.SequenceNode, Content: []*yaml.Node{{Kind: yaml.MappingNode, Content: []*yaml.Node{scalar("$ref"), scalar(ref.Value)}}}}}, p.Content...)
	}
	p.Content = append(p.Content, scalar("nullable"), &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: "true"})
}

// requestSchemas returns the names of the schemas reachable from any
// operation's requestBody, through $ref, transitively.
func requestSchemas(top, schemas *yaml.Node) map[string]bool {
	return reachable(top, schemas, "requestBody")
}

// responseSchemas returns the names of the schemas reachable from any
// operation's responses, through $ref, transitively.
func responseSchemas(top, schemas *yaml.Node) map[string]bool {
	return reachable(top, schemas, "responses")
}

// splitShared gives every schema that both a request and a response reach a
// request-side copy named "<name>Request" (a longer suffix when that name is
// taken): the copy is appended to schemas, every $ref on the request side
// (request bodies, request-only schemas, the copies themselves) is pointed at
// it, request is updated to hold the copies instead of the originals, and the
// map from copy name to original name is returned, for the Thrift lookup.
// 请求和响应共用的结构体拆成两份：请求侧用 "<名>Request" 副本（标 nullable），原 schema 只服务响应（精确的 required）。
func splitShared(top, schemas *yaml.Node, request map[string]bool) map[string]string {
	response := responseSchemas(top, schemas)
	rename := map[string]string{}
	for name := range request {
		if !response[name] {
			continue
		}
		c := name + "Request"
		for mapGet(schemas, c) != nil {
			c += "_"
		}
		rename[name] = c
	}
	if len(rename) == 0 {
		return nil
	}
	if paths := mapGet(top, "paths"); paths != nil {
		for i := 0; i+1 < len(paths.Content); i += 2 {
			ops := paths.Content[i+1]
			for j := 0; j+1 < len(ops.Content); j += 2 {
				if rb := mapGet(ops.Content[j+1], "requestBody"); rb != nil {
					rewriteRefs(rb, rename)
				}
			}
		}
	}
	for name := range request {
		if _, shared := rename[name]; !shared {
			if s := mapGet(schemas, name); s != nil {
				rewriteRefs(s, rename)
			}
		}
	}
	original := map[string]string{}
	names := make([]string, 0, len(rename))
	for name := range rename {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		c := copyNode(mapGet(schemas, name))
		rewriteRefs(c, rename)
		schemas.Content = append(schemas.Content, scalar(rename[name]), c)
		original[rename[name]] = name
		delete(request, name)
		request[rename[name]] = true
	}
	return original
}

// rewriteRefs points every $ref below n at the renamed schema.
func rewriteRefs(n *yaml.Node, rename map[string]string) {
	switch n.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			v := n.Content[i+1]
			if n.Content[i].Value == "$ref" && strings.HasPrefix(v.Value, schemaRefPrefix) {
				if to, ok := rename[strings.TrimPrefix(v.Value, schemaRefPrefix)]; ok {
					v.Value = schemaRefPrefix + to
				}
				continue
			}
			rewriteRefs(v, rename)
		}
	case yaml.SequenceNode, yaml.DocumentNode:
		for _, c := range n.Content {
			rewriteRefs(c, rename)
		}
	}
}

// copyNode is a deep copy of a yaml node.
func copyNode(n *yaml.Node) *yaml.Node {
	if n == nil {
		return nil
	}
	c := *n
	c.Content = make([]*yaml.Node, len(n.Content))
	for i, x := range n.Content {
		c.Content[i] = copyNode(x)
	}
	if n.Alias != nil {
		c.Alias = copyNode(n.Alias)
	}
	return &c
}

// reachable returns the names of the schemas reachable from the given member
// (requestBody or responses) of any operation, through $ref, transitively.
func reachable(top, schemas *yaml.Node, member string) map[string]bool {
	seen := map[string]bool{}
	paths := mapGet(top, "paths")
	if paths == nil {
		return seen
	}
	var stack []string
	for i := 0; i+1 < len(paths.Content); i += 2 {
		ops := paths.Content[i+1]
		for j := 0; j+1 < len(ops.Content); j += 2 {
			if rb := mapGet(ops.Content[j+1], member); rb != nil {
				stack = append(stack, refsIn(rb)...)
			}
		}
	}
	for len(stack) > 0 {
		n := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[n] {
			continue
		}
		s := mapGet(schemas, n)
		if s == nil {
			continue
		}
		seen[n] = true
		stack = append(stack, refsIn(s)...)
	}
	return seen
}

const schemaRefPrefix = "#/components/schemas/"

// refsIn lists the schema names of every $ref below n.
func refsIn(n *yaml.Node) []string {
	var out []string
	switch n.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			if n.Content[i].Value == "$ref" && strings.HasPrefix(n.Content[i+1].Value, schemaRefPrefix) {
				out = append(out, strings.TrimPrefix(n.Content[i+1].Value, schemaRefPrefix))
				continue
			}
			out = append(out, refsIn(n.Content[i+1])...)
		}
	case yaml.SequenceNode, yaml.DocumentNode:
		for _, c := range n.Content {
			out = append(out, refsIn(c)...)
		}
	}
	return out
}

// zeroPasses evaluates an api.vd expression (go-tagexpr, the validator Hertz
// runs after binding) with `$` as the zero value a missing or null member
// binds to, and reports whether the rule accepts it. The subset understood is
// what the rules of a gateway use: `$`, `len($)`, `mblen($)`, `regexp('...')`
// (also with `$` as a second argument), `email($)` / `phone($)` (the zero
// value never passes), numbers, strings, `nil`, `true`, `false`, the
// comparisons, `!`, `&&`, `||` and parentheses. Anything else is an error.
func zeroPasses(expr string) (bool, error) {
	p := &vdParser{toks: vdTokens(expr)}
	v, err := p.or()
	if err != nil {
		return false, err
	}
	if p.pos != len(p.toks) {
		return false, fmt.Errorf("unexpected %q", p.toks[p.pos])
	}
	if v.kind != vdBool {
		return false, fmt.Errorf("expression is not a condition")
	}
	return v.b, nil
}

type vdKind int

const (
	vdZero vdKind = iota // `$`: the zero value, compares as 0, "" and nil
	vdNum
	vdStr
	vdBool
	vdNil
)

type vdVal struct {
	kind vdKind
	n    float64
	s    string
	b    bool
}

func vdTokens(s string) []string {
	var toks []string
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == ' ' || c == '\t':
			i++
		case strings.HasPrefix(s[i:], "&&") || strings.HasPrefix(s[i:], "||") || strings.HasPrefix(s[i:], "==") ||
			strings.HasPrefix(s[i:], "!=") || strings.HasPrefix(s[i:], "<=") || strings.HasPrefix(s[i:], ">="):
			toks = append(toks, s[i:i+2])
			i += 2
		case strings.ContainsRune("!<>()$,-", rune(c)):
			toks = append(toks, s[i:i+1])
			i++
		case c == '\'' || c == '"':
			j := i + 1
			for j < len(s) && s[j] != c {
				if s[j] == '\\' {
					j++
				}
				j++
			}
			if j >= len(s) {
				j = len(s) - 1
			}
			toks = append(toks, s[i:j+1])
			i = j + 1
		default:
			j := i
			for j < len(s) && (s[j] == '_' || s[j] == '.' || s[j] >= '0' && s[j] <= '9' || s[j] >= 'a' && s[j] <= 'z' || s[j] >= 'A' && s[j] <= 'Z') {
				j++
			}
			if j == i {
				j++
			}
			toks = append(toks, s[i:j])
			i = j
		}
	}
	return toks
}

type vdParser struct {
	toks []string
	pos  int
}

func (p *vdParser) peek() string {
	if p.pos < len(p.toks) {
		return p.toks[p.pos]
	}
	return ""
}

func (p *vdParser) next() string {
	t := p.peek()
	p.pos++
	return t
}

func (p *vdParser) expect(t string) error {
	if p.next() != t {
		return fmt.Errorf("expected %q", t)
	}
	return nil
}

func (p *vdParser) or() (vdVal, error) {
	l, err := p.and()
	if err != nil {
		return l, err
	}
	for p.peek() == "||" {
		p.next()
		r, err := p.and()
		if err != nil {
			return r, err
		}
		if l.kind != vdBool || r.kind != vdBool {
			return l, fmt.Errorf("|| needs conditions")
		}
		l = vdVal{kind: vdBool, b: l.b || r.b}
	}
	return l, nil
}

func (p *vdParser) and() (vdVal, error) {
	l, err := p.not()
	if err != nil {
		return l, err
	}
	for p.peek() == "&&" {
		p.next()
		r, err := p.not()
		if err != nil {
			return r, err
		}
		if l.kind != vdBool || r.kind != vdBool {
			return l, fmt.Errorf("&& needs conditions")
		}
		l = vdVal{kind: vdBool, b: l.b && r.b}
	}
	return l, nil
}

func (p *vdParser) not() (vdVal, error) {
	if p.peek() == "!" {
		p.next()
		v, err := p.not()
		if err != nil {
			return v, err
		}
		if v.kind != vdBool {
			return v, fmt.Errorf("! needs a condition")
		}
		return vdVal{kind: vdBool, b: !v.b}, nil
	}
	return p.cmp()
}

func (p *vdParser) cmp() (vdVal, error) {
	l, err := p.prim()
	if err != nil {
		return l, err
	}
	op := p.peek()
	switch op {
	case "==", "!=", "<", "<=", ">", ">=":
	default:
		return l, nil
	}
	p.next()
	r, err := p.prim()
	if err != nil {
		return r, err
	}
	return compare(l, op, r)
}

func (p *vdParser) prim() (vdVal, error) {
	t := p.next()
	switch {
	case t == "":
		return vdVal{}, fmt.Errorf("unexpected end")
	case t == "(":
		v, err := p.or()
		if err != nil {
			return v, err
		}
		return v, p.expect(")")
	case t == "$":
		return vdVal{kind: vdZero}, nil
	case t == "nil":
		return vdVal{kind: vdNil}, nil
	case t == "true", t == "false":
		return vdVal{kind: vdBool, b: t == "true"}, nil
	case t == "-":
		v, err := p.prim()
		if err != nil {
			return v, err
		}
		if v.kind != vdNum {
			return v, fmt.Errorf("- needs a number")
		}
		v.n = -v.n
		return v, nil
	case t[0] == '\'' || t[0] == '"':
		if len(t) < 2 || t[len(t)-1] != t[0] {
			return vdVal{}, fmt.Errorf("unterminated string")
		}
		return vdVal{kind: vdStr, s: t[1 : len(t)-1]}, nil
	case t[0] >= '0' && t[0] <= '9' || t[0] == '.':
		n, err := strconv.ParseFloat(t, 64)
		if err != nil {
			return vdVal{}, fmt.Errorf("bad number %q", t)
		}
		return vdVal{kind: vdNum, n: n}, nil
	case t == "len" || t == "mblen":
		if err := p.expect("("); err != nil {
			return vdVal{}, err
		}
		if err := p.expect("$"); err != nil {
			return vdVal{}, err
		}
		return vdVal{kind: vdNum, n: 0}, p.expect(")")
	case t == "regexp":
		if err := p.expect("("); err != nil {
			return vdVal{}, err
		}
		pat, err := p.prim()
		if err != nil {
			return pat, err
		}
		if pat.kind != vdStr {
			return vdVal{}, fmt.Errorf("regexp needs a pattern")
		}
		if p.peek() == "," {
			p.next()
			if err := p.expect("$"); err != nil {
				return vdVal{}, err
			}
		}
		if err := p.expect(")"); err != nil {
			return vdVal{}, err
		}
		re, err := regexp.Compile(pat.s)
		if err != nil {
			return vdVal{}, fmt.Errorf("regexp %q: %v", pat.s, err)
		}
		return vdVal{kind: vdBool, b: re.MatchString("")}, nil
	case t == "email" || t == "phone":
		if err := p.expect("("); err != nil {
			return vdVal{}, err
		}
		if err := p.expect("$"); err != nil {
			return vdVal{}, err
		}
		for p.peek() == "," { // phone($, 'CN')
			p.next()
			if _, err := p.prim(); err != nil {
				return vdVal{}, err
			}
		}
		return vdVal{kind: vdBool, b: false}, p.expect(")")
	}
	return vdVal{}, fmt.Errorf("unknown %q", t)
}

func compare(l vdVal, op string, r vdVal) (vdVal, error) {
	// The zero value takes the shape of what it is compared with.
	if l.kind == vdZero {
		l = zeroAs(r)
	}
	if r.kind == vdZero {
		r = zeroAs(l)
	}
	eq := op == "==" || op == "!="
	var res bool
	switch {
	case l.kind == vdNum && r.kind == vdNum:
		res = cmpOrdered(l.n, op, r.n)
	case l.kind == vdStr && r.kind == vdStr:
		res = cmpOrdered(l.s, op, r.s)
	case eq && l.kind == vdBool && r.kind == vdBool:
		res = (l.b == r.b) == (op == "==")
	case eq && l.kind == vdNil && r.kind == vdNil:
		res = op == "=="
	case eq && (l.kind == vdNil || r.kind == vdNil):
		res = op == "!=" // a number or string is never nil
	default:
		return vdVal{}, fmt.Errorf("cannot compare with %s", op)
	}
	return vdVal{kind: vdBool, b: res}, nil
}

// zeroAs is the zero value in the type of the other operand: 0, "", or nil
// (go-tagexpr compares an unset field equal to nil).
func zeroAs(other vdVal) vdVal {
	switch other.kind {
	case vdStr:
		return vdVal{kind: vdStr}
	case vdNil:
		return vdVal{kind: vdNil}
	case vdBool:
		return vdVal{kind: vdBool}
	case vdZero:
		return vdVal{kind: vdNil}
	}
	return vdVal{kind: vdNum}
}

func cmpOrdered[T float64 | string](a T, op string, b T) bool {
	switch op {
	case "==":
		return a == b
	case "!=":
		return a != b
	case "<":
		return a < b
	case "<=":
		return a <= b
	case ">":
		return a > b
	}
	return a >= b
}

func scalar(v string) *yaml.Node { return &yaml.Node{Kind: yaml.ScalarNode, Value: v} }
