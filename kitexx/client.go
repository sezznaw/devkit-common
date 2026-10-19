package kitexx

import (
	"context"
	"strings"

	"github.com/bytedance/gopkg/cloud/metainfo"
)

// The client that made the request (the browser or app behind the gateway)
// travels to the RPC services as persistent metainfo, like the identity and
// the trace id: its device type and device id, which only the client knows
// and reports in the request headers, and its IP, which only the gateway
// knows. A handler anywhere in the chain asks Device(ctx) / ClientIP(ctx)
// and no request body carries them (hertzx.Client is the gateway side).
//
// 发请求的客户端（网关后面的浏览器或 App）和 uid、trace_id 一样作为 metainfo 持久值跨每一跳：
// 设备类型和设备号只有客户端知道，由请求头报上来；IP 只有网关知道。链路上任何 handler 用
// Device(ctx) / ClientIP(ctx) 取，请求体里不再放这些字段（网关那边是 hertzx.Client）。
const (
	DeviceKey   = "DEVICE"
	DeviceIDKey = "DEVICE_ID"
	ClientIPKey = "CLIENT_IP"
)

// Client is what the gateway learned about the caller of a request.
type Client struct {
	Device   string // device type, one of client.device.types ("PC", "iOS", ...); "" unknown
	DeviceID string // the client's own device identifier; "" when not sent
	IP       string // the client IP the gateway saw; "" outside a request
}

// WithClient puts c into ctx for this process and for every RPC made with
// ctx. Empty fields are not stored. The gateway's Client middleware calls
// it; tests of RPC handlers call it to play a request from a device.
func WithClient(ctx context.Context, c Client) context.Context {
	if c.Device != "" {
		ctx = metainfo.WithPersistentValue(ctx, DeviceKey, c.Device)
	}
	if c.DeviceID != "" {
		ctx = metainfo.WithPersistentValue(ctx, DeviceIDKey, c.DeviceID)
	}
	if c.IP != "" {
		ctx = metainfo.WithPersistentValue(ctx, ClientIPKey, c.IP)
	}
	return ctx
}

// ClientOf returns everything known about the request's client.
func ClientOf(ctx context.Context) Client {
	return Client{Device: Device(ctx), DeviceID: DeviceID(ctx), IP: ClientIP(ctx)}
}

// Device is the device type of the request's client ("PC", "H5", "iOS", ...
// as client.device.types of the gateway spells them), "" when unknown.
func Device(ctx context.Context) string {
	v, _ := metainfo.GetPersistentValue(ctx, DeviceKey)
	return v
}

// DeviceID is the device identifier the client sent, "" when it sent none.
func DeviceID(ctx context.Context) string {
	v, _ := metainfo.GetPersistentValue(ctx, DeviceIDKey)
	return v
}

// ClientIP is the IP the gateway saw the request come from, "" when the
// context did not start at a gateway (a job, a consumer, a test).
func ClientIP(ctx context.Context) string {
	v, _ := metainfo.GetPersistentValue(ctx, ClientIPKey)
	return v
}

// ClientConfig is the `client:` section of an API service: what it learns
// about the caller from the request headers and passes on to the RPC
// services. The IP is always taken (from the connection, or the proxy's
// X-Forwarded-For); the device type only when client.device.types is set.
type ClientConfig struct {
	Device DeviceConfig `yaml:"device"`
}

// DeviceConfig is `client.device`: the device types the API service
// accepts in X-Device-Type and whether every request must send one.
type DeviceConfig struct {
	// Required refuses a request without a valid X-Device-Type with code
	// 1001. Off, the header is read when present and ignored when absent.
	Required bool `yaml:"required"`
	// Types lists the accepted values, as they are stored and shown:
	// ["PC", "H5", "Android", "iOS", "Telegram"]. Matching ignores case
	// and the stored value is the spelling listed here. Empty: the header
	// is not read at all.
	Types []string `yaml:"types"`
}

// Enabled says whether X-Device-Type is read: when types are listed.
func (d DeviceConfig) Enabled() bool { return len(d.Types) > 0 }

// Canonical returns the listed spelling of a header value ("ios" ->
// "iOS") and whether the value is one of the types.
func (d DeviceConfig) Canonical(v string) (string, bool) {
	v = strings.TrimSpace(v)
	for _, t := range d.Types {
		if strings.EqualFold(t, v) {
			return t, true
		}
	}
	return "", false
}
