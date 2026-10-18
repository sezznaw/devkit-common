package kitexx

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sezznaw/devkit-common/config"
	"github.com/sezznaw/devkit-common/nacosx"
	"github.com/sezznaw/devkit-common/zlog"
)

// IDL generations and the dependency gate.
//
// `make gen` writes idl.deps next to idl.lock: the IDL generation of this
// service and of every RPC service it calls, where a service's generation is
// the number of commits of the IDL repository that touched its directory up
// to the commit the code was generated from (monotonic along main, so newer
// is bigger). main.go embeds the copy in cmd/<service>/ and hands it to
// SetIDLDeps. Two things use it:
//
//   - the instance registers its own generation in Nacos (metadata idl.gen,
//     plus idl.deps: the names of the services it calls);
//   - before it listens, the service waits until every service it calls has
//     at least one usable instance and all of them register a generation at
//     least as new as the one its clients were generated from (WaitIDLDeps).
//
// So releases need no order. A gateway built against a new IDL waits, not
// ready and taking no traffic (the deployment's startupProbe), until the
// service it needs is there, and its post-deploy verification runs after
// that; a service that never arrives leaves the old pods serving and the
// rollout stuck, which the deployment reports. The verify Job has the same
// check (idl-deps).
//
// IDL 代数与依赖闸门。make gen 在 idl.lock 旁写 idl.deps：本服务和它调用的每个 RPC 服务的
// IDL 代数（IDL 仓库里改过该服务目录的提交数，单调递增）。实例把自己的代数注册到 Nacos 元数据
// idl.gen；启动监听前先等每个依赖至少有一个可用实例、且全部实例的代数 ≥ 生成客户端时的代数。
// 发布因此无需排顺序：先发的网关等后发的服务，等到了才就绪、才跑验证。

// IDLDeps is the content of idl.deps.
type IDLDeps struct {
	// Commit is the IDL commit the code was generated from (idl.lock).
	Commit string
	// Self is this service's IDL directory and SelfGen its generation.
	Self    string
	SelfGen int
	// Deps maps each RPC service this one calls to the generation its
	// client was generated from.
	Deps map[string]int
}

// Empty reports whether there is no generation information (idl.deps was
// never written: the service must run make gen and commit the file).
func (d IDLDeps) Empty() bool { return d.Self == "" }

// ParseIDLDeps parses idl.deps: `#` comments, then lines `idl <commit>`,
// `self <service> <generation>` and `dep <service> <generation>`.
func ParseIDLDeps(text string) (IDLDeps, error) {
	d := IDLDeps{Deps: map[string]int{}}
	sc := bufio.NewScanner(strings.NewReader(text))
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		switch {
		case f[0] == "idl" && len(f) == 2:
			d.Commit = f[1]
		case (f[0] == "self" || f[0] == "dep") && len(f) == 3:
			gen, err := strconv.Atoi(f[2])
			if err != nil || gen < 0 {
				return IDLDeps{}, fmt.Errorf("idl.deps line %d: %q is not a generation (a count of IDL commits): %s", n, f[2], line)
			}
			if f[0] == "self" {
				if d.Self != "" {
					return IDLDeps{}, fmt.Errorf("idl.deps line %d: a second self line: %s", n, line)
				}
				d.Self, d.SelfGen = f[1], gen
			} else {
				d.Deps[f[1]] = gen
			}
		default:
			return IDLDeps{}, fmt.Errorf("idl.deps line %d: expected `idl <commit>`, `self <service> <gen>` or `dep <service> <gen>`: %s", n, line)
		}
	}
	if d.Self == "" && len(d.Deps) > 0 {
		return IDLDeps{}, errors.New("idl.deps: dep lines without a self line")
	}
	return d, nil
}

// Metadata keys an instance registers in Nacos.
const (
	// MetaIDLGen is the instance's own IDL generation.
	MetaIDLGen = "idl.gen"
	// MetaIDLDeps lists, comma-separated, the services the instance calls.
	MetaIDLDeps = "idl.deps"
)

// Metadata is what the instance registers: idl.gen and idl.deps. Nothing
// for an empty IDLDeps.
func (d IDLDeps) Metadata() map[string]string {
	if d.Empty() {
		return nil
	}
	m := map[string]string{MetaIDLGen: strconv.Itoa(d.SelfGen)}
	if names := d.depNames(); len(names) > 0 {
		m[MetaIDLDeps] = strings.Join(names, ",")
	}
	return m
}

