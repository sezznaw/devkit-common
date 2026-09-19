// Package config loads a service's YAML configuration.
//
// Files live in a directory (conventionally ./conf) and are named after the
// environment: dev.yaml, test.yaml, prod.yaml. The environment comes from
// APP_ENV and defaults to "dev". ${VAR} references inside the file are
// expanded from the process environment before parsing, so secrets can stay
// out of the repository.
package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

const (
	EnvVar     = "APP_ENV"
	DefaultEnv = "dev"
)

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

// MustLoad is Load that panics; convenient in main().
func MustLoad(dir string, out any) {
	if err := Load(dir, out); err != nil {
		panic(err)
	}
}
