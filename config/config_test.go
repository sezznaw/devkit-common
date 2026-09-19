package config

import (
	"os"
	"path/filepath"
	"testing"
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
