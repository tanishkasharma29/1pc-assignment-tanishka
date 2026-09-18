package cachemw

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

// cacheKey carries both forms of the key: a fixed-width hash used for storage
// and coalescing, and the human-readable derivation used in logs and the debug
// header. Hashing bounds key size regardless of URL length; keeping the readable
// form means a cache question can still be answered from a log line.
type cacheKey struct {
	hash     string
	readable string
}

// buildKey derives the cache key from the request.
//
// Query parameters are sorted so that ?a=1&b=2 and ?b=2&a=1 resolve to one
// entry rather than two — the requirement says "the same parameters", not "the
// same string". Repeated values are sorted too, so key derivation depends on the
// request's meaning and never on its byte order.
//
// The middleware itself knows nothing about which parameters exist. Deciding
// which are meaningful is the query sanitiser's job, and it runs first, so by
// the time a request reaches here the query already contains only parameters
// that genuinely affect the response.
func buildKey(r *http.Request, p Policy) cacheKey {
	var sb strings.Builder
	sb.WriteString(r.Method)
	sb.WriteByte('|')
	sb.WriteString(r.URL.EscapedPath())

	if query := r.URL.Query(); len(query) > 0 {
		sb.WriteByte('|')
		sb.WriteString(canonicalQuery(query))
	}

	for _, name := range p.VaryHeaders {
		sb.WriteByte('|')
		sb.WriteString(strings.ToLower(name))
		sb.WriteByte('=')
		sb.WriteString(r.Header.Get(name))
	}

	readable := sb.String()
	sum := sha256.Sum256([]byte(readable))
	return cacheKey{hash: hex.EncodeToString(sum[:]), readable: readable}
}

// canonicalQuery renders query parameters in a stable order. url.Values.Encode
// already sorts by key; sorting each key's values as well removes the last
// source of ordering sensitivity.
func canonicalQuery(query url.Values) string {
	canonical := make(url.Values, len(query))
	for key, values := range query {
		sorted := append([]string(nil), values...)
		sort.Strings(sorted)
		canonical[key] = sorted
	}
	return canonical.Encode()
}
