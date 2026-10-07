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
					mapSet(op, "summary", summary)
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

// splitSummary: the first line (or sentence, up to the first 。 or .) is the
// summary, the rest the description.
func splitSummary(s string) (summary, rest string) {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\n"); i >= 0 {
		return strings.TrimSpace(s[:i]), strings.TrimSpace(s[i+1:])
	}
	for _, sep := range []string{"。", ". ", "：", ": "} {
		if i := strings.Index(s, sep); i > 0 && i < len(s)-len(sep) {
			return strings.TrimSpace(s[:i]), strings.TrimSpace(s[i+len(sep):])
		}
	}
	return s, ""
}
