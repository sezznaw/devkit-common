package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestZeroPasses(t *testing.T) {
	cases := map[string]bool{
		`len($)>0`:                           false,
		`$>0`:                                false,
		`len($)==6 && regexp('^[0-9]{6}$')`:  false,
		`$>=100000 && $<=9999999`:            false,
		`$>=1 && $<=2`:                       false,
		`$==1 || $==2`:                       false,
		`len($)>0 && len($)<=64`:             false,
		`len($)>0&&len($)<=8000`:             false,
		`len($)>=6 && len($)<=12`:            false,
		`regexp('^[a-z0-9]{4,10}$')`:         false,
		`email($)`:                           false,
		`len($)<=21`:                         true,
		`$>=0`:                               true,
		`$>=0 && $<=7`:                       true,
		`$==nil||$>=0`:                       true,
		`len($)==0 || regexp('^[0-9]+$', $)`: true,
		`$=='' || len($)<=3`:                 true,
		`!($>0)`:                             true,
		`len($)<=100 && (len($)==0 || len($)>=3)`: true,
	}
	for expr, want := range cases {
		got, err := zeroPasses(expr)
		if err != nil {
			t.Errorf("%s: %v", expr, err)
			continue
		}
		if got != want {
			t.Errorf("%s: zero passes = %v, want %v", expr, got, want)
		}
	}
	for _, bad := range []string{`in($, 1, 2)`, `len($)`, `$>`, `sprintf('%d', $)=='1'`} {
		if _, err := zeroPasses(bad); err == nil {
			t.Errorf("%s: expected an error", bad)
		}
	}
}

func TestRequiredRequestAndResponse(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "member"), 0o755)
	os.MkdirAll(filepath.Join(dir, "common"), 0o755)
	os.WriteFile(filepath.Join(dir, "common", "common.thrift"), []byte("namespace go common\n\nstruct PageReq {\n    1: i32 page\n    2: i32 page_size\n}\nstruct Money {\n    1: string amount\n    2: string currency\n}\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "member", "member.thrift"), []byte(`namespace go member

struct ListReq {
    1: common.PageReq page (api.body="page")
    2: string keyword (api.body="keyword")
    3: string username (api.body="username", api.vd="len($)>0 && len($)<=32")
    4: i32 kind (api.body="kind", api.vd="$>=0 && $<=3")
    5: optional i64 role_id (api.body="role_id", api.vd="$>0")
    6: required string tenant (api.body="tenant")
    7: common.Money min_balance (api.body="min_balance")
}
struct MemberInfo {
    1: i64 uid
    2: string username
    3: optional i32 status
    4: common.Money balance
}
struct Empty {}
`), 0o644)
	structs := structFields(dir)
	if got := structs["member.ListReq"]; len(got) != 7 || got[2].vd != "len($)>0 && len($)<=32" || got[4].kind != "optional" || got[5].kind != "required" {
		t.Fatalf("fields = %+v", got)
	}
	doc := `openapi: 3.0.3
info:
  title: t
  description: 会员接口。
paths:
  /v1/member/list:
    post:
      requestBody:
        content:
          application/json:
            schema:
              $ref: '#/components/schemas/member.ListReqBody'
      responses:
        "200":
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/member.MemberInfo'
components:
  schemas:
    member.ListReqBody:
      type: object
      properties:
        page:
          $ref: '#/components/schemas/member.PageReq'
        keyword: {type: string}
        username: {type: string}
        kind: {type: integer}
        role_id: {type: integer}
        tenant: {type: string}
        min_balance:
          $ref: '#/components/schemas/member.Money'
    member.Money:
      type: object
      properties:
        amount: {type: string}
        currency: {type: string}
    member.PageReq:
      type: object
      properties:
        page: {type: integer}
        page_size: {type: integer}
    member.MemberInfo:
      type: object
      properties:
        uid: {type: integer}
        username: {type: string}
        status: {type: integer}
        balance:
          $ref: '#/components/schemas/member.Money'
    member.Empty:
      type: object
`
	var root yaml.Node
	if err := yaml.Unmarshal([]byte(doc), &root); err != nil {
		t.Fatal(err)
	}
	Required(root.Content[0], structs)
	out, _ := yaml.Marshal(&root)
	var d struct {
		Info       struct{ Description string }
		Components struct {
			Schemas map[string]struct {
				Required   []string
				Properties map[string]map[string]any
			}
		}
	}
	if err := yaml.Unmarshal(out, &d); err != nil {
		t.Fatal(err)
	}
	s := d.Components.Schemas
	// Money is under a filter (request) and under a balance (response): the request side gets its own
	// copy with the nullable marks, the response schema keeps exact required fields.
	if got := s["member.Money"].Required; strings.Join(got, ",") != "amount,currency" {
		t.Errorf("response Money required = %v, want amount,currency\n%s", got, out)
	}
	if _, ok := s["member.Money"].Properties["amount"]["nullable"]; ok {
		t.Errorf("response Money must not be nullable\n%s", out)
	}
	if req, ok := s["member.MoneyRequest"]; !ok || req.Properties["amount"]["nullable"] != true || len(req.Required) != 0 {
		t.Errorf("request copy of Money missing or not nullable: %+v\n%s", req, out)
	}
	if ref := fmt.Sprint(s["member.ListReqBody"].Properties["min_balance"]["allOf"]); !strings.Contains(ref, "#/components/schemas/member.MoneyRequest") {
		t.Errorf("request ref must point at MoneyRequest, got %s\n%s", ref, out)
	}
	if ref := s["member.MemberInfo"].Properties["balance"]["$ref"]; ref != "#/components/schemas/member.Money" {
		t.Errorf("response ref must stay at Money, got %v\n%s", ref, out)
	}
	if got := s["member.ListReqBody"].Required; strings.Join(got, ",") != "username,tenant" {
		t.Errorf("request required = %v, want username,tenant\n%s", got, out)
	}
	for _, f := range []string{"page", "keyword", "kind", "role_id"} {
		if s["member.ListReqBody"].Properties[f]["nullable"] != true {
			t.Errorf("request field %s is not nullable\n%s", f, out)
		}
	}
	for _, f := range []string{"username", "tenant"} {
		if _, ok := s["member.ListReqBody"].Properties["username"]["nullable"]; ok {
			t.Errorf("required field %s must not be nullable", f)
		}
	}
	if page := s["member.ListReqBody"].Properties["page"]; page["$ref"] != nil || page["allOf"] == nil {
		t.Errorf("a nullable $ref must be wrapped in allOf: %v", page)
	}
	if got := s["member.PageReq"].Required; len(got) != 0 {
		t.Errorf("PageReq reached from a request must have no required fields, got %v", got)
	}
	if s["member.PageReq"].Properties["page"]["nullable"] != true {
		t.Errorf("PageReq.page is not nullable")
	}
	if got := s["member.MemberInfo"].Required; strings.Join(got, ",") != "uid,username,balance" {
		t.Errorf("response required = %v, want uid,username,balance", got)
	}
	if _, ok := s["member.MemberInfo"].Properties["status"]["nullable"]; ok {
		t.Errorf("response fields are not marked nullable")
	}
	if !strings.Contains(d.Info.Description, "请求字段") {
		t.Errorf("info.description lacks the convention note: %q", d.Info.Description)
	}
}
