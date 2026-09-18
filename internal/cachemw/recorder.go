package cachemw

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"time"

	"github.com/lemonn/typecode-middleware/internal/cache"
)

// recorder captures a complete response instead of forwarding it.
//
// Buffering rather than writing through is a requirement, not an optimisation.
// The cache entry must be stored before any byte reaches the client; if the
// response streamed out as it was produced, a fast client could issue its next
// request and miss the cache before the write completed, causing a second
// billable upstream call. Buffering is what makes that ordering enforceable.
//
// Note the deliberate absence of a Flush method. Implementing http.Flusher would
// advertise a capability this type cannot honour, and a handler that flushed
// would break the ordering guarantee above. Omitting it makes the type assertion
// fail, so handlers correctly fall back to buffered behaviour.
type recorder struct {
	header      http.Header
	status      int
	body        bytes.Buffer
	wroteHeader bool

	// cacheLimit is the size above which a response is served but not stored.
	// Overall memory is bounded independently by the upstream client's own
	// response cap, so a large body cannot grow without limit here.
	cacheLimit int64
	overflow   bool
}

func newRecorder(cacheLimit int64) *recorder {
	return &recorder{header: make(http.Header), cacheLimit: cacheLimit}
}

func (rec *recorder) Header() http.Header { return rec.header }

func (rec *recorder) WriteHeader(status int) {
	if rec.wroteHeader {
		return // mirrors net/http: the first WriteHeader wins
	}
	rec.status = status
	rec.wroteHeader = true
}

func (rec *recorder) Write(p []byte) (int, error) {
	if !rec.wroteHeader {
		rec.WriteHeader(http.StatusOK)
	}
	n, err := rec.body.Write(p)
	if int64(rec.body.Len()) > rec.cacheLimit {
		rec.overflow = true
	}
	return n, err
}

// statusCode reports the effective status, accounting for handlers that write a
// body without an explicit WriteHeader.
func (rec *recorder) statusCode() int {
	if rec.status == 0 {
		return http.StatusOK
	}
	return rec.status
}

// cacheable reports whether the captured response may be stored at all. Size is
// the only concern here; whether the status is worth caching is a policy
// decision made elsewhere.
func (rec *recorder) cacheable() bool { return !rec.overflow }

// entry converts the captured response into a cache entry, keeping only
// allowlisted headers.
//
// The allowlist is what keeps per-request headers (Date, Set-Cookie) and
// hop-by-hop headers out of the cache, where replaying them to a different
// client later would be wrong or unsafe.
func (rec *recorder) entry(storeHeaders []string) *cache.Entry {
	header := make(http.Header, len(storeHeaders))
	for _, name := range storeHeaders {
		if values := rec.header.Values(name); len(values) > 0 {
			header[http.CanonicalHeaderKey(name)] = append([]string(nil), values...)
		}
	}

	body := append([]byte(nil), rec.body.Bytes()...)

	// Derive a strong validator if the handler did not supply one. The body is
	// known in full at this point, so the hash is exact, and storing it lets
	// repeat clients be answered with 304 instead of the payload.
	if header.Get("ETag") == "" && len(body) > 0 {
		sum := sha256.Sum256(body)
		header.Set("ETag", `"`+hex.EncodeToString(sum[:16])+`"`)
	}

	return &cache.Entry{
		Status:   rec.statusCode(),
		Header:   header,
		Body:     body,
		StoredAt: time.Now(),
	}
}
