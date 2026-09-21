package nacosx

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sezznaw/devkit-common/zlog/zlogtest"
)

type dynamic struct {
	Battle struct {
		MaxLevel  int  `yaml:"max_level" json:"max_level"`
		DoubleExp bool `yaml:"double_exp" json:"double_exp"`
	} `yaml:"battle" json:"battle"`
	Pay struct {
		Secret string `yaml:"secret" json:"secret"`
	} `yaml:"pay" json:"pay"`
}

func (d *dynamic) Validate() error {
	if d.Battle.MaxLevel < 0 {
		return errors.New("battle.max_level must not be negative")
	}
	return nil
}

const v1 = "battle:\n  max_level: 60\npay:\n  secret: s3cr3t-one\n"

func watchV1(t *testing.T) (*Value[dynamic], *fakeConfig, *zlogtest.Logs) {
	t.Helper()
	logs := zlogtest.Capture(t)
	c, _, config := newTestClient(t)
	config.set("game.yaml", v1)
	v, err := Watch[dynamic](c, "game.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return v, config, logs
}

func TestWatchLoads(t *testing.T) {
	v, _, logs := watchV1(t)
	if v.Get().Battle.MaxLevel != 60 {
		t.Fatalf("got %+v", v.Get())
	}
	rec := logs.Records()[1] // after "nacos connected"
	values, _ := rec.Fields["values"].(map[string]any)
	if rec.Msg != "config loaded" || rec.Fields["data_id"] != "game.yaml" || values["battle.max_level"] != "60" {
		t.Fatalf("records: %s", logs)
	}
	if strings.Contains(logs.String(), "s3cr3t") || values["pay.secret"] != "***" {
		t.Errorf("a secret must not be in the log: %s", logs)
	}
}

func TestWatchFollowsChanges(t *testing.T) {
	v, config, logs := watchV1(t)
	var hookOld, hookCur int
	v.OnChange(func(old, cur *dynamic) { hookOld, hookCur = old.Battle.MaxLevel, cur.Battle.MaxLevel })
	before := v.Get()

	config.publish("game.yaml", "battle:\n  max_level: 80\n  double_exp: true\npay:\n  secret: s3cr3t-two\n")

	if v.Get().Battle.MaxLevel != 80 || !v.Get().Battle.DoubleExp {
		t.Fatalf("the change is not in effect: %+v", v.Get())
	}
	if before.Battle.MaxLevel != 60 {
		t.Error("a version that was handed out must never be written to")
	}
	if hookOld != 60 || hookCur != 80 {
		t.Errorf("hook got old=%d cur=%d", hookOld, hookCur)
	}
	var rec zlogtest.Record
	for _, r := range logs.Records() {
		if r.Msg == "config changed" {
			rec = r
		}
	}
	changes, _ := rec.Fields["changes"].([]any)
	if len(changes) != 3 {
		t.Fatalf("want 3 changed keys: %s", logs)
	}
	first, _ := changes[0].(map[string]any)
	if first["op"] != "added" || first["key"] != "battle.double_exp" || first["to"] != "true" {
		t.Errorf("first change: %v", first)
	}
	second, _ := changes[1].(map[string]any)
	if second["op"] != "changed" || second["from"] != "60" || second["to"] != "80" {
		t.Errorf("second change: %v", second)
	}
	if strings.Contains(logs.String(), "s3cr3t") {
		t.Errorf("a secret must not be in the log, changed or not: %s", logs)
	}
}

func TestWatchRejectsWhatItCannotUse(t *testing.T) {
	for name, bad := range map[string]string{
		"does not parse": "battle:\n  max_level: [oops\n",
		"wrong type":     "battle:\n  max_level: many\n",
		"not valid":      "battle:\n  max_level: -1\n",
	} {
		t.Run(name, func(t *testing.T) {
			v, config, logs := watchV1(t)
			called := false
			v.OnChange(func(_, _ *dynamic) { called = true })
			config.publish("game.yaml", bad)
			if v.Get().Battle.MaxLevel != 60 || called {
				t.Fatalf("the previous version must stay in effect: %+v", v.Get())
			}
			if !logs.Has("ERROR", "config change rejected") {
				t.Errorf("records: %s", logs)
			}
			// The next good version is taken, and compared with the last good one.
			config.publish("game.yaml", strings.Replace(v1, "60", "70", 1))
			if v.Get().Battle.MaxLevel != 70 || !strings.Contains(logs.String(), `"from":"60","to":"70"`) {
				t.Errorf("after a rejected version: %+v\n%s", v.Get(), logs)
			}
		})
	}
}

func TestWatchKeepsTheLastVersionWhenDeleted(t *testing.T) {
	v, config, logs := watchV1(t)
	config.publish("game.yaml", "")
	if v.Get().Battle.MaxLevel != 60 || !logs.Has("WARN", "config deleted or emptied") {
		t.Fatalf("got %+v\n%s", v.Get(), logs)
	}
}

func TestWatchReportsUnknownKeys(t *testing.T) {
	v, config, logs := watchV1(t)
	config.publish("game.yaml", "battle:\n  max_levle: 99\n")
	if v.Get().Battle.MaxLevel != 0 {
		t.Fatalf("the change is valid without the key nobody knows: %+v", v.Get())
	}
	if !logs.Has("WARN", "keys the program does not know") || !strings.Contains(logs.String(), "max_levle") {
		t.Errorf("a typo must be pointed out: %s", logs)
	}
}

// The SDK pushes on its own goroutines: a push may be late, or come twice.
func TestWatchTrustsTheServerNotThePush(t *testing.T) {
	v, config, logs := watchV1(t)
	count := 0
	v.OnChange(func(_, _ *dynamic) { count++ })

	config.push("game.yaml", v1) // what the SDK does right after ListenConfig
	if count != 0 {
		t.Fatal("the content of now is not a change")
	}
	config.set("game.yaml", strings.Replace(v1, "60", "80", 1))
	config.push("game.yaml", strings.Replace(v1, "60", "70", 1)) // overtaken by the push of 80
	config.push("game.yaml", strings.Replace(v1, "60", "80", 1))
	if v.Get().Battle.MaxLevel != 80 || count != 1 {
		t.Fatalf("level=%d after %d changes, want 80 after 1\n%s", v.Get().Battle.MaxLevel, count, logs)
	}
}

func TestWatchSurvivesAPanickingHook(t *testing.T) {
	v, config, logs := watchV1(t)
	second := false
	v.OnChange(func(_, _ *dynamic) { panic("boom") })
	v.OnChange(func(_, _ *dynamic) { second = true })
	config.publish("game.yaml", strings.Replace(v1, "60", "80", 1))
	if !second || !logs.Has("ERROR", "config change hook panicked") {
		t.Fatalf("second=%v\n%s", second, logs)
	}
}

func TestWatchFailsAtStartOnWhatItCannotUse(t *testing.T) {
	zlogtest.Discard(t)
	c, _, config := newTestClient(t)
	if _, err := Watch[dynamic](c, "missing.yaml"); err == nil || !strings.Contains(err.Error(), "does not exist") || !strings.Contains(err.Error(), "namespace dev") {
		t.Errorf("a missing configuration: %v", err)
	}
	config.set("bad.yaml", "battle: [")
	if _, err := Watch[dynamic](c, "bad.yaml"); err == nil {
		t.Error("a configuration that does not parse must stop the start")
	}
	config.set("invalid.yaml", "battle:\n  max_level: -1\n")
	if _, err := Watch[dynamic](c, "invalid.yaml"); err == nil || !strings.Contains(err.Error(), "must not be negative") {
		t.Errorf("an invalid configuration: %v", err)
	}
	config.getErr = errors.New("nacos down")
	if _, err := Watch[dynamic](c, "game.yaml"); err == nil {
		t.Error("an unreadable configuration must stop the start")
	}
}

func TestWatchJSON(t *testing.T) {
	zlogtest.Discard(t)
	c, _, config := newTestClient(t)
	config.set("game.json", `{"battle": {"max_level": 60}}`)
	v, err := Watch[dynamic](c, "game.json")
	if err != nil || v.Get().Battle.MaxLevel != 60 {
		t.Fatalf("%v %+v", err, v)
	}
}

func TestOnChange(t *testing.T) {
	logs := zlogtest.Capture(t)
	c, _, config := newTestClient(t)
	config.set("level", "info")
	var got []string
	if err := c.OnChange("level", func(s string) { got = append(got, s) }); err != nil {
		t.Fatal(err)
	}
	if err := c.OnChange("level", func(string) { panic("boom") }); err != nil {
		t.Fatalf("a second subscriber shares the one SDK listener: %v", err)
	}
	config.push("level", "info")
	config.publish("level", "debug")
	config.publish("level", "")
	if strings.Join(got, ",") != "debug," {
		t.Fatalf("got %q, want the change and the deletion, not the content of the start", got)
	}
	if !logs.Has("INFO", "config changed") || !logs.Has("WARN", "config deleted") || !logs.Has("ERROR", "config change handler panicked") {
		t.Errorf("records: %s", logs)
	}
	if s, err := c.Get("level"); err != nil || s != "" {
		t.Errorf("Get: %q %v", s, err)
	}
}

func TestOffline(t *testing.T) {
	logs := zlogtest.Capture(t)
	orig := filePollInterval
	filePollInterval = 2 * time.Millisecond
	defer func() { filePollInterval = orig }()

	dir := t.TempDir()
	c := Offline(dir)
	defer c.Close()
	if _, err := Watch[dynamic](c, "game.yaml"); err == nil || !strings.Contains(err.Error(), filepath.Join(dir, "game.yaml")) {
		t.Fatalf("a missing file must be named: %v", err)
	}
	file := filepath.Join(dir, "game.yaml")
	if err := os.WriteFile(file, []byte(v1), 0o644); err != nil {
		t.Fatal(err)
	}
	v, err := Watch[dynamic](c, "game.yaml")
	if err != nil || v.Get().Battle.MaxLevel != 60 {
		t.Fatalf("%v %+v", err, v)
	}
	// Saving the file is a change.
	if err := os.WriteFile(file, []byte(strings.Replace(v1, "60", "80", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return v.Get().Battle.MaxLevel == 80 && logs.Has("INFO", "config changed") })

	if _, err := c.Instances("ser-auth"); err == nil || !strings.Contains(err.Error(), "WithHostPorts") {
		t.Errorf("discovery without Nacos must say what to do instead: %v", err)
	}
	if dereg, err := c.Register(Registration{Service: "gateway", Addr: ":80"}); err != nil || dereg() != nil {
		t.Errorf("registering without Nacos does nothing: %v", err)
	}
}

func TestStatic(t *testing.T) {
	v := Static(&dynamic{})
	v.OnChange(func(_, _ *dynamic) {})
	if v.Get() == nil {
		t.Fatal("Static holds what it was given")
	}
}

func TestChangesOnTheConsole(t *testing.T) {
	c := diff(
		map[string]string{"battle.max_level": "60", "legacy": "false", "pay.secret": "a", "same": "1"},
		map[string]string{"battle.max_level": "80", "double_exp": "true", "pay.secret": "b", "same": "1"},
	)
	want := "~ battle.max_level  60 → 80\n" +
		"+ double_exp        true\n" +
		"- legacy            false\n" +
		"~ pay.secret        *** → ***\n"
	if got := c.LogBlock(false); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	if got := c.LogBlock(true); !strings.Contains(got, "\x1b[32m+ double_exp") {
		t.Errorf("colored: %q", got)
	}
}

func TestSecretKeys(t *testing.T) {
	for _, k := range []string{"password", "db.Password", "redis.pwd", "pay.app_secret", "jwt.signKey", "oss.access_key", "token", "wx.api-key", "key"} {
		if !isSecret(k) {
			t.Errorf("%q must be hidden", k)
		}
	}
	for _, k := range []string{"battle.max_level", "keyword", "hotkeys", "monkey.count"} {
		if isSecret(k) {
			t.Errorf("%q is not a secret", k)
		}
	}
	if mask("dbs", `[{"host":"a","password":"x"}]`) != masked {
		t.Error("a list with secrets inside is hidden as a whole")
	}
	if mask("ports", `[80,443]`) != `[80,443]` {
		t.Error("a harmless list is shown")
	}
}
