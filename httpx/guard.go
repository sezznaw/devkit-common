package httpx

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"

	"github.com/sony/gobreaker/v2"

	"github.com/sezznaw/devkit-common/metricsx"
	"github.com/sezznaw/devkit-common/zlog"
)

// ErrCircuitOpen: the provider has been failing and the breaker is open;
// the call was not sent. It is fast: the alternative, with a provider that
// is down, is every request waiting out every attempt's timeout.
var ErrCircuitOpen = errors.New("httpx: circuit open, provider is failing")

// ErrTooManyInFlight: more calls to the provider are in progress than
// max_concurrent allows and the context ended before a slot came free.
var ErrTooManyInFlight = errors.New("httpx: too many calls in flight to provider")

// guard is the RoundTripper between resty and otelhttp: a semaphore for
// max_concurrent and a circuit breaker. Every attempt (retries included)
// passes through it, so both apply to raw R() calls as well.
type guard struct {
	name string
	next http.RoundTripper
	sem  chan struct{}
	cb   *gobreaker.CircuitBreaker[*http.Response]
}

func newGuard(name string, p Provider, next http.RoundTripper) *guard {
	g := &guard{name: name, next: next}
	if n := p.maxConcurrent(); n > 0 {
		g.sem = make(chan struct{}, n)
	}
	if f := p.breakerFailures(); f > 0 {
		g.cb = gobreaker.NewCircuitBreaker[*http.Response](gobreaker.Settings{
			Name:        name,
			MaxRequests: 1, // half-open: one probe
			Timeout:     p.BreakerOpenFor.Or(defaultBreakerOpenFor),
			ReadyToTrip: func(c gobreaker.Counts) bool { return c.ConsecutiveFailures >= uint32(f) },
			IsSuccessful: func(err error) bool {
				var fe *failedStatus
				return err == nil || !errors.As(err, &fe) && !isNetworkError(err)
			},
			OnStateChange: func(_ string, from, to gobreaker.State) {
				metricsx.HTTPClientBreakerState.WithLabelValues(name).Set(float64(to))
				rec := zlog.Warn
				if to == gobreaker.StateClosed {
					rec = zlog.Info
				}
				rec("provider circuit "+to.String(), zlog.Str("provider", name), zlog.Str("from", from.String()))
			},
		})
		metricsx.HTTPClientBreakerState.WithLabelValues(name).Set(0)
	}
	return g
}

// failedStatus carries a 5xx / 429 response through the breaker as a
// failure without losing the response: the caller still gets it.
type failedStatus struct{ resp *http.Response }

func (f *failedStatus) Error() string { return "status " + f.resp.Status }

func (g *guard) RoundTrip(req *http.Request) (*http.Response, error) {
	if g.sem != nil {
		select {
		case g.sem <- struct{}{}:
			defer func() { <-g.sem }()
		case <-req.Context().Done():
			metricsx.HTTPClientRejected.WithLabelValues(g.name, "in_flight").Inc()
			return nil, ErrTooManyInFlight
		}
	}
	if g.cb == nil {
		return g.next.RoundTrip(req)
	}
	resp, err := g.cb.Execute(func() (*http.Response, error) {
		resp, err := g.next.RoundTrip(req)
		if err == nil && (resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests) {
			return resp, &failedStatus{resp}
		}
		return resp, err
	})
	var fe *failedStatus
	switch {
	case errors.As(err, &fe):
		return fe.resp, nil // the status is the caller's to see
	case errors.Is(err, gobreaker.ErrOpenState), errors.Is(err, gobreaker.ErrTooManyRequests):
		metricsx.HTTPClientRejected.WithLabelValues(g.name, "circuit_open").Inc()
		return nil, ErrCircuitOpen
	}
	return resp, err
}

func isNetworkError(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) || errors.Is(err, context.DeadlineExceeded)
}

// newTransport is the pooled transport under every provider: the Go
// defaults keep 2 idle connections per host, which for a vendor polled
// many times a second means a new TLS handshake for most calls.
func newTransport(p Provider) *http.Transport {
	t := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          200,
		MaxIdleConnsPerHost:   100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	if p.Insecure {
		t.TLSClientConfig = insecureTLS()
	}
	return t
}
