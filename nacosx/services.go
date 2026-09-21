package nacosx

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/nacos-group/nacos-sdk-go/v2/clients"
	"github.com/nacos-group/nacos-sdk-go/v2/clients/naming_client"
	"github.com/nacos-group/nacos-sdk-go/v2/model"
	"github.com/nacos-group/nacos-sdk-go/v2/vo"

	"github.com/sezznaw/devkit-common/zlog"
)

// ServiceState is one service that has instances which can take requests.
type ServiceState struct {
	Name string `json:"name"`
	// Alive is the number of instances that can take requests.
	Alive int `json:"alive"`
	// Instances are their addresses, sorted.
	Instances []string `json:"instances"`
	// Unusable counts the instances Nacos knows that cannot take requests
	// (unhealthy, disabled, weight 0).
	Unusable int `json:"unusable,omitempty"`
}

// ServiceOverview is every service that is alive, sorted by name. On the
// console it is the table of ServiceReport.
type ServiceOverview []ServiceState

// Counts returns the number of services and of instances that are alive.
func (o ServiceOverview) Counts() (services, instances int) {
	for _, s := range o {
		instances += s.Alive
	}
	return len(o), instances
}

func (o ServiceOverview) LogBlock(colored bool) string {
	return ServiceReport{Alive: o}.LogBlock(colored)
}

// ServiceReport is what a record of WatchServices carries: what is alive and,
// for the service the record is about, the instances that came or went. In
// JSON it is {"service", "changed", "alive"}; on the console it is one table,
// with the changes marked in it:
//
//	┌──────────┬───┬──────────────────┐
//	│ SERVICE  │ N │ INSTANCES        │
//	├──────────┼───┼──────────────────┤
//	│ ser-auth │ 1 │ + 127.0.0.1:8889 │
//	│ ser-user │ 2 │   127.0.0.1:8888 │
//	│          │   │   127.0.0.1:8890 │
//	└──────────┴───┴──────────────────┘
//
// "+" came (green), "-" went (red; a service of which nothing is left is a row
// with N = 0), "~" is not what it was (yellow).
type ServiceReport struct {
	Service string          `json:"service,omitempty"`
	Changed InstanceChanges `json:"changed,omitempty"`
	Alive   ServiceOverview `json:"alive"`
	// This is "service@address" of the instances this process registered; the
	// table points them out.
	This map[string]bool `json:"-"`
}

const thisProcess = "  ← this process"

type reportLine struct {
	text  string
	paint func(any) fmt.Formatter
}

type reportRow struct {
	name  string
	n     int
	paint func(any) fmt.Formatter // of the name
	lines []reportLine
}

func (r ServiceReport) rows() []reportRow {
	marks := map[string]InstanceChange{}
	for _, ch := range r.Changed {
		marks[ch.Addr] = ch
	}
	var rows []reportRow
	seen := false
	for _, s := range r.Alive {
		row := reportRow{name: s.Name, n: s.Alive, paint: zlog.Cyan}
		mine := s.Name == r.Service
		seen = seen || mine
		added := 0
		for _, addr := range s.Instances {
			ch, changed := marks[addr]
			switch {
			case mine && changed && ch.Op == "added":
				added++
				row.lines = append(row.lines, reportLine{"+ " + addr, zlog.Green})
			case mine && changed:
				row.lines = append(row.lines, reportLine{"~ " + addr + "  " + ch.state() + " (was " + ch.Was + ")", zlog.Yellow})
			default:
				row.lines = append(row.lines, reportLine{"  " + addr, nil})
			}
			if r.This[s.Name+"@"+addr] {
				row.lines[len(row.lines)-1].text += thisProcess
			}
		}
		if mine && added == s.Alive {
			row.paint = zlog.Green // the whole service is new
		}
		if mine {
			row.lines = append(row.lines, r.gone()...)
		}
		if s.Unusable > 0 {
			row.lines = append(row.lines, reportLine{fmt.Sprintf("! %d more cannot take requests", s.Unusable), zlog.Yellow})
		}
		rows = append(rows, row)
	}
	if !seen && r.Service != "" && len(r.Changed) > 0 {
		// Nothing of the service is left: it still gets its row.
		rows = append(rows, reportRow{name: r.Service, paint: zlog.Red, lines: r.gone()})
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].name < rows[j].name })
	}
	return rows
}

func (r ServiceReport) gone() []reportLine {
	var out []reportLine
	for _, ch := range r.Changed {
		if ch.Op == "removed" {
			out = append(out, reportLine{"- " + ch.Addr, zlog.Red})
		}
	}
	return out
}

