// Command nacosx shows what the nacosx package does and what it logs.
//
//	go run ./examples/nacosx                          # without Nacos: configurations are files
//	NACOS_ADDR=127.0.0.1:8848 go run ./examples/nacosx  # against a real Nacos
//
// Against a real Nacos, create the configuration "example.yaml" in the public
// namespace first, with the content of the constant first below, then change
// it in the console while the program runs.
package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sezznaw/devkit-common/nacosx"
	"github.com/sezznaw/devkit-common/zlog"
)

// Dynamic is what may change while the program runs.
type Dynamic struct {
	Battle struct {
		MaxLevel  int  `yaml:"max_level"`
		DoubleExp bool `yaml:"double_exp"`
	} `yaml:"battle"`
	Pay struct {
		Secret string `yaml:"secret"`
	} `yaml:"pay"`
	Whitelist []string `yaml:"whitelist"`
}

func (d *Dynamic) Validate() error {
	if d.Battle.MaxLevel <= 0 {
		return errors.New("battle.max_level must be positive")
	}
	return nil
}

const first = `battle:
  max_level: 60
pay:
  secret: s3cr3t
whitelist: [neo, trinity]
legacy_flag: false
`

func main() {
	zlog.Init(zlog.Options{Level: "debug", Service: "example"})
	defer zlog.Sync()

	if addr := os.Getenv("NACOS_ADDR"); addr != "" {
		online(addr)
		return
	}
	offline()
}

func offline() {
	dir, _ := os.MkdirTemp("", "nacosx-example")
	defer os.RemoveAll(dir)
	file := filepath.Join(dir, "example.yaml")
	save := func(content string) {
		_ = os.WriteFile(file, []byte(content), 0o644)
		time.Sleep(1500 * time.Millisecond) // the file is looked at once a second
	}
	save(first)

	nc := nacosx.Offline(dir)
	defer nc.Close()
	dyn := watch(nc)

	save(strings.NewReplacer("60", "80", "legacy_flag: false\n", "", "s3cr3t", "an0ther", "max_level", "double_exp: true\n  max_level").Replace(first))
	zlog.Info("the program sees the new value", zlog.Int("max_level", dyn.Get().Battle.MaxLevel))

	save("battle:\n  max_level: [oops\n")
	save("battle:\n  max_level: -5\n")
	save("battle:\n  max_levle: 90\n  max_level: 85\n")
	zlog.Info("the program still runs on a good version", zlog.Int("max_level", dyn.Get().Battle.MaxLevel))
}

func online(addr string) {
	nc, err := nacosx.New(nacosx.Config{Addrs: []string{addr}})
	if err != nil {
		zlog.Fatal("cannot start", zlog.Err(err))
	}
	defer nc.Close()
	dyn := watch(nc)

	deregister, err := nc.Register(nacosx.Registration{Service: "example", Addr: ":8080"})
	if err != nil {
		zlog.Fatal("cannot register", zlog.Err(err))
	}
	for i := 0; i < 60; i++ {
		if in, err := nc.Pick("example"); err == nil {
			zlog.Debug("picked", zlog.Str("instance", in.Addr), zlog.Int("max_level", dyn.Get().Battle.MaxLevel))
		} else {
			zlog.Warn("no instance", zlog.Err(err))
		}
		time.Sleep(5 * time.Second)
	}
	_ = deregister()
}

func watch(nc *nacosx.Client) *nacosx.Value[Dynamic] {
	dyn, err := nacosx.Watch[Dynamic](nc, "example.yaml")
	if err != nil {
		zlog.Fatal("cannot start", zlog.Err(err))
	}
	dyn.OnChange(func(old, cur *Dynamic) {
		zlog.Info("hook: rebuilding what depends on the level", zlog.Int("from", old.Battle.MaxLevel), zlog.Int("to", cur.Battle.MaxLevel))
	})
	return dyn
}
