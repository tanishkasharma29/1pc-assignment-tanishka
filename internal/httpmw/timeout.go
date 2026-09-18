package httpmw

import (
	"context"
	"net/http"
	"time"
)

// Timeout bounds a request by attaching a deadline to its context.
//
// A context deadline is used rather than http.TimeoutHandler for two reasons.
// TimeoutHandler buffers the entire response, which the caching middleware
// already does, so the pair would double-buffer every payload. More importantly,
// it abandons the handler's goroutine while leaving it running, whereas a
// deadline propagates into the upstream client and actually stops the work.
//
// This deadline intentionally does not reach a cache fill in progress: the
// caching middleware detaches that from the request context so one caller's
// deadline cannot cancel a fetch that other callers are waiting on.
func Timeout(d time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), d)
			defer cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
