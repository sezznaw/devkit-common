// Package config loads a service's YAML configuration.
//
// Files live in a directory (conventionally ./conf) and are named after the
// environment. The team has four: local.yaml is a developer's own machine,
// dev.yaml the shared development server, uat.yaml and prod.yaml what their
// names say. The environment comes from APP_ENV; it defaults to "local",
// because a deployment always says where it is and a laptop never does. ${VAR} references inside the file are
// expanded from the process environment before parsing, so secrets can stay
// out of the repository.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	EnvVar = "APP_ENV"
	// LocalEnv is a developer's own machine, and what APP_ENV defaults to.
	LocalEnv   = "local"
	DefaultEnv = LocalEnv
)

// IsLocal reports whether the process runs on a developer's own machine.
func IsLocal() bool { return Env() == LocalEnv }

// Env returns the current environment name.
func Env() string {
	if v := os.Getenv(EnvVar); v != "" {
		return v
	}
	return DefaultEnv
}

// Load reads <dir>/<env>.yaml into out.
func Load(dir string, out any) error {
	return LoadFile(filepath.Join(dir, Env()+".yaml"), out)
}

// LoadFile reads a single YAML file into out, expanding ${VAR} references.
func LoadFile(path string, out any) error {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return fmt.Errorf("config: %s does not exist (%s=%q selects the file; available: %s)", path, EnvVar, Env(), available(filepath.Dir(path)))
	}
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	expanded := os.Expand(string(data), func(key string) string {
		return os.Getenv(key)
	})
	if err := yaml.Unmarshal([]byte(expanded), out); err != nil {
		return fmt.Errorf("config: parse %s: %w", path, err)
	}
	return nil
}

// DirEnv names the environment variable that points at the config directory.
const DirEnv = "CONF_DIR"

// Dir finds the directory holding the <env>.yaml files so that a service does
// not depend on the directory it happens to be started from:
//
//  1. $CONF_DIR, when set
//  2. ./conf                     (make run, go run, Docker WORKDIR)
//  3. <executable dir>/conf      (binary shipped next to its conf/)
//  4. <executable dir>/../conf   (bin/<service> started from anywhere)
func Dir() (string, error) {
	if v := os.Getenv(DirEnv); v != "" {
		if !isDir(v) {
			return "", fmt.Errorf("config: %s=%s is not a directory", DirEnv, v)
		}
		return v, nil
	}
	tried := []string{"conf"}
	if exe, err := os.Executable(); err == nil {
		if real, err := filepath.EvalSymlinks(exe); err == nil {
			exe = real
		}
		base := filepath.Dir(exe)
		tried = append(tried, filepath.Join(base, "conf"), filepath.Join(base, "..", "conf"))
	}
	for _, d := range tried {
		if isDir(d) {
			return d, nil
		}
	}
	wd, _ := os.Getwd()
	return "", fmt.Errorf("config: no conf directory found (working directory %s; looked in %v); start the service from its root directory or set %s", wd, tried, DirEnv)
}

// LoadDefault loads <Dir()>/<APP_ENV>.yaml into out.
func LoadDefault(out any) error {
	dir, err := Dir()
	if err != nil {
		return err
	}
	return Load(dir, out)
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// MustLoad is Load that panics. Prefer LoadDefault plus a plain error message
// in main(): a panic buries the actual problem under a stack trace.
func MustLoad(dir string, out any) {
	if err := Load(dir, out); err != nil {
		panic(err)
	}
}

// available lists the environments that do have a file, for error messages.
func available(dir string) string {
	matches, _ := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if len(matches) == 0 {
		return "none"
	}
	names := make([]string, 0, len(matches))
	for _, m := range matches {
		names = append(names, strings.TrimSuffix(filepath.Base(m), ".yaml"))
	}
	return strings.Join(names, ", ")
}
