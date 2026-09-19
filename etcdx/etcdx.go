// Package etcdx builds an etcd v3 client from config.
package etcdx

import (
	"fmt"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
)

type Config struct {
	Endpoints []string `yaml:"endpoints"`
	Username  string   `yaml:"username"`
	Password  string   `yaml:"password"`
	// DialTimeout in seconds, default 5.
	DialTimeout int `yaml:"dial_timeout"`
}

// New dials etcd; callers own Close().
func New(c Config) (*clientv3.Client, error) {
	if len(c.Endpoints) == 0 {
		return nil, fmt.Errorf("etcdx: no endpoints configured")
	}
	timeout := time.Duration(c.DialTimeout) * time.Second
	if timeout == 0 {
		timeout = 5 * time.Second
	}
	return clientv3.New(clientv3.Config{
		Endpoints:   c.Endpoints,
		Username:    c.Username,
		Password:    c.Password,
		DialTimeout: timeout,
	})
}
