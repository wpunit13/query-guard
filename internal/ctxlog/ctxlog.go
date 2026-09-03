// Package ctxlog carries per-request identity through the proxy pipeline.
//
// The request ID is the correlation key between a client's statement, the
// guard's log lines, the pre-flight evaluation, and any rejection returned
// to the client. It is generated per statement request (or honored from an
// inbound X-Request-ID header) and propagated via context so both the proxy
// handler and engine log lines can include it.
package ctxlog

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

// HeaderName is the inbound header honored for request correlation.
const HeaderName = "X-Request-ID"

type ctxKey struct{}

// NewID returns a short random request ID (16 hex chars). It is
// correlation-only metadata, never auth material.
func NewID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is catastrophic but must not take the proxy
		// down (fail-open principle); fall back to a timestamp-derived ID.
		return fmt.Sprintf("t%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// WithRequestID returns a context carrying the request ID.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// FromContext returns the request ID stored in ctx, or "" if absent.
func FromContext(ctx context.Context) string {
	id, _ := ctx.Value(ctxKey{}).(string)
	return id
}