func (r ServiceReport) LogBlock(colored bool) string {
	rows := r.rows()
	if len(rows) == 0 {
		return "(no service is alive)\n"
	}
	const hName, hN, hInst = "SERVICE", "N", "INSTANCES"
	wName, wN, wInst := len(hName), len(hN), len(hInst)
	for _, row := range rows {
		wName, wN = max(wName, utf8.RuneCountInString(row.name)), max(wN, len(fmt.Sprint(row.n)))
		for _, l := range row.lines {
			wInst = max(wInst, utf8.RuneCountInString(l.text))
		}
	}
	paint := func(text string, p func(any) fmt.Formatter) string {
		if !colored || p == nil {
			return text
		}
		return fmt.Sprint(p(text))
	}
	frame := func(s string) string { return paint(s, zlog.Gray) }
	rule := func(left, mid, right string) string {
		return frame(left+strings.Repeat("─", wName+2)+mid+strings.Repeat("─", wN+2)+mid+strings.Repeat("─", wInst+2)+right) + "\n"
	}
	bar := frame("│")
	line := func(name, n, inst string) string {
		return bar + " " + name + " " + bar + " " + n + " " + bar + " " + inst + " " + bar + "\n"
	}
	pad := func(s string, w int) string { return s + strings.Repeat(" ", max(0, w-utf8.RuneCountInString(s))) }

	var b strings.Builder
	b.WriteString(rule("┌", "┬", "┐"))
	b.WriteString(line(frame(pad(hName, wName)), frame(pad(hN, wN)), frame(pad(hInst, wInst))))
	b.WriteString(rule("├", "┼", "┤"))
	for _, row := range rows {
		if len(row.lines) == 0 {
			row.lines = []reportLine{{}}
		}
		for i, l := range row.lines {
			name, n := pad("", wName), pad("", wN)
			if i == 0 {
				name = paint(pad(row.name, wName), row.paint)
				n = paint(fmt.Sprintf("%*d", wN, row.n), row.paint)
			}
			b.WriteString(line(name, n, paint(pad(l.text, wInst), l.paint)))
		}
	}
	b.WriteString(rule("└", "┴", "┘"))
	return b.String()
}

// serviceWatch follows every service of the namespace and group, for the
// record only: nothing is called through it.
type serviceWatch struct {
	naming naming_client.INamingClient
	group  string
	this   func() map[string]bool // "service@address" registered by this process

	mu         sync.Mutex
	subscribed map[string]bool
	known      map[string]map[string]Instance // service -> address -> instance
	ready      bool                           // the first overview is out
}

var newWatchNamingFunc = newWatchNamingClient // replaced in tests

// newWatchNamingClient is a naming client that believes Nacos when it says a
// service has no instance left.
//
// The client that calls are made through does not: the SDK keeps the last list
// when Nacos pushes an empty one, which protects callers from a Nacos that lost
// its data, and means that the departure of the LAST instance of a service is
// never pushed and SelectAllInstances keeps answering with it (verified against
// Nacos 2.4.3). For a record of what is alive that is the one event that must
// not be missed, so the watch has a client of its own and the protection of
// the calling path stays as it is.
func newWatchNamingClient(c Config) (naming_client.INamingClient, error) {
	p, err := c.params()
	if err != nil {
		return nil, err
	}
	p.ClientConfig.UpdateCacheWhenEmpty = true
	p.ClientConfig.CacheDir = filepath.Join(p.ClientConfig.CacheDir, "watch")
	return clients.NewNamingClient(p)
}

