package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Thrift fields are present unless declared `optional`, and the generated
// Go structs always serialize them, so the OpenAPI schema should say so:
// a front end then gets `uid: number` instead of `uid?: number`. The plugin
// leaves every field optional; Required adds the `required` list to each
// struct schema from the Thrift sources.

var (
	nsRE     = regexp.MustCompile(`(?m)^\s*namespace\s+go\s+([\w.]+)`)
	structRE = regexp.MustCompile(`(?m)^\s*struct\s+(\w+)\s*\{`)
	fieldRE  = regexp.MustCompile(`^\s*\d+\s*:\s*(optional\s+|required\s+)?[\w.<>, ]+?\s+(\w+)`)
)

// requiredFields reads every .thrift under dir (recursively: the service's
// directory and its siblings such as common/) and returns, per
// "<namespace>.<Struct>", the fields that are not optional.
func requiredFields(dir string) map[string][]string {
	out := map[string][]string{}
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
		lines := strings.Split(src, "\n")
		cur := ""
		for _, line := range lines {
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
			if m := fieldRE.FindStringSubmatch(line); m != nil && strings.TrimSpace(m[1]) != "optional" {
				out[ns+"."+cur] = append(out[ns+"."+cur], m[2])
			}
		}
		return nil
	})
	return out
}

// Required sets `required` on every components.schemas entry whose name is
// "<ns>.<Struct>" or "<ns>.<Struct>Body" (the plugin's names, prefixed by
// prefixSchemas) from the Thrift declaration. Schemas without properties
// (common.Empty) are left alone.
func Required(top *yaml.Node, structs map[string][]string) {
	comps := mapGet(top, "components")
	if comps == nil {
		return
	}
	schemas := mapGet(comps, "schemas")
	if schemas == nil {
		return
	}
	for i := 0; i+1 < len(schemas.Content); i += 2 {
		name, schema := schemas.Content[i].Value, schemas.Content[i+1]
		fields, ok := structs[name]
		if !ok {
			fields, ok = structs[strings.TrimSuffix(name, "Body")]
		}
		if !ok || len(fields) == 0 || mapGet(schema, "properties") == nil || mapGet(schema, "required") != nil {
			continue
		}
		props := mapGet(schema, "properties")
		seq := &yaml.Node{Kind: yaml.SequenceNode}
		for _, f := range fields {
			if mapGet(props, f) != nil {
				seq.Content = append(seq.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: f})
			}
		}
		if len(seq.Content) > 0 {
			schema.Content = append(schema.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: "required"}, seq)
		}
	}
}
