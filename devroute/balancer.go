package devroute

import (
	"context"
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
// instance. With no takeover instance present the picker is the default
// balancer's own, so a service with routing on and nobody taking anything
// over behaves exactly as before.
func NewBalancer() loadbalance.Loadbalancer {
	return &balancer{base: loadbalance.NewWeightedBalancer()}
}

type balancer struct {
	base loadbalance.Loadbalancer
}

func (b *balancer) Name() string { return BalancerName }

func (b *balancer) GetPicker(result discovery.Result) loadbalance.Picker {
	tagged := map[string][]discovery.Instance{}
	baseline := make([]discovery.Instance, 0, len(result.Instances))
	for _, in := range result.Instances {
		if tag, ok := in.Tag(TagMetadata); ok && tag != "" {
			tagged[tag] = append(tagged[tag], in)
		} else {
			baseline = append(baseline, in)
		}
	}
	base := b.base.GetPicker(discovery.Result{Cacheable: result.Cacheable, CacheKey: result.CacheKey, Instances: baseline})
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
