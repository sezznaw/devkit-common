package hertzx

import (
	"bytes"

	"gopkg.in/yaml.v3"

	"github.com/sezznaw/devkit-common/kitexx"
)

// withDeviceHeaders documents the headers hertzx.Client reads in the OpenAPI
// document the gateway serves: a parameter component for X-Device-Type (with
// the configured values as enum, required when the configuration says so)
// and one for X-Device-Id, referenced from every operation. apidoc does not
// know the configuration, so this happens when the document is served; the
// front end's generator reads the served document and sees them.
func withDeviceHeaders(spec []byte, d kitexx.DeviceConfig) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(spec, &doc); err != nil {
		return nil, err
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return spec, nil
	}
	top := doc.Content[0]
	if paths := mapEntry(top, "paths"); paths != nil && paths.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(paths.Content); i += 2 {
			if isFrameworkPath(paths.Content[i].Value) {
				continue
			}
			item := paths.Content[i+1]
			if item.Kind != yaml.MappingNode {
				continue
			}
			for j := 0; j+1 < len(item.Content); j += 2 {
				op := item.Content[j+1]
				if !isHTTPMethod(item.Content[j].Value) || op.Kind != yaml.MappingNode {
					continue
				}
				params := mapEntry(op, "parameters")
				if params == nil {
					params = &yaml.Node{Kind: yaml.SequenceNode}
					op.Content = append(op.Content, scalarNode("parameters"), params)
				}
				params.Content = append(params.Content, refNode("#/components/parameters/XDeviceType"), refNode("#/components/parameters/XDeviceId"))
			}
		}
	}
	comp := mapEntry(top, "components")
	if comp == nil {
		comp = &yaml.Node{Kind: yaml.MappingNode}
		top.Content = append(top.Content, scalarNode("components"), comp)
	}
	params := mapEntry(comp, "parameters")
	if params == nil {
		params = &yaml.Node{Kind: yaml.MappingNode}
		comp.Content = append(comp.Content, scalarNode("parameters"), params)
	}
	enum := &yaml.Node{Kind: yaml.SequenceNode, Style: yaml.FlowStyle}
	for _, t := range d.Types {
		enum.Content = append(enum.Content, scalarNode(t))
	}
	params.Content = append(params.Content,
		scalarNode("XDeviceType"), headerParam(DeviceHeader, d.Required, "客户端设备类型（平台），每个请求都带", enum),
		scalarNode("XDeviceId"), headerParam(DeviceIDHeader, false, "客户端自己生成的设备标识，可选；1 到 128 位字母、数字、. _ : -", nil),
	)
	var out bytes.Buffer
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, err
	}
	return out.Bytes(), enc.Close()
}

func headerParam(name string, required bool, description string, enum *yaml.Node) *yaml.Node {
	schema := &yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{scalarNode("type"), scalarNode("string")}}
	if enum != nil {
		schema.Content = append(schema.Content, scalarNode("enum"), enum)
	}
	req := "false"
	if required {
		req = "true"
	}
	return &yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{
		scalarNode("name"), scalarNode(name),
		scalarNode("in"), scalarNode("header"),
		scalarNode("required"), {Kind: yaml.ScalarNode, Tag: "!!bool", Value: req},
		scalarNode("description"), scalarNode(description),
		scalarNode("schema"), schema,
	}}
}

func scalarNode(v string) *yaml.Node { return &yaml.Node{Kind: yaml.ScalarNode, Value: v} }

func refNode(ref string) *yaml.Node {
	return &yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{scalarNode("$ref"), scalarNode(ref)}}
}

// mapEntry is the value of key in a mapping node, nil when absent.
func mapEntry(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func isHTTPMethod(s string) bool {
	switch s {
	case "get", "post", "put", "patch", "delete", "head", "options":
		return true
	}
	return false
}
