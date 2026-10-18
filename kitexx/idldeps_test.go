package kitexx

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sezznaw/devkit-common/nacosx"
	"github.com/sezznaw/devkit-common/zlog/zlogtest"
)

const sampleIDLDeps = `# Written by make gen. Do not edit.
idl ec5c537aa
self ser-admin-api 57
dep ser-admin 42
dep ser-member 61
`

func TestParseIDLDeps(t *testing.T) {
	d, err := ParseIDLDeps(sampleIDLDeps)
	if err != nil {
		t.Fatal(err)
	}
	if d.Commit != "ec5c537aa" || d.Self != "ser-admin-api" || d.SelfGen != 57 || d.Deps["ser-admin"] != 42 || d.Deps["ser-member"] != 61 || len(d.Deps) != 2 {
		t.Fatalf("parsed %+v", d)
	}
	m := d.Metadata()
	if m[MetaIDLGen] != "57" || m[MetaIDLDeps] != "ser-admin,ser-member" {
		t.Fatalf("metadata %v", m)
	}
	if got := RegistrationMetadata(nil); got != nil {
		t.Fatalf("no idl.deps set: no metadata, got %v", got)
	}

	// make gen's placeholder when idl.deps was not committed: no information, not an error.
	d, err = ParseIDLDeps("# idl.deps missing: run make gen on your machine and commit idl.deps\n")
	if err != nil || !d.Empty() || d.Metadata() != nil {
		t.Fatalf("placeholder: %+v %v", d, err)
	}
	for _, bad := range []string{"self a x\n", "dep a 1\n", "self a 1\nself b 2\n", "what 1\n", "self a -1\n"} {
		if _, err := ParseIDLDeps(bad); err == nil {
			t.Errorf("%q must not parse", bad)
		}
	}
}

func TestSetIDLDepsAndRegistrationMetadata(t *testing.T) {
	t.Cleanup(func() { idlDeps = IDLDeps{} })
	if err := SetIDLDeps(sampleIDLDeps); err != nil {
		t.Fatal(err)
	}
	m := RegistrationMetadata(map[string]string{"protocol": "http"})
	if m["protocol"] != "http" || m[MetaIDLGen] != "57" || m[MetaIDLDeps] != "ser-admin,ser-member" {
		t.Fatalf("metadata %v", m)
	}
	if err := SetIDLDeps("self a x\n"); err == nil || !strings.Contains(err.Error(), "generation") {
		t.Fatalf("a bad file is reported: %v", err)
	}
}

func instAt(addr string, meta map[string]string) nacosx.Instance {
	return nacosx.Instance{Addr: addr, Weight: 10, Healthy: true, Enabled: true, Metadata: meta}
}

func TestIDLDepsUnmet(t *testing.T) {
	d, _ := ParseIDLDeps(sampleIDLDeps)
	world := map[string][]nacosx.Instance{}
	var lookupErr error
	lookup := func(name string) ([]nacosx.Instance, error) {
		if lookupErr != nil {
			return nil, lookupErr
		}
		return world[name], nil
	}

	unmet, warnings := d.unmet(lookup)
	if len(unmet) != 2 || !strings.Contains(unmet[0], "ser-admin: no usable instance") || !strings.Contains(unmet[1], "ser-member: no usable instance") || len(warnings) != 0 {
		t.Fatalf("nothing registered: %v %v", unmet, warnings)
	}

	world["ser-admin"] = []nacosx.Instance{instAt("10.0.0.1:8100", map[string]string{MetaIDLGen: "41"}), instAt("10.0.0.2:8100", map[string]string{MetaIDLGen: "42"})}
	world["ser-member"] = []nacosx.Instance{instAt("10.0.0.3:8110", map[string]string{MetaIDLGen: "70"})}
	unmet, warnings = d.unmet(lookup)
	if len(unmet) != 1 || !strings.Contains(unmet[0], "ser-admin at 10.0.0.1:8100 is at IDL generation 41, this release needs 42") || len(warnings) != 0 {
		t.Fatalf("one old instance: %v %v", unmet, warnings)
	}

	// The old instance left: open.
	world["ser-admin"] = world["ser-admin"][1:]
	if unmet, warnings = d.unmet(lookup); len(unmet) != 0 || len(warnings) != 0 {
		t.Fatalf("all new: %v %v", unmet, warnings)
	}

	// An instance built before idl.deps cannot be judged: a warning, not a wait.
	world["ser-member"] = []nacosx.Instance{instAt("10.0.0.3:8110", nil)}
	if unmet, warnings = d.unmet(lookup); len(unmet) != 0 || len(warnings) != 1 || !strings.Contains(warnings[0], "registers no idl.gen") {
		t.Fatalf("no metadata: %v %v", unmet, warnings)
	}
	world["ser-member"] = []nacosx.Instance{instAt("10.0.0.3:8110", map[string]string{MetaIDLGen: "x"})}
	if unmet, warnings = d.unmet(lookup); len(unmet) != 0 || len(warnings) != 1 || !strings.Contains(warnings[0], "not a number") {
		t.Fatalf("bad metadata: %v %v", unmet, warnings)
	}

	// A dependency that depends on us too is not waited for, whatever its generation.
	world["ser-member"] = []nacosx.Instance{instAt("10.0.0.3:8110", map[string]string{MetaIDLGen: "1", MetaIDLDeps: "ser-vendor,ser-admin-api"})}
	if unmet, warnings = d.unmet(lookup); len(unmet) != 0 || len(warnings) != 1 || !strings.Contains(warnings[0], "depends on ser-admin-api too") {
		t.Fatalf("cycle: %v %v", unmet, warnings)
	}

	lookupErr = errors.New("nacos down")
	if unmet, _ = d.unmet(lookup); len(unmet) != 2 || !strings.Contains(unmet[0], "nacos down") {
		t.Fatalf("lookup error: %v", unmet)
	}
}

