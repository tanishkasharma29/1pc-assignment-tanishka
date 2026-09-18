package httpapi

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/lemonn/typecode-middleware/internal/apperror"
	"github.com/lemonn/typecode-middleware/internal/httpmw"
)

type paramCtxKey int

const resourceIDKey paramCtxKey = iota

// requireResourceID validates the {id} path parameter before the request reaches
// the cache or the upstream.
//
// This is the cheapest saving in the system. The upstream does not reject
// malformed ids cheaply — /posts/abc, /posts/0 and /posts/-1 all come back as a
// billable 404 — so refusing them here turns a paid round trip into a local 400.
// It runs ahead of the caching middleware so these requests never even produce a
// cache key, which is what keeps the reachable keyspace bounded.
func requireResourceID(maxID int, log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw := chi.URLParam(r, "id")

			id, err := strconv.Atoi(raw)
			if err != nil {
				writeError(w, r, apperror.BadRequest("invalid resource id").
					WithField("id", "must be an integer"), log)
				return
			}
			if id < 1 {
				writeError(w, r, apperror.BadRequest("invalid resource id").
					WithField("id", "must be greater than zero"), log)
				return
			}
			if id > maxID {
				// An upper bound is what makes the keyspace finite. Without it a
				// caller can mint unlimited distinct ids, each one a cache entry
				// and, on first sight, a billable call.
				writeError(w, r, apperror.BadRequest("invalid resource id").
					WithField("id", fmt.Sprintf("must not exceed %d", maxID)), log)
				return
			}

			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), resourceIDKey, id)))
		})
	}
}

// resourceIDFrom returns the id validated by requireResourceID. Handlers reached
// through that middleware can rely on it being present and in range.
func resourceIDFrom(ctx context.Context) int {
	id, _ := ctx.Value(resourceIDKey).(int)
	return id
}

// positiveIntValidator bounds a numeric query parameter. Same reasoning as the
// path id: an unbounded accepted range is an unbounded cache keyspace, and an
// empty result (the upstream answers `200 []` for an unmatched filter) is a
// perfectly cacheable success, so it offers no natural limit of its own.
func positiveIntValidator(name string, max int) httpmw.ParamValidator {
	return func(value string) error {
		n, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("must be an integer")
		}
		if n < 1 {
			return fmt.Errorf("must be greater than zero")
		}
		if n > max {
			return fmt.Errorf("must not exceed %d", max)
		}
		return nil
	}
}

// boolValidator accepts only the two spellings the upstream understands.
// strconv.ParseBool would also accept "1", "t" and "TRUE", which would map
// several distinct cache keys onto one response.
func boolValidator(value string) error {
	if value != "true" && value != "false" {
		return fmt.Errorf("must be \"true\" or \"false\"")
	}
	return nil
}
