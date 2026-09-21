package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadExpandsEnv(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "test.yaml"), []byte("addr: ${TEST_ADDR}\nport: 9090\n"), 0o644)
	t.Setenv("APP_ENV", "test")
	t.Setenv("TEST_ADDR", "127.0.0.1")
	var c struct {
		Addr string `yaml:"addr"`
		Port int    `yaml:"port"`
	}
	if err := Load(dir, &c); err != nil {
		t.Fatal(err)
	}
	if c.Addr != "127.0.0.1" || c.Port != 9090 {
		t.Fatalf("got %+v", c)
	}
}

func TestDurationYAML(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "dev.yaml"), []byte("a: 3s\nb: 500ms\nc: 2\nd: 1.5\n"), 0o644)
	t.Setenv("APP_ENV", "dev")
	var c struct{ A, B, C, D, E Duration }
	if err := Load(dir, &c); err != nil {
		t.Fatal(err)
	}
	if c.A.Std() != 3*time.Second || c.B.Std() != 500*time.Millisecond || c.C.Std() != 2*time.Second || c.D.Std() != 1500*time.Millisecond {
		t.Fatalf("parsed %v %v %v %v", c.A.Std(), c.B.Std(), c.C.Std(), c.D.Std())
	}
	if c.E.Or(7*time.Second) != 7*time.Second || c.A.Or(7*time.Second) != 3*time.Second {
		t.Fatal("Or() default handling wrong")
	}
	os.WriteFile(filepath.Join(dir, "dev.yaml"), []byte("a: soon\n"), 0o644)
	if err := Load(dir, &c); err == nil || !strings.Contains(err.Error(), "invalid duration") {
		t.Fatalf("expected a readable duration error, got %v", err)
	}
}

func TestDirResolutionAndMessages(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	t.Setenv(DirEnv, "")
	if _, err := Dir(); err == nil || !strings.Contains(err.Error(), "no conf directory found") || !strings.Contains(err.Error(), DirEnv) {
		t.Fatalf("expected a helpful not-found error, got %v", err)
	}
	os.MkdirAll(filepath.Join(root, "conf"), 0o755)
	if d, err := Dir(); err != nil || d != "conf" {
		t.Fatalf("Dir() = %q, %v; want ./conf", d, err)
	}
	other := t.TempDir()
	t.Setenv(DirEnv, other)
	if d, _ := Dir(); d != other {
		t.Fatalf("CONF_DIR must win, got %q", d)
	}
	t.Setenv(DirEnv, filepath.Join(other, "missing"))
	if _, err := Dir(); err == nil {
		t.Fatal("a CONF_DIR that is not a directory must be an error")
	}
	// Missing environment file names the file, the selector and what exists.
	t.Setenv(DirEnv, "")
	os.WriteFile(filepath.Join(root, "conf", "dev.yaml"), []byte("x: 1\n"), 0o644)
	t.Setenv("APP_ENV", "staging")
	var out struct{}
	err := LoadDefault(&out)
	if err == nil || !strings.Contains(err.Error(), "staging.yaml does not exist") || !strings.Contains(err.Error(), "available: dev") {
		t.Fatalf("unhelpful error: %v", err)
	}
}

// A laptop does not set APP_ENV, a deployment always does: no APP_ENV is local.
func TestDefaultEnvIsLocal(t *testing.T) {
	t.Setenv("APP_ENV", "")
	if Env() != "local" || !IsLocal() {
		t.Fatalf("Env() = %q, IsLocal() = %v", Env(), IsLocal())
	}
	t.Setenv("APP_ENV", "dev")
	if Env() != "dev" || IsLocal() {
		t.Fatalf("APP_ENV=dev: Env() = %q, IsLocal() = %v", Env(), IsLocal())
	}
}
