package main

import (
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Directives are the `// @docs ...` lines of the IDL: what the plugin cannot
// read from Thrift itself, written where the colleague already is.
//
//	// @docs title: Sportsbook 玩家网关
//	// @docs description: 玩家端所有 HTTP 接口。响应统一 {code, msg, data}。
//	// @docs tag member: 会员
//	// @docs tag wallet: 钱包
//
// A method's tag is the second segment of its path (/v1/<domain>/...),
// shown with the name the `tag` directive gives it; /ping and anything
// without a domain go under "系统".
type Directives struct {
	Title       string
	Description string
	Tags        []TagName // in the order written
}

type TagName struct{ Key, Name string }

var directive = regexp.MustCompile(`^\s*//\s*@docs\s+(title|description|tag)\s*(\S*?)\s*:\s*(.*?)\s*$`)

// ParseDirectives reads the @docs lines of a Thrift file.
func ParseDirectives(src []byte) Directives {
	var d Directives
	for _, line := range strings.Split(string(src), "\n") {
		m := directive.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		switch m[1] {
		case "title":
			d.Title = m[3]
		case "description":
			if d.Description != "" {
				d.Description += "\n"
			}
			d.Description += m[3]
		case "tag":
			d.Tags = append(d.Tags, TagName{Key: m[2], Name: m[3]})
		}
	}
	return d
}

const systemTag = "系统"

// PublicMethods are the service methods whose comment block carries a
// `// @public` line: no login required. Everything else needs a bearer token.
func PublicMethods(src []byte) map[string]bool {
	out := map[string]bool{}
	lines := strings.Split(string(src), "\n")
	pending := false
	for _, line := range lines {
		t := strings.TrimSpace(line)
		switch {
		case t == "":
			continue
		case strings.HasPrefix(t, "//"):
			if strings.Contains(t, "@public") {
				pending = true
			}
		default:
			if pending {
				if m := methodLine.FindStringSubmatch(line); m != nil {
					out[m[1]] = true
				}
			}
			pending = false
		}
	}
	return out
}

var methodLine = regexp.MustCompile(`^\s*\w+\s+(\w+)\s*\(`)

var auditLine = regexp.MustCompile(`@(audit|noaudit)\b`)

// AuditMethods are the service methods whose comment block says
// `// @audit` (true) or `// @noaudit` (false): hertzx.Audit records them
// (or not) regardless of their permission point.
func AuditMethods(src []byte) map[string]bool {
	out := map[string]bool{}
	var pending *bool
	for _, line := range strings.Split(string(src), "\n") {
		t := strings.TrimSpace(line)
		switch {
		case t == "":
			continue
		case strings.HasPrefix(t, "//"):
			if m := auditLine.FindStringSubmatch(t); m != nil {
				v := m[1] == "audit"
				pending = &v
			}
		default:
			if pending != nil {
				if m := methodLine.FindStringSubmatch(line); m != nil {
					out[m[1]] = *pending
				}
			}
			pending = nil
		}
	}
	return out
}

// Audits writes x-audit for the methods that said so.
func Audits(top *yaml.Node, audits map[string]bool) {
	paths := mapGet(top, "paths")
	if paths == nil || len(audits) == 0 {
		return
	}
	for i := 0; i+1 < len(paths.Content); i += 2 {
		item := paths.Content[i+1]
		for j := 0; j+1 < len(item.Content); j += 2 {
			op := item.Content[j+1]
			if op.Kind != yaml.MappingNode {
				continue
			}
			method := ""
			if n := mapGet(op, "operationId"); n != nil {
				method = n.Value
				if k := strings.LastIndex(method, "_"); k >= 0 {
					method = method[k+1:]
				}
			}
			mapDel(op, "x-audit")
			if v, ok := audits[method]; ok {
				if v {
					mapSet(op, "x-audit", "true")
				} else {
					mapSet(op, "x-audit", "false")
				}
			}
		}
	}
}

var permLine = regexp.MustCompile(`@perm\s+([A-Za-z0-9_.:*-]+)`)

var limitLine = regexp.MustCompile(`@limit\s+(\d+\s*/\s*[a-z]+|off)`)

// LimitMethods are the service methods whose comment block carries a
// `// @limit <count>/<s|m|h>` line: a request-rate limit per client IP on
// that route alone (hertzx.Guard), on top of the service-wide one.
func LimitMethods(src []byte) map[string]string {
	return taggedMethods(src, limitLine)
}

// taggedMethods maps each method to the capture of the last matching
// comment line in the block right above it.
func taggedMethods(src []byte, re *regexp.Regexp) map[string]string {
	out := map[string]string{}
	pending := ""
	for _, line := range strings.Split(string(src), "\n") {
		t := strings.TrimSpace(line)
		switch {
		case t == "":
			continue
		case strings.HasPrefix(t, "//"):
			if m := re.FindStringSubmatch(t); m != nil {
				pending = strings.ReplaceAll(m[1], " ", "")
			}
		default:
			if pending != "" {
				if m := methodLine.FindStringSubmatch(line); m != nil {
					out[m[1]] = pending
				}
			}
			pending = ""
		}
	}
	return out
}

// Limits writes each method's @limit as the operation's x-rate-limit.
func Limits(top *yaml.Node, limits map[string]string) {
	setExtension(top, "x-rate-limit", limits)
}

// setExtension sets (or clears) one x- field of every operation from a
// method -> value map.
func setExtension(top *yaml.Node, field string, values map[string]string) {
	paths := mapGet(top, "paths")
	if paths == nil || len(values) == 0 {
		return
	}
	for i := 0; i+1 < len(paths.Content); i += 2 {
		item := paths.Content[i+1]
		for j := 0; j+1 < len(item.Content); j += 2 {
			op := item.Content[j+1]
			if op.Kind != yaml.MappingNode {
				continue
			}
			method := ""
			if n := mapGet(op, "operationId"); n != nil {
				method = n.Value
				if k := strings.LastIndex(method, "_"); k >= 0 {
					method = method[k+1:]
				}
			}
			mapDel(op, field)
			if v := values[method]; v != "" {
				mapSet(op, field, v)
			}
		}
	}
}

// PermMethods are the service methods whose comment block carries a
// `// @perm <point>` line, with the point: the gateway serves them only to
// a login holding that permission point (hertzx.RequirePermission).
func PermMethods(src []byte) map[string]string {
	out := map[string]string{}
	pending := ""
	for _, line := range strings.Split(string(src), "\n") {
		t := strings.TrimSpace(line)
		switch {
		case t == "":
			continue
		case strings.HasPrefix(t, "//"):
			if m := permLine.FindStringSubmatch(t); m != nil {
				pending = m[1]
			}
		default:
			if pending != "" {
				if m := methodLine.FindStringSubmatch(line); m != nil {
					out[m[1]] = pending
				}
			}
			pending = ""
		}
	}
	return out
}

// Permissions writes each method's permission point as the operation's
// x-permission, which hertzx.RequirePermission reads from the embedded
// document and hertzx.PermissionPoints lists for a role editor.
func Permissions(top *yaml.Node, perms map[string]string) {
	paths := mapGet(top, "paths")
	if paths == nil || len(perms) == 0 {
		return
	}
	for i := 0; i+1 < len(paths.Content); i += 2 {
		item := paths.Content[i+1]
		for j := 0; j+1 < len(item.Content); j += 2 {
			op := item.Content[j+1]
			if op.Kind != yaml.MappingNode {
				continue
			}
			method := ""
			if n := mapGet(op, "operationId"); n != nil {
				method = n.Value
				if k := strings.LastIndex(method, "_"); k >= 0 {
					method = method[k+1:]
				}
			}
			mapDel(op, "x-permission")
			if p := perms[method]; p != "" {
				mapSet(op, "x-permission", p)
			}
		}
	}
}

// Secure adds the bearer security scheme and requires it on every operation
// except the public ones (an empty security list, which is how the gateway's
// RequireLogin recognises them in the embedded document).
func Secure(top *yaml.Node, public map[string]bool) {
	paths := mapGet(top, "paths")
	if paths == nil {
		return
	}
	for i := 0; i+1 < len(paths.Content); i += 2 {
		item := paths.Content[i+1]
		for j := 0; j+1 < len(item.Content); j += 2 {
			op := item.Content[j+1]
			if op.Kind != yaml.MappingNode {
				continue
			}
			opID := ""
			if n := mapGet(op, "operationId"); n != nil {
				opID = n.Value
			}
			method := opID
			if k := strings.LastIndex(opID, "_"); k >= 0 {
				method = opID[k+1:]
			}
			mapDel(op, "security")
			if public[method] {
				op.Content = append(op.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: "security"}, &yaml.Node{Kind: yaml.SequenceNode, Style: yaml.FlowStyle})
			} else {
				req := &yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{{Kind: yaml.ScalarNode, Value: "bearerAuth"}, {Kind: yaml.SequenceNode, Style: yaml.FlowStyle}}}
				op.Content = append(op.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: "security"}, &yaml.Node{Kind: yaml.SequenceNode, Content: []*yaml.Node{req}})
			}
		}
	}
	comp := mapGet(top, "components")
	if comp == nil {
		comp = &yaml.Node{Kind: yaml.MappingNode}
		top.Content = append(top.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: "components"}, comp)
	}
	mapDel(comp, "securitySchemes")
	scheme := &yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{
		{Kind: yaml.ScalarNode, Value: "type"}, {Kind: yaml.ScalarNode, Value: "http"},
		{Kind: yaml.ScalarNode, Value: "scheme"}, {Kind: yaml.ScalarNode, Value: "bearer"},
		{Kind: yaml.ScalarNode, Value: "bearerFormat"}, {Kind: yaml.ScalarNode, Value: "JWT"},
		{Kind: yaml.ScalarNode, Value: "description"}, {Kind: yaml.ScalarNode, Value: "登录接口返回的 access_token；Authorization: Bearer <token>。标了 @public 的接口不需要。"},
	}}
	comp.Content = append(comp.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: "securitySchemes"}, &yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{{Kind: yaml.ScalarNode, Value: "bearerAuth"}, scheme}})
}

