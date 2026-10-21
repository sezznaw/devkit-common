package hertzx

import (
	"net"
	"strings"

	"github.com/cloudwego/hertz/pkg/app"
)

// ClientIP returns the function that finds the client's address, for
// engine.SetClientIPFunc (hertzx.New and the callback listener install it):
// the connection's address when it is not a trusted proxy; otherwise the
// rightmost address of X-Forwarded-For that is not a trusted proxy, since
// each proxy appends the address it saw and only the entries added by our
// own proxies can be believed. A client that sends X-Forwarded-For itself
// only adds entries to the left of the real one. X-Real-IP is not read: a
// trusted CDN passes on whatever the client put there.
//
// Hertz's default trusts every address and takes the leftmost entry, which
// the client chooses: through Cloudflare a request with
// "X-Forwarded-For: <office IP>" passed for the office (found 2026-10-21).
//
// ClientIP 取客户端地址：连接方不是可信代理就用连接地址；是可信代理就从 X-Forwarded-For 右往左
// 取第一个不是可信代理的地址（每层代理在右边追加自己看到的地址，只有我们自己的代理加的才可信，
// 客户端自己带的只会在左边）。不读 X-Real-IP（可信的 CDN 会把客户端填的原样带过来）。hertz 默认
// 信任一切并取最左边的值，经 Cloudflare 填办公室 IP 就能冒充（2026-10-21 实测）。
func ClientIP(trusted []*net.IPNet) app.ClientIP {
	isTrusted := func(ip net.IP) bool {
		// 0.0.0.0 / :: is no network peer: an in-process request (a test,
		// testx.From) has no connection; a real TCP peer never has it.
		if ip.IsUnspecified() {
			return true
		}
		for _, n := range trusted {
			if n.Contains(ip) {
				return true
			}
		}
		return false
	}
	return func(c *app.RequestContext) string {
		addr := c.RemoteAddr()
		if addr == nil {
			return ""
		}
		peer, _, err := net.SplitHostPort(strings.TrimSpace(addr.String()))
		if err != nil {
			peer = strings.TrimSpace(addr.String())
		}
		peerIP := net.ParseIP(peer)
		if peerIP == nil || !isTrusted(peerIP) {
			return peer
		}
		client := peer
		var entries []string
		for _, h := range c.Request.Header.PeekAll("X-Forwarded-For") {
			entries = append(entries, strings.Split(string(h), ",")...)
		}
		for i := len(entries) - 1; i >= 0; i-- {
			ip := net.ParseIP(strings.TrimSpace(entries[i]))
			if ip == nil {
				break // a malformed entry: nothing to its left can be believed
			}
			client = ip.String()
			if !isTrusted(ip) {
				break
			}
		}
		return client
	}
}
