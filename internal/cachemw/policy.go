package cachemw

import (
	"net/http"
	"time"

	"github.com/lemonn/typecode-middleware/internal/config"
)

// Policy is the per-route cache configuration, resolved from config.
//
// Holding policy per route rather than globally is what makes the middleware
// configurable in the way the requirements ask for: the same generic middleware
// can cache one route aggressively, cache another briefly, and skip a third
// entirely.
type Policy struct {
	Name    string
	Enabled bool
	// TTL of 0 means the entry never expires, which is the right answer for
	// resources the upstream guarantees are immutable.
	TTL time.Duration
	// NegativeTTL governs cacheable failures. Note the deliberate asymmetry with
	// TTL: here 0 means "do not cache negatives at all" rather than "never
	// expire". Positive keys come from a small set we validate and control;
	// negative keys are chosen by the caller and unbounded in cardinality, so
	// making them immortal would hand out unlimited memory to anyone who asked.
	NegativeTTL time.Duration
	// VaryHeaders are folded into the cache key when the response legitimately
	// differs by request header.
	VaryHeaders []string
}

// PolicyFromConfig resolves a configured route policy, applying defaults for
// fields the YAML omitted.
func PolicyFromConfig(name string, rp config.RoutePolicy) Policy {
	p := Policy{Name: name, Enabled: true, VaryHeaders: rp.VaryHeaders}
	if rp.Enabled != nil {
		p.Enabled = *rp.Enabled
	}
	if rp.TTL != nil {
		p.TTL = rp.TTL.Std()
	}
	if rp.NegativeTTL != nil {
		p.NegativeTTL = rp.NegativeTTL.Std()
	}
	return p
}

// ttlFor decides whether a response may be cached and for how long.
//
// Only two classes of response are storable:
//
//   - 2xx, the answer itself. An empty collection is included: the upstream
//     returns `200 []` for an unmatched filter, and that is a correct answer,
//     not a failure.
//   - 404, which for immutable data is a permanent, valid answer. Caching it is
//     what stops a loop over nonexistent IDs from billing us on every request.
//
// Everything else — 5xx, 429, timeouts, our own 4xx validation rejections — is
// never cached. Those are either transient, or cheap enough to recompute that
// storing them would only risk serving a stale failure.
func (p Policy) ttlFor(status int) (time.Duration, bool) {
	switch {
	case status >= http.StatusOK && status < http.StatusMultipleChoices:
		return p.TTL, true
	case status == http.StatusNotFound:
		if p.NegativeTTL <= 0 {
			return 0, false
		}
		return p.NegativeTTL, true
	default:
		return 0, false
	}
}