// Localize rewrites the generated document for people: every operation gets
// a summary (the first line of its IDL comment) and a tag named after its
// domain, the top-level tag list follows the directives' order, and the
// plugin's English placeholders become Chinese.
func Localize(top *yaml.Node, d Directives) {
	names := map[string]string{}
	for _, t := range d.Tags {
		names[t.Key] = t.Name
	}
	used := map[string]bool{}
	var order []string
	paths := mapGet(top, "paths")
	if paths != nil {
		for i := 0; i+1 < len(paths.Content); i += 2 {
			path, item := paths.Content[i].Value, paths.Content[i+1]
			tag := tagFor(path, names)
			for j := 0; j+1 < len(item.Content); j += 2 {
				op := item.Content[j+1]
				if op.Kind != yaml.MappingNode {
					continue
				}
				setTag(op, tag)
				if !used[tag] {
					used[tag] = true
					order = append(order, tag)
				}
				if desc := mapGet(op, "description"); desc != nil && mapGet(op, "summary") == nil {
					summary, rest := splitSummary(desc.Value)
					if summary != "" {
						mapSet(op, "summary", summary)
					}
					if rest == "" {
						mapDel(op, "description")
					} else {
						desc.Value = rest
					}
				}
				if resp := mapGet(op, "responses"); resp != nil {
					for k := 0; k+1 < len(resp.Content); k += 2 {
						if dn := mapGet(resp.Content[k+1], "description"); dn != nil && dn.Value == "Successful response" {
							dn.Value = "成功。业务结果看 code：0 成功，其余见错误码表"
						}
					}
				}
			}
		}
	}
	// Tag list: directives first (those in use), then the rest as met.
	var tags []*yaml.Node
	seen := map[string]bool{}
	add := func(name string) {
		if seen[name] || !used[name] {
			return
		}
		seen[name] = true
		tags = append(tags, &yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{{Kind: yaml.ScalarNode, Value: "name"}, {Kind: yaml.ScalarNode, Value: name}}})
	}
	for _, t := range d.Tags {
		add(t.Name)
	}
	for _, n := range order {
		add(n)
	}
	if len(tags) > 0 {
		mapDel(top, "tags")
		top.Content = append(top.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: "tags"}, &yaml.Node{Kind: yaml.SequenceNode, Content: tags})
	}
}

