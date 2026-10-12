package devroute

import (
	"context"
	"reflect"
	"testing"

	"github.com/cloudwego/kitex/pkg/discovery"
	"github.com/cloudwego/kitex/pkg/loadbalance"
)

func inst(addr string, tag string) discovery.Instance {
	var tags map[string]string
	if tag != "" {
		tags = map[string]string{TagMetadata: tag}
	}
	return discovery.NewInstance("tcp", addr, 10, tags)
}

func TestParse(t *testing.T) {
	for _, c := range []struct {
		in   string
		want int
		ok   bool
	}{{"alice", 1, true}, {"alice,bob-2", 2, true}, {" alice , bob ", 2, true}, {"", 0, false}, {"Alice", 0, false}, {"a b", 0, false}, {"alice,", 0, false}, {"x" + string(make([]byte, 40)), 0, false}, {"a,b,c", 3, true}, {"a,b,c,d", 0, false}} {
		tags, ok := Parse(c.in)
		if ok != c.ok || len(tags) != c.want {
			t.Errorf("Parse(%q) = %v %v", c.in, tags, ok)
		}
	}
}

func TestContextRoundTrip(t *testing.T) {
	ctx := WithTags(context.Background(), []string{"alice", "bob"})
	if got := Tags(ctx); len(got) != 2 || got[0] != "alice" || got[1] != "bob" {
		t.Errorf("tags: %v", got)
	}
	if TagString(ctx) != "alice,bob" {
		t.Errorf("string: %q", TagString(ctx))
	}
	if Tags(context.Background()) != nil {
		t.Error("empty ctx has tags")
	}
}

func TestPicker(t *testing.T) {
	base1, base2 := inst("10.0.0.1:8100", ""), inst("10.0.0.2:8100", "")
	alice := inst("172.31.41.180:23001", "alice")
	bob := inst("172.31.41.180:23002", "bob")
	lb := NewBalancer()
	all := discovery.Result{Instances: []discovery.Instance{base1, alice, base2, bob}}
	p := lb.GetPicker(all)
	addr := func(in discovery.Instance) string {
		if in == nil {
			return "<nil>"
		}
		return in.Address().String()
	}
	plain := context.Background()
	for i := 0; i < 50; i++ {
		if a := addr(p.Next(plain, nil)); a != "10.0.0.1:8100" && a != "10.0.0.2:8100" {
			t.Fatalf("plain request reached %s", a)
		}
	}
	if a := addr(p.Next(WithTags(plain, []string{"alice"}), nil)); a != "172.31.41.180:23001" {
		t.Errorf("alice -> %s", a)
	}
	if a := addr(p.Next(WithTags(plain, []string{"carol", "bob"}), nil)); a != "172.31.41.180:23002" {
		t.Errorf("carol,bob -> %s (second tag must match)", a)
	}
	for i := 0; i < 20; i++ {
		if a := addr(p.Next(WithTags(plain, []string{"carol"}), nil)); a != "10.0.0.1:8100" && a != "10.0.0.2:8100" {
			t.Fatalf("unknown tag reached %s", a)
		}
	}
	// Baseline empty: a plain request gets no instance rather than a takeover one.
	onlyTakeover := lb.GetPicker(discovery.Result{Instances: []discovery.Instance{alice}})
	if in := onlyTakeover.Next(plain, nil); in != nil {
		t.Errorf("plain request with no baseline reached %s", addr(in))
	}
	if a := addr(onlyTakeover.Next(WithTags(plain, []string{"alice"}), nil)); a != "172.31.41.180:23001" {
		t.Errorf("alice with no baseline -> %s", a)
	}
}

// With no takeover instance the picker is the default balancer's own: the
// routing being on changes nothing for a service nobody takes over.
func TestPickerIsDefaultWithoutTakeover(t *testing.T) {
	res := discovery.Result{Instances: []discovery.Instance{inst("10.0.0.1:8100", ""), inst("10.0.0.2:8100", "")}}
	got := NewBalancer().GetPicker(res)
	want := loadbalance.NewWeightedBalancer().GetPicker(res)
	if _, isOurs := got.(*picker); isOurs {
		t.Fatal("picker wraps the default one although there is nothing to route")
	}
	gt, wt := typeName(got), typeName(want)
	if gt != wt {
		t.Errorf("picker type %s, default balancer gives %s", gt, wt)
	}
}

