package httpmw

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The three-way rule these tests cover was derived from probing the live
// upstream, which treats the two kinds of unsupported parameter differently:
// it silently ignores ones it does not recognise, but genuinely honours
// _limit/_start/_page. Copying that behaviour requires making the same
// distinction rather than applying one blanket policy.

func testPolicy() QueryPolicy {
	return QueryPolicy{
		Allow: map[string]ParamValidator{
			"userId": func(v string) error {
				if v != "1" && v != "2" {
					return fmt.Errorf("must be 1 or 2")
				}
				return nil
			},
			"completed": func(v string) error {
				if v != "true" && v != "false" {
					return fmt.Errorf("must be true or false")
				}
				return nil
			},
		},
		Deny: map[string]string{"_limit": "not supported", "_page": "not supported"},
	}
}

// sanitize runs the middleware and reports the query the handler observed.
func sanitize(t *testing.T, target string) (status int, observedQuery string) {
	t.Helper()

	var errStatus int
	writeError := func(w http.ResponseWriter, r *http.Request, err error) {
		errStatus = http.StatusBadRequest
		w.WriteHeader(http.StatusBadRequest)
	}

	handler := SanitizeQuery(testPolicy(), writeError)(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			observedQuery = r.URL.RawQuery
			w.WriteHeader(http.StatusOK)
		}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	if errStatus != 0 {
		return errStatus, ""
	}
	return rec.Code, observedQuery
}

// TestUnrecognisedParametersAreStripped.
//
// Stripping, rather than rejecting, is the correct behaviour: the upstream
// ignores these and returns the identical response, so rejecting would be less
// faithful than the API being proxied. It also collapses what would otherwise be
// an unbounded cache keyspace — ?x=1, ?x=2, ?x=3 ... would each be a distinct
// key holding byte-identical data, and each one a separate billable call.
func TestUnrecognisedParametersAreStripped(t *testing.T) {
	t.Parallel()

	status, query := sanitize(t, "/posts?foo=bar&baz=qux")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: unknown parameters must not be an error", status)
	}
	if query != "" {
		t.Errorf("handler saw query %q, want it stripped to empty", query)
	}
}

func TestUnrecognisedParametersCollapseOntoTheBareRoute(t *testing.T) {
	t.Parallel()

	_, withJunk := sanitize(t, "/posts?tracking=abc123")
	_, bare := sanitize(t, "/posts")
	if withJunk != bare {
		t.Errorf("?tracking=abc123 canonicalised to %q but the bare route is %q; "+
			"they must share a cache key", withJunk, bare)
	}
}

// TestDeniedParametersAreRejected.
//
// These are honoured by the upstream, so silently dropping one would answer a
// different question than the one asked — returning 100 items to a client that
// requested 5. A wrong answer is worse than an honest error.
func TestDeniedParametersAreRejected(t *testing.T) {
	t.Parallel()

	for _, target := range []string{"/posts?_limit=5", "/posts?_page=2", "/posts?userId=1&_limit=5"} {
		if status, _ := sanitize(t, target); status != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", target, status)
		}
	}
}

func TestAllowedParametersArePreserved(t *testing.T) {
	t.Parallel()

	status, query := sanitize(t, "/todos?userId=1&completed=true")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if query != "completed=true&userId=1" {
		t.Errorf("query = %q, want the canonical sorted form", query)
	}
}

// TestQueryIsCanonicalisedForKeying: sorting here is what makes the cache key
// order-insensitive downstream.
func TestQueryIsCanonicalisedForKeying(t *testing.T) {
	t.Parallel()

	_, first := sanitize(t, "/todos?userId=1&completed=true")
	_, second := sanitize(t, "/todos?completed=true&userId=1")
	if first != second {
		t.Errorf("two orderings produced %q and %q; they must canonicalise identically", first, second)
	}
}

func TestInvalidValuesAreRejected(t *testing.T) {
	t.Parallel()

	for _, target := range []string{"/posts?userId=99", "/posts?userId=abc", "/todos?completed=yes"} {
		if status, _ := sanitize(t, target); status != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", target, status)
		}
	}
}

// TestRepeatedParametersAreRejected: ?userId=1&userId=2 has no defensible
// meaning, and picking one silently would let the cache key disagree with the
// response actually produced.
func TestRepeatedParametersAreRejected(t *testing.T) {
	t.Parallel()

	if status, _ := sanitize(t, "/posts?userId=1&userId=2"); status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", status)
	}
}

func TestMalformedQueryIsRejected(t *testing.T) {
	t.Parallel()

	if status, _ := sanitize(t, "/posts?%zz=1"); status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", status)
	}
}
