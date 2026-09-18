package httpapi

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/lemonn/typecode-middleware/internal/cachemw"
	"github.com/lemonn/typecode-middleware/internal/upstream"
)

// healthResponse answers "is this process alive", which is independent of
// whether its dependencies are.
type healthResponse struct {
	Status string    `json:"status"`
	Time   time.Time `json:"time"`
}

// readyResponse answers "should this instance receive traffic", and carries the
// numbers that describe how well the cache is doing its job.
type readyResponse struct {
	Status  string               `json:"status"`
	Cache   cachemw.Stats        `json:"cache"`
	Budget  upstream.BudgetState `json:"upstream_budget"`
	Calls   int64                `json:"upstream_calls"`
	Backend string               `json:"cache_backend"`
	Detail  string               `json:"detail,omitempty"`
}

// handleHealth is a liveness probe: it must not touch dependencies, or a Redis
// outage would cause the orchestrator to restart healthy processes.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, r, http.StatusOK, healthResponse{Status: "ok", Time: time.Now().UTC()}, s.log)
}

// handleReady is a readiness probe. A cache backend that cannot be reached is
// reported as degraded rather than down: the proxy still serves correct results
// without it, it just costs more to do so.
func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	resp := readyResponse{
		Status:  "ready",
		Cache:   s.cacheMW.Stats(),
		Budget:  s.upstream.Budget().Snapshot(),
		Calls:   s.upstream.Calls(),
		Backend: s.cacheBackend.Name(),
	}

	if err := s.cacheBackend.Ping(r.Context()); err != nil {
		resp.Status = "degraded"
		resp.Detail = "cache backend unreachable; serving without shared cache"
		s.log.WarnContext(r.Context(), "cache backend ping failed",
			slog.String("backend", s.cacheBackend.Name()),
			slog.String("error", err.Error()))
	}

	// Deliberately 200 even when degraded. Losing the shared cache costs money,
	// not correctness, and pulling the instance out of rotation would not fix
	// the cache while removing capacity that still works.
	writeJSON(w, r, http.StatusOK, resp, s.log)
}
