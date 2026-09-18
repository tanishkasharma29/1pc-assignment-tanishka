// Package httpmw holds the cross-cutting HTTP middleware: identity, logging,
// panic recovery, rate limiting and query sanitisation.
//
// Response caching deliberately lives in its own package (cachemw) because it is
// the subject of the exercise and benefits from being readable in isolation.
package httpmw

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
)

// RequestIDHeader is both read and written: an upstream proxy's id is preferred
// over a fresh one so a request can be traced across hops.
const RequestIDHeader = "X-Request-Id"

type ctxKey int

const requestIDKey ctxKey = iota

// RequestID assigns every request a stable identifier, available to every later
// middleware and handler through the context and echoed back to the client.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(RequestIDHeader)
		if id == "" || len(id) > 128 {
			// Regenerate rather than trust an oversized value: request ids end
			// up in logs, and unbounded client-controlled strings do not belong
			// there.
			id = newRequestID()
		}
		w.Header().Set(RequestIDHeader, id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey, id)))
	})
}

// RequestIDFrom returns the request id, or "" outside a request.
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

func newRequestID() string {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return "unknown"
	}
	return hex.EncodeToString(buf)
}
