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
	}{{"alice", 1, true}, {"alice,bob-2", 2, true}, {" alice , bob ", 2, true}, {"", 0, false}, {"Alice", 0, false}, {"a b", 0, false}, {"alice,", 0, false}, {"x" + string(make([]byte, 40)), 0, false}} {
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