func TestWaitIDLDeps(t *testing.T) {
	logs := zlogtest.Capture(t)
	d, _ := ParseIDLDeps(sampleIDLDeps)
	calls := 0
	lookup := func(name string) ([]nacosx.Instance, error) {
		calls++
		gen := "41"
		if calls > 4 { // the dependency's release arrives on the third round
			gen = "42"
		}
		if name == "ser-member" {
			gen = "61"
		}
		return []nacosx.Instance{instAt(name+":1", map[string]string{MetaIDLGen: gen})}, nil
	}
	if err := waitIDLDeps(d, lookup, time.Second, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if !logs.Has("WARN", "deps gate waiting") || !logs.Has("INFO", "deps gate open") {
		t.Fatalf("the wait and the opening are logged: %s", logs)
	}

	never := func(name string) ([]nacosx.Instance, error) { return nil, nil }
	err := waitIDLDeps(d, never, 20*time.Millisecond, time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "still waiting for ser-admin: no usable instance") {
		t.Fatalf("times out with the reason: %v", err)
	}
}

func TestWaitIDLDepsSwitches(t *testing.T) {
	logs := zlogtest.Capture(t)
	t.Cleanup(func() { idlDeps = IDLDeps{} })
	var cfg Config
	cfg.Service.Name = "ser-admin-api"
	cfg.Nacos.Addrs = []string{"n1:8848"}

	// A developer's machine: off, whatever the binary carries.
	t.Setenv("APP_ENV", "local")
	if err := SetIDLDeps(sampleIDLDeps); err != nil {
		t.Fatal(err)
	}
	if err := WaitIDLDeps(cfg); err != nil || !logs.Has("INFO", "deps gate off") {
		t.Fatalf("local: %v %s", err, logs)
	}

	t.Setenv("APP_ENV", "dev")
	t.Setenv(DepsGateEnv, "off")
	if err := WaitIDLDeps(cfg); err != nil || !logs.Has("WARN", "deps gate off") {
		t.Fatalf("DEPS_GATE=off: %v %s", err, logs)
	}
	t.Setenv(DepsGateEnv, "")

	// No idl.deps in the binary: off with a warning that says what to do.
	idlDeps = IDLDeps{}
	if err := WaitIDLDeps(cfg); err != nil || !strings.Contains(logs.String(), "run make gen and commit idl.deps") {
		t.Fatalf("no idl.deps: %v %s", err, logs)
	}

	// A service that calls nobody has nothing to wait for and asks no Nacos.
	idlDeps, _ = ParseIDLDeps("self ser-vendor 9\n")
	if err := WaitIDLDeps(cfg); err != nil || !logs.Has("INFO", "deps gate open") {
		t.Fatalf("no deps: %v %s", err, logs)
	}

	// Without Nacos there is nothing to ask.
	idlDeps = d(t)
	cfg.RegistryDisabled = true
	if err := WaitIDLDeps(cfg); err != nil {
		t.Fatal(err)
	}
}

func d(t *testing.T) IDLDeps {
	d, err := ParseIDLDeps(sampleIDLDeps)
	if err != nil {
		t.Fatal(err)
	}
	return d
}
