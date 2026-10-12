package devroute

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/cloudwego/kitex/pkg/discovery"
	"github.com/cloudwego/kitex/pkg/loadbalance"

	"github.com/sezznaw/devkit-common/zlog"
)

// BalancerName is the Kitex load balancer name.
const BalancerName = "devroute"

// NewBalancer returns the load balancer of tagged routing. Instances with a
// dev-tag are grouped by tag; everything else is the baseline. A request
// whose context carries tags goes to an instance of the first tag that has
// one; every other request is picked among the baseline instances by the
// balancer Kitex uses by default (weighted random), never a takeover
// instance.
//
// It is a loadbalance.Rebalancer: Kitex's lbcache only tells a balancer that
// implements Rebalance/Delete about instance changes, and the default
// balancer caches its picker by CacheKey until told. Without this, a
// downstream whose Pods changed would keep being called at the old
// addresses until this process restarts.
func NewBalancer() loadbalance.Loadbalancer {
	return &balancer{base: loadbalance.NewWeightedBalancer()}
}

type balancer struct {
	base    loadbalance.Loadbalancer
	pickers sync.Map // CacheKey -> loadbalance.Picker
}

func (b *balancer) Name() string { return BalancerName }

// GetPicker: cached by CacheKey for a cacheable result (what the resolver
// returns), rebuilt by Rebalance; built on the spot otherwise.
func (b *balancer) GetPicker(result discovery.Result) loadbalance.Picker {
	if !result.Cacheable {
		return b.build(result)
	}
	if p, ok := b.pickers.Load(result.CacheKey); ok {
		return p.(loadbalance.Picker)
	}
	p := b.build(result)
	b.pickers.Store(result.CacheKey, p)
	return p
}

// Rebalance implements loadbalance.Rebalancer: the instances changed.
func (b *balancer) Rebalance(ch discovery.Change) {
	if !ch.Result.Cacheable {
		return
	}
	b.pickers.Store(ch.Result.CacheKey, b.build(ch.Result))
}

// Delete implements loadbalance.Rebalancer: the target is gone.
func (b *balancer) Delete(ch discovery.Change) {
	if !ch.Result.Cacheable {
		return
	}
	b.pickers.Delete(ch.Result.CacheKey)
	if rb, ok := b.base.(loadbalance.Rebalancer); ok {
		rb.Delete(ch)
	}
}

// build splits the instances and makes the picker. The baseline goes to the
// default balancer through its own Rebalance (so its cache follows ours).
func (b *balancer) build(result discovery.Result) loadbalance.Picker {
	tagged := map[string][]discovery.Instance{}
	baseline := make([]discovery.Instance, 0, len(result.Instances))
	for _, in := range result.Instances {
		if tag, ok := in.Tag(TagMetadata); ok && tag != "" {
			tagged[tag] = append(tagged[tag], in)
		} else {
			baseline = append(baseline, in)
		}
	}
	baseResult := discovery.Result{Cacheable: result.Cacheable, CacheKey: result.CacheKey, Instances: baseline}
	var base loadbalance.Picker
	if rb, ok := b.base.(loadbalance.Rebalancer); ok && result.Cacheable {
		rb.Rebalance(discovery.Change{Result: baseResult})
	}
	base = b.base.GetPicker(baseResult)
	if len(tagged) == 0 {
		return base
	}
	return &picker{base: base, tagged: tagged, hasBaseline: len(baseline) > 0}
}

type picker struct {
	base        loadbalance.Picker
	tagged      map[string][]discovery.Instance
	hasBaseline bool
	next        atomic.Uint64
}

// Next: rule 1, a tag of the request names an instance -> that instance
// (round robin among several with the same tag); rule 2, everything else
// is the baseline's business, and when the baseline is empty the answer is
// "no instance", not a takeover instance.
func (p *picker) Next(ctx context.Context, request interface{}) discovery.Instance {
	for _, tag := range Tags(ctx) {
		if list := p.tagged[tag]; len(list) > 0 {
			in := list[p.next.Add(1)%uint64(len(list))]
			zlog.Ctx(ctx).Debug("devroute: takeover instance picked", zlog.Str("dev_tag", tag), zlog.Str("addr", in.Address().String()))
			return in
		}
	}
	if !p.hasBaseline {
		return nil
	}
	return p.base.Next(ctx, request)
}
