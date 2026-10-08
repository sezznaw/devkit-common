package kitexx

// CORSConfig is the `cors:` section of an API service: which browser
// origins may call it. hertzx.New installs the middleware when enabled; it
// answers the preflight (OPTIONS) with POST, GET, OPTIONS, the Authorization,
// Content-Type and X-Request-Id headers and a 12 hour cache, and exposes
// x-trace-id to the page. Credentials (cookies) are never shared: the login
// is a bearer token in a header.
//
// CORSConfig 是 API 服务的 `cors:` 段：允许哪些浏览器来源跨域调用。默认什么都不允许；
// 来源要写全（协议、域名、端口），如 https://tester.example.com、http://localhost:5173。
type CORSConfig struct {
	Enabled        bool     `yaml:"enabled"`
	AllowedOrigins []string `yaml:"allowed_origins"`
}
