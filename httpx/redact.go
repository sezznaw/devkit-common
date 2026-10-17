package httpx

import "regexp"

// Query parameter values are where vendors put API keys (and callers put
// phone numbers, order ids, ...). net/http quotes the full URL in its
// errors ("Get \"https://host/p?key=abc\": timeout"), resty repeats those
// errors through its logger, and a caller logs the error it got back: three
// ways for a secret to reach the log store. Everything httpx writes or
// returns passes through here first; the parameter names stay, the values
// become ***.
//
// 查询参数的值是厂商放 API key 的地方（调用方还会放手机号、订单号）。net/http 的错误带完整 URL，
// resty 通过它的 logger 再打一遍，调用方拿到错误又记一次：密钥有三条路进日志库。httpx 写日志和
// 返回错误前都过一遍这里，参数名保留，值换成 ***。
var queryValue = regexp.MustCompile(`([?&][^=&\s"'<>]+)=([^&\s"'<>]*)`)

// redactQuery masks the value of every query parameter in s.
func redactQuery(s string) string { return queryValue.ReplaceAllString(s, "$1=***") }

// redactedError is what Client.redactErr returns when a message had to be
// masked: the clean text, with the cause reachable through Unwrap so
// errors.Is / errors.As (net.Error, *StatusError, ...) keep working.
type redactedError struct {
	msg   string
	cause error
}

func (e *redactedError) Error() string { return e.msg }
func (e *redactedError) Unwrap() error { return e.cause }
