// Command apidoc turns the Thrift IDL of an API service into an OpenAPI 3
// document, so that a service's documentation page (/docs) is generated from
// the same file as its routes and never drifts from them.
//
// It runs thriftgo with the CloudWeGo plugin thrift-gen-http-swagger
// (installed by `make tools`) on a copy of the IDL in which every struct field
// without an annotation gets `api.body="<name>"`: the plugin only documents
// annotated fields, and in this project a field without one is a JSON body
// field (the API convention is POST + JSON). The response structs carry the
// envelope {code, msg, data} themselves, so the document shows exactly what
// the client receives. Afterwards the title and version are filled in.
//
// `// @docs title: ...`, `// @docs description: ...` and `// @docs tag <domain>: <名字>`
// lines in the IDL name the document and its sections (docs.go); the first
// line of a method's comment is its summary. -title / -description on the
// command line win over the directives.
//
//	go run github.com/sezznaw/devkit-common/cmd/apidoc -idl ../idl/ser-api/ser-api.thrift -out cmd/ser-api/openapi.yaml -version v1.2.3
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

func main() {
	idl := flag.String("idl", "", "the service's Thrift IDL (with api.* annotations)")
	out := flag.String("out", "openapi.yaml", "where to write the OpenAPI document")
	title := flag.String("title", "", "info.title (default: the IDL's file name)")
	version := flag.String("version", "", "info.version (default: dev)")
	desc := flag.String("description", "", "info.description")
	flag.Parse()
	if *idl == "" {
		fmt.Fprintln(os.Stderr, "apidoc: -idl is required")
		os.Exit(2)
	}
	if err := run(*idl, *out, *title, *version, *desc); err != nil {
		fmt.Fprintln(os.Stderr, "apidoc:", err)
		os.Exit(1)
	}
}

func run(idl, out, title, version, desc string) error {
	if _, err := exec.LookPath("thriftgo"); err != nil {
		return fmt.Errorf("thriftgo not found in PATH (run: make tools)")
	}
	if _, err := exec.LookPath("thrift-gen-http-swagger"); err != nil {
		return fmt.Errorf("thrift-gen-http-swagger not found in PATH (run: make tools)")
	}
	tmp, err := os.MkdirTemp("", "apidoc-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	// Copy the IDL directory (includes are relative) with the annotations added.
	srcDir := filepath.Dir(idl)
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".thrift") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(srcDir, e.Name()))
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(tmp, e.Name()), Annotate(b), 0o644); err != nil {
			return err
		}
	}
	outDir := filepath.Join(tmp, "out")
	cmd := exec.Command("thriftgo", "-g", "go", "-p", "http-swagger:OutputDir="+outDir, filepath.Base(idl))
	cmd.Dir = tmp
	if b, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("thriftgo: %w\n%s", err, b)
	}
	doc, err := os.ReadFile(filepath.Join(outDir, "openapi.yaml"))
	if err != nil {
		return fmt.Errorf("the plugin wrote no openapi.yaml: %w", err)
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
	patched, err := Patch(doc, title, version, desc, strings.TrimSuffix(filepath.Base(idl), ".thrift"), d)
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
func Patch(doc []byte, title, version, desc, fallback string, d Directives) ([]byte, error) {
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
