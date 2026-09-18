package httpmw

import (
	"log/slog"
	"net/http"
	"time"
)

// Logger emits one structured line per request.
//
// It deliberately records the X-Cache header set by the caching middleware: hit
// ratio is the metric this whole service is optimised for, so it belongs on
// every request line rather than behind a separate metrics endpoint.
func Logger(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			started := time.Now()
			obs := newObserver(w)

			next.ServeHTTP(obs, r)

			attrs := []slog.Attr{
				slog.String("request_id", RequestIDFrom(r.Context())),
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.String("query", r.URL.RawQuery),
				slog.Int("status", obs.status),
				slog.Int("bytes", obs.bytes),
				slog.Duration("duration", time.Since(started)),
				slog.String("remote_addr", clientIP(r, false)),
			}
			if cacheStatus := obs.Header().Get("X-Cache"); cacheStatus != "" {
				attrs = append(attrs, slog.String("cache", cacheStatus))
			}

			// Server faults are the operator's problem and are logged as errors;
			// client faults are the caller's and are not.
			level := slog.LevelInfo
			switch {
			case obs.status >= 500:
				level = slog.LevelError
			case obs.status >= 400:
				level = slog.LevelWarn
			}
			log.LogAttrs(r.Context(), level, "http request", attrs...)
		})
	}
}