func (d IDLDeps) depNames() []string {
	names := make([]string, 0, len(d.Deps))
	for name := range d.Deps {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// idlDeps is the process's idl.deps, set once at start by main.go.
var idlDeps IDLDeps

// SetIDLDeps hands the framework the embedded idl.deps (cmd/<service>/idl.deps,
// a copy of the service's idl.deps made by make gen). A file with comments
// only (make gen's placeholder when idl.deps was not committed) means no
// information: the gate stays off with a warning, and `devkit lint` (rule
// idl-deps) says to run make gen and commit the file.
func SetIDLDeps(text string) error {
	d, err := ParseIDLDeps(text)
	if err != nil {
		return fmt.Errorf("kitexx: %w", err)
	}
	idlDeps = d
	return nil
}

// RegistrationMetadata adds the IDL generation keys to the metadata an
// instance registers with; base may be nil.
func RegistrationMetadata(base map[string]string) map[string]string {
	extra := idlDeps.Metadata()
	if len(extra) == 0 {
		return base
	}
	out := make(map[string]string, len(base)+len(extra))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

// DepsGateEnv is the environment variable that switches the gate off
// ("off"), an escape hatch for the deployment when a dependency is known to
// stay old on purpose. The gate is also off on a developer's machine
// (APP_ENV local) and without Nacos (registry_disabled).
const DepsGateEnv = "DEPS_GATE"

var (
	depsGateTimeout = 10 * time.Minute
	depsGatePoll    = 2 * time.Second
	depsGateLogEach = 30 * time.Second
)

// lookupFunc is nacosx.Client.Lookup: the usable instances of a service,
// possibly none.
type lookupFunc func(service string) ([]nacosx.Instance, error)

// unmet evaluates the gate once: which dependencies are not there yet, each
// with the reason, and the warnings about instances that could not be
// judged (built before idl.deps, or depending on this service too).
func (d IDLDeps) unmet(lookup lookupFunc) (unmet, warnings []string) {
	for _, name := range d.depNames() {
		need := d.Deps[name]
		list, err := lookup(name)
		if err != nil {
			unmet = append(unmet, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		if len(list) == 0 {
			unmet = append(unmet, fmt.Sprintf("%s: no usable instance registered", name))
			continue
		}
		for _, in := range list {
			if theirs := in.Metadata[MetaIDLDeps]; theirs != "" && d.Self != "" && containsName(theirs, d.Self) {
				// Two services that wait for each other would wait forever;
				// the one that comes second is the one that waits.
				warnings = append(warnings, fmt.Sprintf("%s depends on %s too: not waited for (a cycle would wait forever)", name, d.Self))
				break
			}
			raw, ok := in.Metadata[MetaIDLGen]
			if !ok {
				warnings = append(warnings, fmt.Sprintf("%s at %s registers no %s (built before idl.deps): not waited for", name, in.Addr, MetaIDLGen))
				continue
			}
			gen, err := strconv.Atoi(raw)
			if err != nil {
				warnings = append(warnings, fmt.Sprintf("%s at %s registers %s=%q, not a number: not waited for", name, in.Addr, MetaIDLGen, raw))
				continue
			}
			if gen < need {
				unmet = append(unmet, fmt.Sprintf("%s at %s is at IDL generation %d, this release needs %d", name, in.Addr, gen, need))
			}
		}
	}
	return unmet, warnings
}

func containsName(list, name string) bool {
	for _, n := range strings.Split(list, ",") {
		if strings.TrimSpace(n) == name {
			return true
		}
	}
	return false
}

// WaitIDLDeps is the dependency gate: it returns when every RPC service in
// idl.deps has usable instances at a generation at least as new as this
// release was built against, and with an error when that is not the case
// within depsGateTimeout (the process then exits and the deployment restarts
// it, which keeps the rollout waiting and the old pods serving). It returns
// at once, saying why, on a developer's machine, without Nacos, with
// DEPS_GATE=off, and when the binary carries no idl.deps.
func WaitIDLDeps(cfg Config) error {
	d := idlDeps
	switch {
	case cfg.RegistryDisabled:
		return nil // no Nacos, nothing to ask
	case config.IsLocal():
		zlog.Info("deps gate off", zlog.Str("why", "a developer's machine"))
		return nil
	case strings.EqualFold(os.Getenv(DepsGateEnv), "off"):
		zlog.Warn("deps gate off", zlog.Str("why", DepsGateEnv+"=off"))
		return nil
	case d.Empty():
		zlog.Warn("deps gate off", zlog.Str("why", "the binary carries no idl.deps: run make gen and commit idl.deps with the service"))
		return nil
	case len(d.Deps) == 0:
		zlog.Info("deps gate open", zlog.Str("why", "this service calls no RPC service"), zlog.Int("idl_gen", int64(d.SelfGen)))
		return nil
	}
	cli, err := Nacos(cfg)
	if err != nil {
		return err
	}
	return waitIDLDeps(d, cli.Lookup, depsGateTimeout, depsGatePoll)
}

func waitIDLDeps(d IDLDeps, lookup lookupFunc, timeout, poll time.Duration) error {
	start := time.Now()
	deadline := start.Add(timeout)
	var lastLog time.Time
	for attempt := 1; ; attempt++ {
		unmet, warnings := d.unmet(lookup)
		if len(unmet) == 0 {
			fields := []zlog.Field{zlog.Int("idl_gen", int64(d.SelfGen)), zlog.Int("deps", int64(len(d.Deps))), zlog.Dur("waited", time.Since(start).Round(time.Millisecond))}
			if len(warnings) > 0 {
				fields = append(fields, zlog.Any("not_judged", warnings))
			}
			zlog.Info("deps gate open", fields...)
			return nil
		}
		if attempt == 1 || time.Since(lastLog) >= depsGateLogEach {
			lastLog = time.Now()
			fields := []zlog.Field{zlog.Any("waiting_for", unmet), zlog.Dur("waited", time.Since(start).Round(time.Second)), zlog.Dur("gives_up_after", timeout)}
			if len(warnings) > 0 {
				fields = append(fields, zlog.Any("not_judged", warnings))
			}
			zlog.Warn("deps gate waiting: the services this release calls are not there yet (releasing them opens the gate)", fields...)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("kitexx: deps gate: after %s still waiting for %s", timeout, strings.Join(unmet, "; "))
		}
		time.Sleep(poll)
	}
}