// WatchServices puts the state of all services on record, in every service
// that calls it: one record at start with everything that is alive, and then a
// record whenever a service comes ("service online"), goes ("service offline")
// or gains or loses an instance ("service instances changed"). Each record
// carries the instances that changed and, again, everything that is alive with
// its number of instances, so that the last record always says how things are.
//
// Instances of a known service are pushed by Nacos and show up within a
// second. A service name that was not there before is found by asking for the
// list, every `every`; that is the delay for the first instance of a new name.
//
// A second call, and a call on an offline client, do nothing. A problem is a
// warning and not an error: a service must not fail to start over its records.
func (c *Client) WatchServices(every time.Duration) {
	if c.IsOffline() || every <= 0 {
		return
	}
	c.mu.Lock()
	if c.closed || c.watchStarted {
		c.mu.Unlock()
		return
	}
	c.watchStarted = true
	c.mu.Unlock()

	naming, err := newWatchNamingFunc(c.cfg)
	if err != nil {
		zlog.Warn("the state of the other services will not be on record", zlog.Err(err))
		return
	}
	w := &serviceWatch{naming: naming, group: c.cfg.GroupName(), this: c.registeredHere, subscribed: map[string]bool{}, known: map[string]map[string]Instance{}}
	c.mu.Lock()
	c.watch = w
	c.mu.Unlock()

	if err := w.round(); err != nil {
		zlog.Warn("cannot list the services in Nacos; trying again", zlog.Err(err), zlog.Dur("every", every))
	}
	w.mu.Lock()
	w.ready = true
	overview := w.overview()
	w.mu.Unlock()
	services, instances := overview.Counts()
	zlog.Info("services alive", zlog.Int("services", services), zlog.Int("instances", instances),
		zlog.Str("group", w.group), zlog.Str("namespace", c.cfg.namespaceName()), zlog.Any("overview", ServiceReport{Alive: overview, This: w.this()}))

	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		failing := false
		for {
			select {
			case <-c.stop:
				naming.CloseClient()
				return
			case <-t.C:
				// One warning per outage: the connection has records of its own.
				err := w.round()
				if err != nil && !failing {
					zlog.Warn("cannot list the services in Nacos; trying again", zlog.Err(err), zlog.Dur("every", every))
				}
				failing = err != nil
			}
		}
	}()
}

// registeredHere is "service@address" of what this process registered.
func (c *Client) registeredHere() map[string]bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]bool, len(c.registered))
	for k := range c.registered {
		out[k] = true
	}
	return out
}

// watching says whether WatchServices reports the services. The records of the
// calling path ("service discovered", "instances changed") then repeat it and
// go to debug; the warning that nothing can take requests stays.
func (c *Client) watching() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.watch != nil
}

// round subscribes to the services that are new in the list of Nacos.
func (w *serviceWatch) round() error {
	const pageSize = 100
	for page := uint32(1); ; page++ {
		list, err := w.naming.GetAllServicesInfo(vo.GetAllServiceInfoParam{GroupName: w.group, PageNo: page, PageSize: pageSize})
		if err != nil {
			return err
		}
		for _, name := range list.Doms {
			w.mu.Lock()
			done := w.subscribed[name]
			w.subscribed[name] = true
			w.mu.Unlock()
			if done {
				continue
			}
			name := name
			err := w.naming.Subscribe(&vo.SubscribeParam{
				ServiceName: name,
				GroupName:   w.group,
				SubscribeCallback: func(instances []model.Instance, err error) {
					if err == nil {
						w.update(name, convert(instances))
					}
				},
			})
			if err != nil {
				w.mu.Lock()
				delete(w.subscribed, name) // the next round tries again
				w.mu.Unlock()
				return fmt.Errorf("subscribe to %s: %w", name, err)
			}
		}
		if len(list.Doms) < pageSize {
			return nil
		}
	}
}

// update takes the instances of one service as Nacos pushed them and writes
// the record when what is alive differs from before.
func (w *serviceWatch) update(name string, list []Instance) {
	w.mu.Lock()
	defer w.mu.Unlock()
	cur := make(map[string]Instance, len(list))
	for _, in := range list {
		cur[in.Addr] = in
	}
	old := w.known[name]
	w.known[name] = cur
	changes := instanceDiff(old, cur)
	if !w.ready || len(changes) == 0 {
		return // before the first overview everything is news, and it is in there
	}
	before, after := alive(old), alive(cur)
	overview := w.overview()
	services, instances := overview.Counts()
	fields := []zlog.Field{
		zlog.Str("name", name).Cyan(), zlog.Int("alive", after),
		zlog.Int("services", services), zlog.Int("instances", instances),
		zlog.Any("overview", ServiceReport{Service: name, Changed: changes, Alive: overview, This: w.this()}),
	}
	switch {
	case before == 0 && after > 0:
		fields[0] = zlog.Str("name", name).Green()
		zlog.Info("service online", fields...)
	case before > 0 && after == 0:
		fields[0] = zlog.Str("name", name).Red()
		zlog.Info("service offline", fields...)
	default:
		zlog.Info("service instances changed", fields...)
	}
}

func alive(instances map[string]Instance) int {
	n := 0
	for _, in := range instances {
		if in.usable() {
			n++
		}
	}
	return n
}

// overview is what is alive, sorted by name. The caller holds mu.
func (w *serviceWatch) overview() ServiceOverview {
	out := ServiceOverview{}
	for name, instances := range w.known {
		st := ServiceState{Name: name}
		for addr, in := range instances {
			if in.usable() {
				st.Instances = append(st.Instances, addr)
			} else {
				st.Unusable++
			}
		}
		if st.Alive = len(st.Instances); st.Alive == 0 {
			continue
		}
		sort.Strings(st.Instances)
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
