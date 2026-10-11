package main

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

var errConst = regexp.MustCompile(`^\s*const\s+i32\s+(\w+)\s*=\s*(\d+)`)

// errorCodes reads every `const i32 Name = code` of the Thrift files in dir
// with the comment line above it, and returns them as a YAML sequence of
// {code, name, message, domain} sorted by code; nil when there is no such
// directory.
func errorCodes(dir string) *yaml.Node {
	files, _ := filepath.Glob(filepath.Join(dir, "*.thrift"))
	if len(files) == 0 {
		return nil
	}
	sort.Strings(files)
	type entry struct {
		code                  int
		name, message, domain string
	}
	var entries []entry
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		domain := strings.TrimSuffix(filepath.Base(f), ".thrift")
		lines := strings.Split(string(b), "\n")
		for i, line := range lines {
			m := errConst.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			code, _ := strconv.Atoi(m[2])
			msg := ""
			if i > 0 && strings.HasPrefix(strings.TrimSpace(lines[i-1]), "//") {
				msg = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(lines[i-1]), "//"))
			}
			entries = append(entries, entry{code, m[1], msg, domain})
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].code < entries[j].code })
	seq := &yaml.Node{Kind: yaml.SequenceNode}
	scalar := func(v string) *yaml.Node { return &yaml.Node{Kind: yaml.ScalarNode, Value: v} }
	for _, e := range entries {
		seq.Content = append(seq.Content, &yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{
			scalar("code"), {Kind: yaml.ScalarNode, Tag: "!!int", Value: strconv.Itoa(e.code)},
			scalar("name"), scalar(e.name),
			scalar("message"), scalar(e.message),
			scalar("domain"), scalar(e.domain),
		}})
	}
	return seq
}
