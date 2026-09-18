package httpapi

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/lemonn/typecode-middleware/internal/apperror"
	"github.com/lemonn/typecode-middleware/internal/cache"
	"github.com/lemonn/typecode-middleware/internal/cachemw"
	"github.com/lemonn/typecode-middleware/internal/config"
	"github.com/lemonn/typecode-middleware/internal/httpmw"
	"github.com/lemonn/typecode-middleware/internal/service"
	"github.com/lemonn/typecode-middleware/internal/upstream"
)

// Server wires the HTTP surface together.
type Server struct {
	cfg          config.Config
	log          *slog.Logger
	handler      *Handler
	cacheMW      *cachemw.Middleware
	cacheBackend cache.Cache
	upstream     *upstream.Client
}

func NewServer(
	cfg config.Config,
	log *slog.Logger,
	svc *service.Service,
	cacheMW *cachemw.Middleware,
	cacheBackend cache.Cache,
	up *upstream.Client,
) *Server {
	return &Server{
		cfg:          cfg,
		log:          log,
		handler:      NewHandler(svc, log),
		cacheMW:      cacheMW,
		cacheBackend: cacheBackend,
		upstream:     up,
	}
}

// Routes builds the router.
//
// Middleware order is a correctness concern, not a style choice:
//
//	RequestID    first, so everything after it can be correlated.
//	Logger       outside Recoverer, so a recovered panic still produces a line.
//	Recoverer    outside the cache middleware. singleflight propagates a leader's
//	             panic to every request waiting on the same key, so recovery must
//	             sit where it can catch each of them.
//	Timeout      bounds every request. It does not reach a cache fill already in
//	             progress, which the cache middleware detaches deliberately.
//	RateLimiter  before any per-route work, so rejected traffic costs nothing.
//	Sanitize     before the cache, so invalid requests never build a key and the
//	             query reaching the cache is already canonical.
//	ValidateID   before the cache, for the same reason.
//	Cache        last, immediately around the handler it is caching.
func (s *Server) Routes() http.Handler {
	r := chi.NewRouter()

	r.Use(httpmw.RequestID)
	r.Use(httpmw.Logger(s.log))
	r.Use(httpmw.Recoverer(s.log))
	r.Use(httpmw.Timeout(s.cfg.Server.RequestTimeout.Std()))

	if s.cfg.RateLimit.Enabled {
		limiter := httpmw.NewRateLimiter(httpmw.RateLimitOptions{
			RequestsPerSecond: s.cfg.RateLimit.RequestsPerSecond,
			Burst:             s.cfg.RateLimit.Burst,
			MaxClients:        s.cfg.RateLimit.MaxClients,
			ClientTTL:         s.cfg.RateLimit.ClientTTL.Std(),
			TrustProxyHeaders: s.cfg.RateLimit.TrustProxyHeaders,
			OnReject: func(w http.ResponseWriter, r *http.Request) {
				writeError(w, r, apperror.RateLimited(), s.log)
			},
		})
		r.Use(limiter.Middleware)
	}

	r.Get("/healthz", s.handleHealth)
	r.Get("/readyz", s.handleReady)

	validateID := requireResourceID(s.cfg.Validation.MaxResourceID, s.log)

	r.Route("/posts", func(r chi.Router) {
		r.With(
			s.sanitizeFor(config.RoutePostsList),
			s.cacheFor(config.RoutePostsList),
		).Get("/", s.handler.ListPosts)

		r.With(
			s.sanitizeFor(config.RoutePostsItem),
			validateID,
			s.cacheFor(config.RoutePostsItem),
		).Get("/{id}", s.handler.GetPost)
	})

	r.Route("/todos", func(r chi.Router) {
		r.With(
			s.sanitizeFor(config.RouteTodosList),
			s.cacheFor(config.RouteTodosList),
		).Get("/", s.handler.ListTodos)

		r.With(
			s.sanitizeFor(config.RouteTodosItem),
			validateID,
			s.cacheFor(config.RouteTodosItem),
		).Get("/{id}", s.handler.GetTodo)
	})

	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, r, apperror.NotFound("no such route"), s.log)
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, r, apperror.BadRequest("method not allowed for this route"), s.log)
	})

	return r
}

// cacheFor mounts the caching middleware with a route's configured policy. This
// is the enable/disable switch: a route that is not wrapped is never cached, and
// a wrapped route whose policy is disabled passes straight through.
func (s *Server) cacheFor(route string) func(http.Handler) http.Handler {
	return s.cacheMW.For(cachemw.PolicyFromConfig(route, s.cfg.Cache.Routes[route]))
}

// sanitizeFor builds the query policy for a route by joining the parameter names
// from configuration with the validators defined here. Configuration decides
// which parameters exist; code decides what a valid value looks like.
func (s *Server) sanitizeFor(route string) func(http.Handler) http.Handler {
	rp := s.cfg.Cache.Routes[route]

	allow := make(map[string]httpmw.ParamValidator, len(rp.AllowQuery))
	for _, name := range rp.AllowQuery {
		allow[name] = s.validatorFor(name)
	}

	deny := make(map[string]string, len(rp.DenyQuery))
	for _, name := range rp.DenyQuery {
		// Rejected rather than ignored: the upstream honours these, so dropping
		// one silently would answer a different question than the one asked.
		deny[name] = "not supported by this proxy"
	}

	return httpmw.SanitizeQuery(
		httpmw.QueryPolicy{Allow: allow, Deny: deny},
		func(w http.ResponseWriter, r *http.Request, err error) { writeError(w, r, err, s.log) },
	)
}

func (s *Server) validatorFor(name string) httpmw.ParamValidator {
	switch name {
	case "userId":
		return positiveIntValidator(name, s.cfg.Validation.MaxUserID)
	case "completed":
		return boolValidator
	default:
		// An allowlisted parameter with no validator is accepted as-is. Reaching
		// this means configuration named a parameter the code does not know, so
		// it is worth a warning rather than a silent pass.
		s.log.Warn("allowlisted query parameter has no validator; values unchecked",
			slog.String("param", name))
		return nil
	}
}
