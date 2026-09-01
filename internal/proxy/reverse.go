package proxy

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"
)

// ──────────────────────────────────────────────────────────────────────────────
// Reverse Proxy — upstream database forwarding
// ──────────────────────────────────────────────────────────────────────────────

// statementPath is the engine coordinator endpoint that accepts queries. The
// handler intercepts POST requests to this path for Tier 1/2 evaluation.
const statementPath = "/v1/statement"

// newReverseProxy builds an httputil.ReverseProxy that forwards requests to
// the upstream database coordinator.
//
// It uses httputil.ReverseProxy as required by the architecture principles:
// no custom TCP pooling or socket handling. A non-zero FlushInterval streams
// large response bodies incrementally (periodic flushing) rather than buffering
// a single large page in memory, and the transport is configured with sane
// dial / response-header timeouts.
func newReverseProxy(upstreamURL string) (*httputil.ReverseProxy, error) {
	target, err := url.Parse(upstreamURL)
	if err != nil {
		return nil, fmt.Errorf("proxy: invalid upstream url %q: %w", upstreamURL, err)
	}
	if target.Scheme == "" || target.Host == "" {
		return nil, fmt.Errorf("proxy: upstream url %q must include a scheme and host", upstreamURL)
	}

	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			// Rewrite only the destination authority; keep the original path,
			// query, method, and (importantly) all client headers intact so
			// auth/session headers pass through 1:1.
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.Host = target.Host

			// Suppress the X-Forwarded-For header that httputil.ReverseProxy
			// would otherwise inject automatically. We are a transparent
			// passthrough (headers mirrored 1:1) and injecting a header the
			// client did not send can break strict upstreams such as Trino,
			// which rejects X-Forwarded-For unless it is configured to accept it.
			// Setting the slice to nil (rather than an empty string) tells
			// ReverseProxy to omit the header entirely.
			req.Header["X-Forwarded-For"] = nil
		},
		// Stream large bodies incrementally instead of buffering them whole.
		FlushInterval: 100 * time.Millisecond,
		Transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout:   10 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			ResponseHeaderTimeout: 30 * time.Second,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
		},
	}

	return proxy, nil
}
