package hertzx

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/sezznaw/devkit-common/kitexx"
)

func TestWithDeviceHeaders(t *testing.T) {
	spec := `openapi: 3.0.3
info:
  title: t
paths:
  /ping:
    get:
      summary: ping
  /v1/auth/login:
    post:
      summary: login
      parameters:
        - name: X-Trace-Id
          in: header
          schema: {type: string}
  /v1/member/getProfile:
    post:
      summary: profile
components:
  schemas:
    A: {type: object}
`
	d := kitexx.DeviceConfig{Required: true, Types: []string{"PC", "iOS"}}
	out, err := withDeviceHeaders([]byte(spec), d)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Paths map[string]map[string]struct {
			Parameters []map[string]any `yaml:"parameters"`
		} `yaml:"paths"`
		Components struct {
			Schemas    map[string]any `yaml:"schemas"`
			Parameters map[string]struct {
				Name     string `yaml:"name"`
				In       string `yaml:"in"`
				Required bool   `yaml:"required"`
				Schema   struct {
					Enum []string `yaml:"enum"`
				} `yaml:"schema"`
			} `yaml:"parameters"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if n := len(doc.Paths["/v1/auth/login"]["post"].Parameters); n != 3 {
		t.Fatalf("login parameters: %d\n%s", n, out)
	}
	if n := len(doc.Paths["/v1/member/getProfile"]["post"].Parameters); n != 2 {
		t.Fatalf("profile parameters: %d", n)
	}
	if doc.Paths["/ping"]["get"].Parameters != nil {
		t.Fatal("framework path got parameters")
	}
	p := doc.Components.Parameters["XDeviceType"]
	if p.Name != DeviceHeader || p.In != "header" || !p.Required || strings.Join(p.Schema.Enum, ",") != "PC,iOS" {
		t.Fatalf("XDeviceType: %+v", p)
	}
	if q := doc.Components.Parameters["XDeviceId"]; q.Name != DeviceIDHeader || q.Required {
		t.Fatalf("XDeviceId: %+v", q)
	}
	if _, ok := doc.Components.Schemas["A"]; !ok {
		t.Fatal("existing components lost")
	}
	// the order of paths is kept (the sidebar follows it)
	if i, j := strings.Index(string(out), "/v1/auth/login"), strings.Index(string(out), "/v1/member/getProfile"); i > j {
		t.Fatal("path order changed")
	}
}
