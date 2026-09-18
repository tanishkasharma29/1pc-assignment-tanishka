package httpmw

import (
	"net/http"
	"net/url"

	"github.com/lemonn/typecode-middleware/internal/apperror"
)

// ParamValidator checks one parameter value. A nil validator accepts anything.
type ParamValidator func(value string) error

// QueryPolicy classifies query parameters into three groups, because the
// upstream treats two kinds of unsupported parameter very differently and
// copying its behaviour requires making the same distinction.
//
//	Allow  validated, forwarded, and part of the cache key.
//
//	Deny   rejected with 400. These are parameters the upstream honours but we
//	       do not support. Quietly dropping one would return a materially wrong
//	       answer — a client asking for 5 items would receive 100 — and a wrong
//	       answer is worse than an honest error.
//
//	other  silently stripped. The upstream ignores unrecognised parameters and
//	       returns the identical response, so stripping them reproduces its
//	       behaviour exactly. It also collapses what would otherwise be an
//	       unbounded cache keyspace: without this, ?x=1, ?x=2, ?x=3 ... are
//	       distinct keys holding byte-identical responses, and every one of them
//	       is a separate billable call.
type QueryPolicy struct {
	Allow map[string]ParamValidator
	Deny  map[string]string // name -> client-facing reason
}

// ErrorWriter renders a rejection. It is injected so this package stays free of
// any particular response format.
type ErrorWriter func(http.ResponseWriter, *http.Request, error)

// SanitizeQuery enforces a QueryPolicy before any downstream middleware runs.
//
// Ordering matters: this must sit in front of the caching middleware. Rejections
// then happen before a cache key is built or an upstream call is considered, so
// a malformed request costs nothing at all — and the cache middleware receives a
// query string already reduced to the parameters that genuinely affect the
// response.
func SanitizeQuery(policy QueryPolicy, writeError ErrorWriter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, err := url.ParseQuery(r.URL.RawQuery)
			if err != nil {
				writeError(w, r, apperror.BadRequest("query string is malformed"))
				return
			}

			kept := make(url.Values, len(raw))
			for name, values := range raw {
				if reason, denied := policy.Deny[name]; denied {
					writeError(w, r, apperror.BadRequest(
						"unsupported query parameter").WithField(name, reason))
					return
				}

				validate, allowed := policy.Allow[name]
				if !allowed {
					continue // unrecognised: stripped, matching upstream behaviour
				}
				// Repeats are refused rather than guessed at: ?userId=1&userId=2
				// has no obvious meaning, and picking one silently would make
				// the cache key disagree with the response.
				if len(values) != 1 {
					writeError(w, r, apperror.BadRequest(
						"query parameter must appear exactly once").WithField(name, "repeated"))
					return
				}
				if validate != nil {
					if err := validate(values[0]); err != nil {
						writeError(w, r, apperror.BadRequest(
							"invalid query parameter").WithField(name, err.Error()))
						return
					}
				}
				kept[name] = values
			}

			// Rewrite the query canonically. Encode sorts by key, so downstream
			// key derivation is order-insensitive by construction.
			r.URL.RawQuery = kept.Encode()
			next.ServeHTTP(w, r)
		})
	}
}
