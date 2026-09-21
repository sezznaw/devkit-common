package kitexx

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"

	"github.com/cloudwego/kitex/pkg/discovery"
	"github.com/cloudwego/kitex/pkg/registry"
	"github.com/cloudwego/kitex/pkg/rpcinfo"

	"github.com/sezznaw/devkit-common/config"
	"github.com/sezznaw/devkit-common/nacosx"
)

var (
	offlineMu sync.Mutex
	offline   *nacosx.Client
)

// Nacos returns the Nacos client of the process, the one the service is
// registered and its downstreams are found through, for what the service wants
// from Nacos itself.
//
// With registry_disabled there is no Nacos. The client then is an offline one:
// configurations are the files of <conf dir>/nacos, named like their data id,
// so that a service that reads its configuration from Nacos still runs on a
// developer's machine, and follows the file as it would follow Nacos.
func Nacos(cfg Config) (*nacosx.Client, error) {
	if !cfg.RegistryDisabled {
		cli, err := nacosx.Shared(cfg.Nacos)
		if err != nil {
			return nil, fmt.Errorf("%w. To run without Nacos, for example on your own machine, set registry_disabled: true", err)
		}
		return cli, nil
	}
	offlineMu.Lock()
	defer offlineMu.Unlock()
	if offline == nil {
		dir, err := config.Dir()
		if err != nil {
			dir = "conf"
		}
		offline = nacosx.Offline(filepath.Join(dir, "nacos"))
	}
	return offline, nil
}

// WatchConfig reads the configuration of the service from Nacos into a T and
// keeps it current while the service runs; see nacosx.Watch for what that
// means and nacosx.Value for how it is used. The data id is config_data_id,
// by default "<service.name>.yaml".
//
//	dyn, err := kitexx.WatchConfig[Dynamic](cfg.Config)
//	...
//	dyn.Get().Battle.MaxLevel
func WatchConfig[T any](cfg Config) (*nacosx.Value[T], error) {
	nc, err := Nacos(cfg)
	if err != nil {
		return nil, err
	}
	dataID := cfg.ConfigDataID
	if dataID == "" {
		dataID = cfg.Service.Name + ".yaml"
	}
	return nacosx.Watch[T](nc, dataID)
}

// nacosRegistry registers a Kitex server through nacosx.
type nacosRegistry struct {
	cli *nacosx.Client
	// advertise, when set, is registered instead of the listener's address.
	advertise string

	mu         sync.Mutex
	deregister map[string]func() error
}

func newNacosRegistry(cli *nacosx.Client, advertise string) *nacosRegistry {
	return &nacosRegistry{cli: cli, advertise: advertise, deregister: map[string]func() error{}}
}

func registryKey(info *registry.Info) (string, error) {
	if info == nil || info.ServiceName == "" || info.Addr == nil {
		return "", fmt.Errorf("kitexx: registry info without service name or address: %+v", info)
	}
	return info.ServiceName + "@" + info.Addr.String(), nil
}

func (r *nacosRegistry) Register(info *registry.Info) error {
	key, err := registryKey(info)
	if err != nil {
		return err
	}
	addr := info.Addr.String()
	if r.advertise != "" {
		addr = r.advertise
	}
	dereg, err := r.cli.Register(nacosx.Registration{
		Service:  info.ServiceName,
		Addr:     addr,
		Weight:   float64(info.Weight),
		Metadata: info.Tags,
	})
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.deregister[key] = dereg
	r.mu.Unlock()
	return nil
}

func (r *nacosRegistry) Deregister(info *registry.Info) error {
	key, err := registryKey(info)
	if err != nil {
		return err
	}
	r.mu.Lock()
	dereg := r.deregister[key]
	delete(r.deregister, key)
	r.mu.Unlock()
	if dereg == nil {
		return nil
	}
	return dereg()
}

// nacosResolver finds the instances of the services a Kitex client calls.
// Kitex asks again every few seconds; nacosx answers from memory, which Nacos
// keeps current by pushing, and puts every change of a list on record.
type nacosResolver struct {
	cli  *nacosx.Client
	name string
}

func newNacosResolver(cli *nacosx.Client, c nacosx.Config) *nacosResolver {
	// Kitex shares the cache of instance lists between resolvers of one name.
	return &nacosResolver{cli: cli, name: fmt.Sprintf("nacosx:%v:%s:%s", c.Addrs, c.Namespace, c.GroupName())}
}

func (r *nacosResolver) Target(_ context.Context, target rpcinfo.EndpointInfo) string {
	return target.ServiceName()
}

func (r *nacosResolver) Resolve(_ context.Context, desc string) (discovery.Result, error) {
	list, err := r.cli.Instances(desc)
	if err != nil {
		return discovery.Result{}, err
	}
	instances := make([]discovery.Instance, 0, len(list))
	for _, in := range list {
		instances = append(instances, discovery.NewInstance("tcp", in.Addr, int(in.Weight), in.Metadata))
	}
	return discovery.Result{Cacheable: true, CacheKey: desc, Instances: instances}, nil
}

func (r *nacosResolver) Diff(cacheKey string, prev, next discovery.Result) (discovery.Change, bool) {
	return discovery.DefaultDiff(cacheKey, prev, next)
}

func (r *nacosResolver) Name() string { return r.name }
