// Command apidoc turns the Thrift IDL of an API service into an OpenAPI 3
// document, so that a service's documentation page (/docs) is generated from
// the same file as its routes and never drifts from them.
//
// It runs thriftgo with the CloudWeGo plugin thrift-gen-http-swagger
// (installed by `make tools`) on a copy of the IDL in which every struct field
// without an annotation gets `api.body="<name>"`: the plugin only documents
// annotated fields, and in this project a field without one is a JSON body
// field (the API convention is POST + JSON). A method returns its data
// struct and the handler answers hertzx.OK / Fail, which add the envelope
// {code, msg, data} at runtime; the document adds the same envelope around
// every response schema (Envelope), so it shows exactly what the client
// receives. Afterwards the title and version are filled in.
//
// `// @docs title: ...`, `// @docs description: ...` and `// @docs tag <domain>: <名字>`
// lines in the IDL name the document and its sections (docs.go); the first
// line of a method's comment is its summary. A `// @public` line in a
// method's comment marks it as needing no login; every other operation gets
// the bearer security requirement, which is also what the gateway's
// RequireLogin reads to decide what to protect. -title / -description on the
// command line win over the directives.
//
//	go run github.com/sezznaw/devkit-common/cmd/apidoc -dir ../idl/ser-api -out cmd/ser-api/openapi.yaml -version v1.2.3
//
// -dir documents a gateway whose IDL is a directory, one Thrift file per
// domain (dir.go); -idl documents a single file.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

func main() {
	idl := flag.String("idl", "", "the service's Thrift IDL (with api.* annotations); one file")
	dir := flag.String("dir", "", "the service's IDL directory: one Thrift file per domain, merged into one document")
	out := flag.String("out", "openapi.yaml", "where to write the OpenAPI document")
	title := flag.String("title", "", "info.title (default: the IDL's file name)")
	version := flag.String("version", "", "info.version (default: dev)")
	desc := flag.String("description", "", "info.description")
	flag.Parse()
	if *idl == "" && *dir == "" {
		fmt.Fprintln(os.Stderr, "apidoc: -idl <file> or -dir <directory> is required")
		os.Exit(2)
	}
	var err error
	if *dir != "" {
		err = runDir(*dir, *out, *title, *version, *desc)
	} else {
		err = run(*idl, *out, *title, *version, *desc)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "apidoc:", err)
		os.Exit(1)
	}
}

func run(idl, out, title, version, desc string) error {
	if err := needTools(); err != nil {
		return err
	}
	tmp, work, err := stage(filepath.Dir(idl))
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	doc, err := plugin(work, filepath.Base(idl))
	if err != nil {
		return err
	}
	src, err := os.ReadFile(idl)
	if err != nil {
		return err
	}
	d := ParseDirectives(src)
	if title == "" {
		title = d.Title
	}
	if desc == "" {
		desc = d.Description
	}
	patched, err := Patch(doc, title, version, desc, strings.TrimSuffix(filepath.Base(idl), ".thrift"), d, PublicMethods(src), PermMethods(src), AuditMethods(src), LimitMethods(src), MaxBodyMethods(src), SensitiveMethods(src))
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return err
	}
	return os.WriteFile(out, patched, 0o644)
}

var (
	structStart = regexp.MustCompile(`^\s*struct\s+\w+\s*\{`)
	// "1: optional i64 uid" with no "(...)" annotation after the name.
	fieldLine = regexp.MustCompile(`^(\s*\d+\s*:\s*(?:optional\s+|required\s+)?[\w.<>, ]+?\s+(\w+))\s*([,;]?)\s*(//.*|#.*)?$`)
)

// Annotate adds api.body="<field>" to every struct field that has no
// annotation, so the document covers it. Fields that have one (api.query,
// api.path, api.header, api.body, openapi.*) are left alone, and so is
// everything outside struct bodies.
func Annotate(src []byte) []byte {
	var b strings.Builder
	in := false
	for _, line := range strings.Split(string(src), "\n") {
		t := strings.TrimSpace(line)
		switch {
		case structStart.MatchString(line):
			in = true
		case strings.HasPrefix(t, "}"):
			in = false
		case in && !strings.Contains(line, "(") && !strings.HasPrefix(t, "//") && !strings.HasPrefix(t, "#") && t != "":
			if m := fieldLine.FindStringSubmatch(line); m != nil {
				line = fmt.Sprintf(`%s (api.body="%s")%s`, m[1], m[2], m[3])
				if m[4] != "" {
					line += "  " + m[4]
				}
			}
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return []byte(b.String())
}

// Patch fills info.title / version / description, keeping the document's
// order (a yaml.Node edit, not a map round trip).
func Patch(doc []byte, title, version, desc, fallback string, d Directives, public map[string]bool, perms map[string]string, audits map[string]bool, limits, maxBodies, sensitive map[string]string) ([]byte, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(doc, &root); err != nil {
		return nil, fmt.Errorf("parse openapi.yaml: %w", err)
	}
	if root.Kind != yaml.DocumentNode || len(root.Content) == 0 || root.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("openapi.yaml is not a mapping")
	}
	top := root.Content[0]
	info := mapGet(top, "info")
	if info == nil {
		info = &yaml.Node{Kind: yaml.MappingNode}
		top.Content = append([]*yaml.Node{{Kind: yaml.ScalarNode, Value: "info"}, info}, top.Content...)
	}
	if title == "" {
		title = fallback
	}
	if version == "" {
		version = "dev"
	}
	mapSet(info, "title", title)
	mapSet(info, "version", version)
	if desc != "" {
		mapSet(info, "description", desc)
	} else if d := mapGet(info, "description"); d != nil && d.Value == "API description" {
		mapDel(info, "description")
	}
	Localize(top, d)
	Secure(top, public)
	Permissions(top, perms)
	Audits(top, audits)
	Limits(top, limits)
	MaxBodies(top, maxBodies)
	Sensitives(top, sensitive)
	Envelope(top)
	out, err := yaml.Marshal(&root)
	if err != nil {
		return nil, err
	}
	return append([]byte("# Generated by `make gen` (devkit-common cmd/apidoc) from the service's IDL. Do not edit.\n"), out...), nil
}

func mapGet(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func mapSet(m *yaml.Node, key, val string) {
	if n := mapGet(m, key); n != nil {
		n.Kind, n.Tag, n.Value, n.Style = yaml.ScalarNode, "", val, 0
		return
	}
	m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: key}, &yaml.Node{Kind: yaml.ScalarNode, Value: val})
}

func mapDel(m *yaml.Node, key string) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content = append(m.Content[:i], m.Content[i+2:]...)
			return
		}
	}
}
