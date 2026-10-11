package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRender(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "member.thrift"), []byte("namespace go errcode\n\n// 会员不存在\nconst i32 MemberNotFound = 2001\nconst i32 NoMsg = 2002\n"), 0o644)
	es, err := read(dir)
	if err != nil || len(es) != 2 {
		t.Fatalf("%v %v", es, err)
	}
	src, err := render(es)
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	for _, want := range []string{`MemberNotFound: "会员不存在"`, `NoMsg: ""`, "func Err(code int32) error", "func Errf("} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %q:\n%s", want, s)
		}
	}
}
