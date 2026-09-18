package httpmw

import (
	"log/slog"
	"net/http"
	"runtime/debug"
)

// Recoverer converts a panic into a 500 instead of killing the process.
//
// Its position in the chain matters: it must sit outside the caching middleware.
// singleflight propagates a leader's panic to every request waiting on the same
// key, so without an outer recovery a single bad response would take down every
// concurrent caller rather than just its own request.
func Recoverer(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				rec := recover()
				if rec == nil {
					return
				}
				// http.ErrAbortHandler is the documented way for a handler to
				// abandon a response deliberately; re-panicking preserves that
				// contract instead of logging it as a fault.
				if rec == http.ErrAbortHandler {
					panic(rec)
				}

				log.ErrorContext(r.Context(), "panic recovered",
					slog.String("request_id", RequestIDFrom(r.Context())),
					slog.String("method", r.Method),
					slog.String("path", r.URL.Path),
					slog.Any("panic", rec),
					slog.String("stack", string(debug.Stack())))

				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"error":{"code":"internal_error","message":"internal server error"}}`))
			}()

			next.ServeHTTP(w, r)
		})
	}
}