func tagFor(path string, names map[string]string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) < 2 {
		return systemTag
	}
	key := parts[1]
	if n, ok := names[key]; ok {
		return n
	}
	return key
}

func setTag(op *yaml.Node, tag string) {
	seq := &yaml.Node{Kind: yaml.SequenceNode, Content: []*yaml.Node{{Kind: yaml.ScalarNode, Value: tag}}}
	if t := mapGet(op, "tags"); t != nil {
		*t = *seq
		return
	}
	op.Content = append([]*yaml.Node{{Kind: yaml.ScalarNode, Value: "tags"}, seq}, op.Content...)
}

// splitSummary: the first sentence of the first line (up to the first 。 or
// ". ") is the summary, the rest the description. Directive lines (`@public`,
// `@docs ...`) are the IDL's, not the reader's, and are dropped: the plugin
// copies every comment line above a method, directives included.
func splitSummary(s string) (summary, rest string) {
	var lines []string
	for _, l := range strings.Split(s, "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "@") {
			continue
		}
		lines = append(lines, l)
	}
	if len(lines) == 0 {
		return "", ""
	}
	first, tail := lines[0], strings.Join(lines[1:], "\n")
	for _, sep := range []string{"。", ". ", "：", ": "} {
		if i := strings.Index(first, sep); i > 0 && i < len(first)-len(sep) {
			summary, after := strings.TrimSpace(first[:i]), strings.TrimSpace(first[i+len(sep):])
			if tail != "" {
				after = strings.TrimSpace(after + "\n" + tail)
			}
			return summary, after
		}
	}
	return first, tail
}