func typeName(v any) string { return reflect.TypeOf(v).String() }

// Kitex's lbcache tells the balancer about instance changes through
// Rebalance; the cached picker (Cacheable results) must follow them, for the
// baseline as much as for the takeover instances.
func TestRebalanceFollowsInstanceChanges(t *testing.T) {
	lb := NewBalancer()
	old := discovery.Result{Cacheable: true, CacheKey: "svc", Instances: []discovery.Instance{inst("10.0.0.1:8100", "")}}
	p := lb.GetPicker(old)
	if a := p.Next(context.Background(), nil).Address().String(); a != "10.0.0.1:8100" {
		t.Fatalf("first pick %s", a)
	}
	// the Pod moved and alice took the service over
	now := discovery.Result{Cacheable: true, CacheKey: "svc", Instances: []discovery.Instance{inst("10.0.0.2:8100", ""), inst("172.31.41.180:23001", "alice")}}
	lb.(loadbalance.Rebalancer).Rebalance(discovery.Change{Result: now, Added: now.Instances, Removed: old.Instances})
	p = lb.GetPicker(now)
	for i := 0; i < 20; i++ {
		if a := p.Next(context.Background(), nil).Address().String(); a != "10.0.0.2:8100" {
			t.Fatalf("after rebalance a plain request reached %s", a)
		}
	}
	if a := p.Next(WithTags(context.Background(), []string{"alice"}), nil).Address().String(); a != "172.31.41.180:23001" {
		t.Errorf("alice after rebalance -> %s", a)
	}
	// alice left: back to a plain default picker over the new baseline
	gone := discovery.Result{Cacheable: true, CacheKey: "svc", Instances: []discovery.Instance{inst("10.0.0.2:8100", "")}}
	lb.(loadbalance.Rebalancer).Rebalance(discovery.Change{Result: gone})
	if a := lb.GetPicker(gone).Next(WithTags(context.Background(), []string{"alice"}), nil).Address().String(); a != "10.0.0.2:8100" {
		t.Errorf("alice gone -> %s", a)
	}
	// Without Rebalance a cached picker must not change (that is lbcache's contract).
	stale := discovery.Result{Cacheable: true, CacheKey: "svc", Instances: []discovery.Instance{inst("10.0.0.9:8100", "")}}
	if a := lb.GetPicker(stale).Next(context.Background(), nil).Address().String(); a != "10.0.0.2:8100" {
		t.Errorf("GetPicker rebuilt a cached picker without Rebalance: %s", a)
	}
	lb.(loadbalance.Rebalancer).Delete(discovery.Change{Result: stale})
	if a := lb.GetPicker(stale).Next(context.Background(), nil).Address().String(); a != "10.0.0.9:8100" {
		t.Errorf("after Delete the picker is rebuilt from the result: %s", a)
	}
}

// Active follows the takeover instances a balancer has seen: a tag is active
// from the moment an instance with it arrives to the moment it leaves.
func TestActiveFollowsTakeoverInstances(t *testing.T) {
	b := NewBalancer()
	key := "ser-active-test"
	base := inst("10.42.0.1:8120", "")
	b.GetPicker(discovery.Result{Cacheable: true, CacheKey: key, Instances: []discovery.Instance{base}})
	if Active([]string{"zoe"}) {
		t.Fatal("zoe active before her instance registered")
	}
	b.(loadbalance.Rebalancer).Rebalance(discovery.Change{Result: discovery.Result{Cacheable: true, CacheKey: key, Instances: []discovery.Instance{base, inst("172.31.41.180:24001", "zoe")}}})
	if !Active([]string{"zoe"}) || Active([]string{"bob"}) || Active(nil) {
		t.Fatal("active set does not follow the instances")
	}
	b.(loadbalance.Rebalancer).Rebalance(discovery.Change{Result: discovery.Result{Cacheable: true, CacheKey: key, Instances: []discovery.Instance{base}}})
	if Active([]string{"zoe"}) {
		t.Fatal("zoe still active after her instance left")
	}
}
