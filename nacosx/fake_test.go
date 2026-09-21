package nacosx

import (
	"errors"
	"sync"
	"sync/atomic"

	"github.com/nacos-group/nacos-sdk-go/v2/clients/config_client"
	"github.com/nacos-group/nacos-sdk-go/v2/clients/naming_client"
	"github.com/nacos-group/nacos-sdk-go/v2/model"
	"github.com/nacos-group/nacos-sdk-go/v2/vo"
)

// fakeConfig stands in for the SDK's configuration client. Like the SDK it
// knows one listener per configuration; unlike the SDK it calls it on the
// goroutine of the test, so that a test needs no waiting.
type fakeConfig struct {
	config_client.IConfigClient
	mu        sync.Mutex
	content   map[string]string
	listeners map[string]func(namespace, group, dataID, data string)
	getErr    error
	listenErr error
	gets      int
}

func (f *fakeConfig) GetConfig(p vo.ConfigParam) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets++
	return f.content[p.DataId], f.getErr
}

func (f *fakeConfig) ListenConfig(p vo.ConfigParam) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listenErr != nil {
		return f.listenErr
	}
	if f.listeners == nil {
		f.listeners = map[string]func(string, string, string, string){}
	}
	if _, twice := f.listeners[p.DataId]; twice {
		return errors.New("fake: a second SDK listener for " + p.DataId)
	}
	f.listeners[p.DataId] = p.OnChange
	return nil
}

func (f *fakeConfig) CloseClient() {}

// set stores content without telling anybody.
func (f *fakeConfig) set(dataID, content string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.content == nil {
		f.content = map[string]string{}
	}
	f.content[dataID] = content
}

// publish stores content and pushes it, as the server does.
func (f *fakeConfig) publish(dataID, content string) {
	f.set(dataID, content)
	f.push(dataID, content)
}

// push calls the listener with data, whatever the stored content is.
func (f *fakeConfig) push(dataID, data string) {
	f.mu.Lock()
	fn := f.listeners[dataID]
	f.mu.Unlock()
	if fn != nil {
		fn("", "", dataID, data)
	}
}

// fakeNaming stands in for the SDK's naming client. As in the SDK, Subscribe
// calls the callback before it returns.
type fakeNaming struct {
	naming_client.INamingClient
	healthy atomic.Bool

	mu           sync.Mutex
	instances    map[string][]model.Instance
	callbacks    map[string]func([]model.Instance, error)
	registered   []vo.RegisterInstanceParam
	deregistered []vo.DeregisterInstanceParam
}

func newFakeNaming() *fakeNaming {
	f := &fakeNaming{instances: map[string][]model.Instance{}, callbacks: map[string]func([]model.Instance, error){}}
	f.healthy.Store(true)
	return f
}

func (f *fakeNaming) ServerHealthy() bool { return f.healthy.Load() }
func (f *fakeNaming) CloseClient()        {}

func (f *fakeNaming) RegisterInstance(p vo.RegisterInstanceParam) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.registered = append(f.registered, p)
	return true, nil
}

func (f *fakeNaming) DeregisterInstance(p vo.DeregisterInstanceParam) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deregistered = append(f.deregistered, p)
	return true, nil
}

func (f *fakeNaming) SelectAllInstances(p vo.SelectAllInstancesParam) ([]model.Instance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.instances[p.ServiceName], nil
}

func (f *fakeNaming) Subscribe(p *vo.SubscribeParam) error {
	f.mu.Lock()
	f.callbacks[p.ServiceName] = p.SubscribeCallback
	now := f.instances[p.ServiceName]
	f.mu.Unlock()
	if len(now) > 0 {
		p.SubscribeCallback(now, nil)
	}
	return nil
}

// push replaces the instances of a service and tells the subscriber.
func (f *fakeNaming) push(service string, list ...model.Instance) {
	f.mu.Lock()
	f.instances[service] = list
	fn := f.callbacks[service]
	f.mu.Unlock()
	if fn != nil {
		fn(list, nil)
	}
}

func inst(ip string, healthy bool) model.Instance {
	return model.Instance{Ip: ip, Port: 8888, Weight: 10, Healthy: healthy, Enable: true}
}
