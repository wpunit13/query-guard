package proxy

import (
	"context"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// ──────────────────────────────────────────────────────────────────────────────
// Reverse Proxy — upstream database forwarding
// ──────────────────────────────────────────────────────────────────────────────

// statementPath is the engine coordinator endpoint that accepts queries. The
// handler intercepts POST requests to this path for Tier 1/2 evaluation.
const statementPath = "/v1/statement"

// clientHostKey is the context key under which the original client-facing Host
// is stored before the Director rewrites req.Host to the upstream target.
type clientHostKey struct{}

// rewritePrefixBytes is the max number of leading bytes we buffer to rewrite
// Trino's protocol URL fields. Trino's metadata (which holds all URL fields)
// is always at the start of the JSON and far smaller than this; larger pages
// stream their remainder unchanged.
const rewritePrefixBytes = 64 << 10 // 64 KiB

// uriRewriteRe matches a Trino V1 protocol URL field and captures the scheme
// and path so the authority can be rewritten to the client-facing host.
var uriRewriteRe = regexp.MustCompile(`"(infoUri|nextUri|partialCancelUri)":\s*"(https?)://[^/"]*(/[^"]*)"`)

// newReverseProxy builds an httputil.ReverseProxy that forwards requests to
// the upstream database coordinator.
//
// It uses httputil.ReverseProxy as required by the architecture principles:
// no custom TCP pooling or socket handling. A non-zero FlushInterval streams
// large response bodies incrementally (periodic flushing) rather than buffering
// a single large page in memory, and the transport is configured with sane
// dial / response-header timeouts.
//
// ModifyResponse rewrites the Trino V1 protocol URLs (infoUri/nextUri/
// partialCancelUri) embedded in the JSON body so a client following them hits
// the client-facing host/port instead of the upstream's internal hostname
// (e.g. a Docker service name the client cannot resolve).
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
			// Capture the client-facing Host before rewriting, so ModifyResponse
			// can rewrite Trino protocol URLs to the address the client used.
			ctx := context.WithValue(req.Context(), clientHostKey{}, req.Host)
			*req = *req.WithContext(ctx)

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

			// Drop the client's Accept-Encoding so Go's transport adds its own
			// "Accept-Encoding: gzip" and auto-decompresses the upstream body.
			// If we forwarded the client's header, the transport would not
			// decompress, and ModifyResponse would receive a gzipped body that
			// it cannot rewrite into client-facing URLs.
			req.Header["Accept-Encoding"] = nil
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
		ModifyResponse: rewriteTrinoURIs,
	}

	return proxy, nil
}

// rewriteTrinoURIs rewrites the client-facing authority into the Trino protocol
// URLs embedded in a JSON response body, so a client following them reaches the
// proxy (the address it connected to) instead of the upstream's internal host.
//
// Trino places all URL fields (infoUri/nextUri/partialCancelUri) in the leading
// metadata before `data`/`columns`. We rewrite a bounded prefix and, when the
// page is larger than that prefix, stream the remainder unchanged. This keeps
// memory flat (bounded by rewritePrefixBytes) for huge pages, while small
// (typical Trino) pages are rewritten whole. The wire output is byte-identical
// for clients apart from the rewritten URLs.
func rewriteTrinoURIs(resp *http.Response) error {
	if resp.Request == nil {
		return nil
	}
	clientHost, _ := resp.Request.Context().Value(clientHostKey{}).(string)
	if clientHost == "" {
		return nil
	}
	if ct, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); ct != "application/json" {
		return nil
	}

	// Read up to the rewrite prefix. Trino's metadata (which holds all URL
	// fields) fits within this for realistic pages.
	head, err := io.ReadAll(io.LimitReader(resp.Body, rewritePrefixBytes))
	if err != nil {
		return err
	}
	rewritten := rewritePrefix(head, clientHost)

	// Restore the body: rewritten prefix + untouched remainder.
	resp.Body = io.NopCloser(io.MultiReader(strings.NewReader(rewritten), resp.Body))

	if len(head) < rewritePrefixBytes {
		// Small page: we consumed it entirely; the body is just `rewritten`.
		resp.Header.Set("Content-Length", fmt.Sprintf("%d", len(rewritten)))
	} else {
		// Huge page: remainder is still to be read; use chunked transfer so the
		// proxy can stream it through without knowing the final length.
		resp.Header.Del("Content-Length")
		resp.Header.Del("Transfer-Encoding")
	}
	return nil
}

// rewritePrefix rewrites Trino protocol URL authorities inside a JSON prefix.
// URL fields that straddle the prefix end are left untouched (their tail is in
// the streamed remainder) — they are short and always fit in the prefix.
func rewritePrefix(b []byte, clientHost string) string {
	return uriRewriteRe.ReplaceAllStringFunc(string(b), func(m string) string {
		sub := uriRewriteRe.FindStringSubmatch(m)
		if len(sub) != 4 {
			return m
		}
		return fmt.Sprintf(`"%s": "%s://%s%s"`, sub[1], sub[2], clientHost, sub[3])
	})
}