// Envelope wraps every 200 response schema in the gateway's envelope
// {code, msg, data}: the IDL method returns its data struct and the handler
// answers hertzx.OK / Fail, which add the envelope at runtime, so the
// document adds it here to show exactly what the client receives. A method
// that returns common.Empty documents {code, msg} with no data.
func Envelope(top *yaml.Node) {
	paths := mapGet(top, "paths")
	if paths == nil {
		return
	}
	for i := 0; i+1 < len(paths.Content); i += 2 {
		item := paths.Content[i+1]
		for j := 0; j+1 < len(item.Content); j += 2 {
			op := item.Content[j+1]
			resp := mapGet(op, "responses")
			if resp == nil {
				continue
			}
			for k := 0; k+1 < len(resp.Content); k += 2 {
				if resp.Content[k].Value != "200" {
					continue
				}
				content := mapGet(resp.Content[k+1], "content")
				if content == nil {
					continue
				}
				js := mapGet(content, "application/json")
				if js == nil {
					continue
				}
				schema := mapGet(js, "schema")
				if schema == nil || mapGet(schema, "code") != nil {
					continue
				}
				wrapped := envelopeSchema(schema)
				for m := 0; m+1 < len(js.Content); m += 2 {
					if js.Content[m].Value == "schema" {
						js.Content[m+1] = wrapped
					}
				}
			}
		}
	}
}

func envelopeSchema(data *yaml.Node) *yaml.Node {
	scalar := func(v string) *yaml.Node { return &yaml.Node{Kind: yaml.ScalarNode, Value: v} }
	prop := func(typ, format, desc string) *yaml.Node {
		n := &yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{scalar("type"), scalar(typ)}}
		if format != "" {
			n.Content = append(n.Content, scalar("format"), scalar(format))
		}
		n.Content = append(n.Content, scalar("description"), scalar(desc))
		return n
	}
	props := &yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{
		scalar("code"), prop("integer", "int32", "0 成功；其他见错误码表（idl/errors.md）"),
		scalar("msg"), prop("string", "", "给人看的说明，成功时为空"),
	}}
	empty := false
	if ref := mapGet(data, "$ref"); ref != nil && strings.Contains(ref.Value, "Empty") {
		empty = true
	}
	if !empty {
		props.Content = append(props.Content, scalar("data"), data)
	}
	return &yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{
		scalar("type"), scalar("object"),
		scalar("required"), {Kind: yaml.SequenceNode, Content: []*yaml.Node{scalar("code"), scalar("msg")}},
		scalar("properties"), props,
	}}
}
