// Package config loads and validates runtime configuration.
//
// Precedence is defaults < YAML file < environment. The YAML file carries the
// per-route cache policy table, which is inherently hierarchical and reads badly
// as flat env vars; environment variables override individual scalars so a
// container can be retuned without rebuilding an image.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Route policy names. These are the keys of Cache.Routes and the suffixes used
// by the CACHE_ROUTE_<NAME>_* environment overrides.
const (
	RoutePostsList = "posts_list"
	RoutePostsItem = "posts_item"
	RouteTodosList = "todos_list"
	RouteTodosItem = "todos_item"
)

// Config is the fully resolved application configuration.
type Config struct {
	Env        string     `yaml:"env"`
	Server     Server     `yaml:"server"`
	Log        Log        `yaml:"log"`
	Upstream   Upstream   `yaml:"upstream"`
	Cache      Cache      `yaml:"cache"`
	RateLimit  RateLimit  `yaml:"rate_limit"`
	Validation Validation `yaml:"validation"`
}

// Validation bounds the values the proxy will accept in a path or query.
//
// These are a cost control, not merely input hygiene. Every accepted value is a
// potential cache key, and a key the caller chooses freely is a key they can
// create without limit. The upstream ignores such bounds entirely — it answers
// /posts/abc and /posts/-1 with a billable 404 — so enforcing them here is what
// turns "cost is reduced" into "cost is bounded".
//
// The defaults sit deliberately above the observed dataset (ids 1-100 and 1-200,
// users 1-10) so modest upstream growth does not break the proxy, while still
// keeping the reachable keyspace small enough for the cache to hold in full.
type Validation struct {
	MaxResourceID int `yaml:"max_resource_id"`
	MaxUserID     int `yaml:"max_user_id"`
}

type Server struct {
	Addr              string   `yaml:"addr"`
	ReadHeaderTimeout Duration `yaml:"read_header_timeout"`
	ReadTimeout       Duration `yaml:"read_timeout"`
	WriteTimeout      Duration `yaml:"write_timeout"`
	IdleTimeout       Duration `yaml:"idle_timeout"`
	// ShutdownTimeout bounds how long in-flight requests may take to drain.
	ShutdownTimeout Duration `yaml:"shutdown_timeout"`
	// RequestTimeout bounds a single request end-to-end. It must exceed the
	// upstream budget below or slow fetches would be cancelled by their own
	// deadline before the upstream client had a chance to retry.
	RequestTimeout Duration `yaml:"request_timeout"`
	MaxHeaderBytes int      `yaml:"max_header_bytes"`
}

type Log struct {
	Level  string `yaml:"level"`  // debug | info | warn | error
	Format string `yaml:"format"` // json | text
}

type Upstream struct {
	BaseURL string `yaml:"base_url"`
	// Timeout bounds one attempt, not the whole retry sequence.
	Timeout Duration `yaml:"timeout"`
	// MaxAttempts includes the first try, so 3 means "try, retry, retry".
	MaxAttempts         int      `yaml:"max_attempts"`
	RetryBaseDelay      Duration `yaml:"retry_base_delay"`
	RetryMaxDelay       Duration `yaml:"retry_max_delay"`
	MaxResponseBytes    int64    `yaml:"max_response_bytes"`
	MaxIdleConns        int      `yaml:"max_idle_conns"`
	MaxIdleConnsPerHost int      `yaml:"max_idle_conns_per_host"`
	// Budget is the global ceiling on billable calls (see Budget docs).
	Budget Budget `yaml:"budget"`
}

// Budget bounds total upstream spend per window. Unlike a per-client rate limit,
// which only constrains one caller, this is the only true ceiling on cost.
type Budget struct {
	Enabled bool `yaml:"enabled"`
	// MaxCalls per Window. Upstream publishes x-ratelimit-limit: 1000, so this
	// defaults below that and is additionally reconciled against the live
	// x-ratelimit-remaining header observed on each response.
	MaxCalls int      `yaml:"max_calls"`
	Window   Duration `yaml:"window"`
	// ReserveRemaining refuses to spend the last N of the upstream's own
	// published allowance, leaving headroom for traffic we cannot foresee.
	ReserveRemaining int `yaml:"reserve_remaining"`
}

type Cache struct {
	Enabled bool   `yaml:"enabled"`
	Backend string `yaml:"backend"` // memory | redis
	// MaxBodyBytes caps what may be cached. G2 requires buffering the whole
	// response before flushing, so this is also the memory bound per entry.
	MaxBodyBytes int64 `yaml:"max_body_bytes"`
	// StoreHeaders is an allowlist. Anything absent is neither stored nor
	// replayed, which keeps per-request headers (Date, Set-Cookie) and
	// hop-by-hop headers out of the cache.
	StoreHeaders []string               `yaml:"store_headers"`
	Memory       Memory                 `yaml:"memory"`
	Redis        Redis                  `yaml:"redis"`
	Routes       map[string]RoutePolicy `yaml:"routes"`
	// DebugHeaders adds X-Cache-Key to responses. Never enable in production:
	// it reveals the key derivation to clients.
	DebugHeaders bool `yaml:"debug_headers"`
}

type Memory struct {
	// MaxEntries bounds the positive (2xx) partition.
	MaxEntries int `yaml:"max_entries"`
	// MaxNegativeEntries bounds the non-2xx partition separately. Negative keys
	// are attacker-controlled and unbounded in cardinality, so sharing one LRU
	// would let 404 spam evict the real dataset.
	MaxNegativeEntries int      `yaml:"max_negative_entries"`
	MaxBytes           int64    `yaml:"max_bytes"`
	JanitorInterval    Duration `yaml:"janitor_interval"`
}

type Redis struct {
	Addr         string   `yaml:"addr"`
	Password     string   `yaml:"password"`
	DB           int      `yaml:"db"`
	KeyPrefix    string   `yaml:"key_prefix"`
	DialTimeout  Duration `yaml:"dial_timeout"`
	ReadTimeout  Duration `yaml:"read_timeout"`
	WriteTimeout Duration `yaml:"write_timeout"`
	PoolSize     int      `yaml:"pool_size"`
	// Lock controls cross-instance coordination. This, not capacity, is the
	// entire reason Redis exists in this design.
	Lock Lock `yaml:"lock"`
}

type Lock struct {
	Enabled bool `yaml:"enabled"`
	// TTL bounds how long a crashed leader can block the fleet.
	TTL Duration `yaml:"ttl"`
	// WaitTimeout is how long a follower polls for the leader's result before
	// failing open and fetching itself. Availability is chosen over a strict
	// cost bound; the budget remains the real ceiling.
	WaitTimeout  Duration `yaml:"wait_timeout"`
	PollInterval Duration `yaml:"poll_interval"`
}

// RoutePolicy is the per-route cache configuration. Pointers distinguish "not
// set in YAML" from "explicitly set to the zero value", which matters because
// ttl: 0 legitimately means "never expire".
type RoutePolicy struct {
	Enabled *bool `yaml:"enabled"`
	// TTL of 0 means never expire, which is correct for immutable resources.
	TTL *Duration `yaml:"ttl"`
	// NegativeTTL applies to cacheable failures (404). Kept short because the
	// keyspace is attacker-controlled.
	NegativeTTL *Duration `yaml:"negative_ttl"`
	// AllowQuery lists params that are validated, forwarded and included in the
	// cache key. Unrecognised params are stripped; DenyQuery params are refused.
	AllowQuery []string `yaml:"allow_query"`
	// DenyQuery lists params the upstream honours but we decline to support.
	// Stripping these would return a materially wrong answer (e.g. 100 items to
	// a client that asked for 5), so they are rejected with 400 instead.
	DenyQuery   []string `yaml:"deny_query"`
	VaryHeaders []string `yaml:"vary_headers"`
}

type RateLimit struct {
	Enabled bool `yaml:"enabled"`
	// RequestsPerSecond and Burst are per client IP.
	RequestsPerSecond float64  `yaml:"requests_per_second"`
	Burst             int      `yaml:"burst"`
	MaxClients        int      `yaml:"max_clients"`
	ClientTTL         Duration `yaml:"client_ttl"`
	// TrustProxyHeaders makes the limiter read X-Forwarded-For. Only enable
	// behind a proxy you control, otherwise clients can spoof their identity
	// and defeat the limit entirely.
	TrustProxyHeaders bool `yaml:"trust_proxy_headers"`
}

// Default returns a configuration that runs correctly with no file and no
// environment: memory backend, no Redis, no Docker. Redis is opt-in.
func Default() Config {
	return Config{
		Env: "development",
		Server: Server{
			Addr:              ":8080",
			ReadHeaderTimeout: Duration(5 * time.Second),
			ReadTimeout:       Duration(15 * time.Second),
			WriteTimeout:      Duration(25 * time.Second),
			IdleTimeout:       Duration(60 * time.Second),
			ShutdownTimeout:   Duration(15 * time.Second),
			RequestTimeout:    Duration(15 * time.Second),
			MaxHeaderBytes:    1 << 20,
		},
		Log: Log{Level: "info", Format: "json"},
		Upstream: Upstream{
			BaseURL:             "https://jsonplaceholder.typicode.com",
			Timeout:             Duration(3 * time.Second),
			MaxAttempts:         3,
			RetryBaseDelay:      Duration(100 * time.Millisecond),
			RetryMaxDelay:       Duration(2 * time.Second),
			MaxResponseBytes:    2 << 20,
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 50,
			Budget: Budget{
				Enabled:          true,
				MaxCalls:         800,
				Window:           Duration(time.Hour),
				ReserveRemaining: 25,
			},
		},
		Cache: Cache{
			Enabled:      true,
			Backend:      "memory",
			MaxBodyBytes: 1 << 20,
			StoreHeaders: []string{"Content-Type", "Content-Language", "ETag", "Last-Modified", "Vary"},
			Memory: Memory{
				MaxEntries:         4096,
				MaxNegativeEntries: 1024,
				MaxBytes:           64 << 20,
				JanitorInterval:    Duration(time.Minute),
			},
			Redis: Redis{
				Addr:         "localhost:6379",
				KeyPrefix:    "typecode:",
				DialTimeout:  Duration(2 * time.Second),
				ReadTimeout:  Duration(time.Second),
				WriteTimeout: Duration(time.Second),
				PoolSize:     20,
				Lock: Lock{
					Enabled:      true,
					TTL:          Duration(10 * time.Second),
					WaitTimeout:  Duration(5 * time.Second),
					PollInterval: Duration(25 * time.Millisecond),
				},
			},
			Routes: defaultRoutes(),
		},
		Validation: Validation{MaxResourceID: 1000, MaxUserID: 100},
		RateLimit: RateLimit{
			Enabled:           true,
			RequestsPerSecond: 50,
			Burst:             100,
			MaxClients:        10000,
			ClientTTL:         Duration(10 * time.Minute),
			TrustProxyHeaders: false,
		},
	}
}

func defaultRoutes() map[string]RoutePolicy {
	yes := true
	immutable := Duration(0) // never expires: the problem guarantees immutability
	collection := Duration(10 * time.Minute)
	negShort := Duration(5 * time.Minute)
	negColl := Duration(time.Minute)
	pagination := []string{"_limit", "_start", "_page", "_sort", "_order"}

	return map[string]RoutePolicy{
		RoutePostsItem: {Enabled: &yes, TTL: &immutable, NegativeTTL: &negShort},
		RouteTodosItem: {Enabled: &yes, TTL: &immutable, NegativeTTL: &negShort},
		RoutePostsList: {
			Enabled: &yes, TTL: &collection, NegativeTTL: &negColl,
			AllowQuery: []string{"userId"}, DenyQuery: pagination,
		},
		RouteTodosList: {
			Enabled: &yes, TTL: &collection, NegativeTTL: &negColl,
			AllowQuery: []string{"userId", "completed"}, DenyQuery: pagination,
		},
	}
}

// Load resolves configuration from defaults, an optional YAML file and the
// environment, then validates the result. A missing file is not an error: the
// defaults are designed to run standalone.
func Load(path string) (Config, error) {
	cfg := Default()

	if path != "" {
		data, err := os.ReadFile(path)
		switch {
		case err == nil:
			// KnownFields rejects typos in the config file rather than silently
			// ignoring them, which is how misconfiguration usually hides.
			dec := yaml.NewDecoder(strings.NewReader(string(data)))
			dec.KnownFields(true)
			if err := dec.Decode(&cfg); err != nil {
				return Config{}, fmt.Errorf("parse config %s: %w", path, err)
			}
		case os.IsNotExist(err):
			// Fall through to defaults.
		default:
			return Config{}, fmt.Errorf("read config %s: %w", path, err)
		}
	}

	if err := applyEnv(&cfg); err != nil {
		return Config{}, err
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// applyEnv overlays environment variables. Only operationally useful knobs are
// exposed; the full policy table stays in YAML where it is readable.
func applyEnv(cfg *Config) error {
	var errs []string
	fail := func(k string, err error) { errs = append(errs, fmt.Sprintf("%s: %v", k, err)) }

	envStr("APP_ENV", &cfg.Env)
	envStr("SERVER_ADDR", &cfg.Server.Addr)
	envStr("LOG_LEVEL", &cfg.Log.Level)
	envStr("LOG_FORMAT", &cfg.Log.Format)
	envStr("UPSTREAM_BASE_URL", &cfg.Upstream.BaseURL)
	envStr("CACHE_BACKEND", &cfg.Cache.Backend)
	envStr("REDIS_ADDR", &cfg.Cache.Redis.Addr)
	envStr("REDIS_PASSWORD", &cfg.Cache.Redis.Password)
	envStr("REDIS_KEY_PREFIX", &cfg.Cache.Redis.KeyPrefix)

	envBool("CACHE_ENABLED", &cfg.Cache.Enabled, fail)
	envBool("CACHE_DEBUG_HEADERS", &cfg.Cache.DebugHeaders, fail)
	envBool("REDIS_LOCK_ENABLED", &cfg.Cache.Redis.Lock.Enabled, fail)
	envBool("RATE_LIMIT_ENABLED", &cfg.RateLimit.Enabled, fail)
	envBool("RATE_LIMIT_TRUST_PROXY", &cfg.RateLimit.TrustProxyHeaders, fail)
	envBool("UPSTREAM_BUDGET_ENABLED", &cfg.Upstream.Budget.Enabled, fail)

	envInt("UPSTREAM_MAX_ATTEMPTS", &cfg.Upstream.MaxAttempts, fail)
	envInt("UPSTREAM_BUDGET_MAX_CALLS", &cfg.Upstream.Budget.MaxCalls, fail)
	envInt("CACHE_MEMORY_MAX_ENTRIES", &cfg.Cache.Memory.MaxEntries, fail)
	envInt("CACHE_MEMORY_MAX_NEGATIVE_ENTRIES", &cfg.Cache.Memory.MaxNegativeEntries, fail)
	envInt("REDIS_DB", &cfg.Cache.Redis.DB, fail)
	envInt("VALIDATION_MAX_RESOURCE_ID", &cfg.Validation.MaxResourceID, fail)
	envInt("VALIDATION_MAX_USER_ID", &cfg.Validation.MaxUserID, fail)

	envDur("UPSTREAM_TIMEOUT", &cfg.Upstream.Timeout, fail)
	envDur("SERVER_REQUEST_TIMEOUT", &cfg.Server.RequestTimeout, fail)
	envDur("UPSTREAM_BUDGET_WINDOW", &cfg.Upstream.Budget.Window, fail)
	envDur("REDIS_LOCK_TTL", &cfg.Cache.Redis.Lock.TTL, fail)
	envDur("REDIS_LOCK_WAIT_TIMEOUT", &cfg.Cache.Redis.Lock.WaitTimeout, fail)

	// Per-route overrides: CACHE_ROUTE_POSTS_ITEM_ENABLED, ..._TTL, ..._NEGATIVE_TTL.
	// This is what lets caching be disabled for one route without a redeploy.
	for name, policy := range cfg.Cache.Routes {
		prefix := "CACHE_ROUTE_" + strings.ToUpper(name)
		if raw, ok := os.LookupEnv(prefix + "_ENABLED"); ok {
			v, err := strconv.ParseBool(raw)
			if err != nil {
				fail(prefix+"_ENABLED", err)
			} else {
				policy.Enabled = &v
			}
		}
		if raw, ok := os.LookupEnv(prefix + "_TTL"); ok {
			var d Duration
			if err := d.parse(raw); err != nil {
				fail(prefix+"_TTL", err)
			} else {
				policy.TTL = &d
			}
		}
		if raw, ok := os.LookupEnv(prefix + "_NEGATIVE_TTL"); ok {
			var d Duration
			if err := d.parse(raw); err != nil {
				fail(prefix+"_NEGATIVE_TTL", err)
			} else {
				policy.NegativeTTL = &d
			}
		}
		cfg.Cache.Routes[name] = policy
	}

	if len(errs) > 0 {
		return fmt.Errorf("invalid environment configuration: %s", strings.Join(errs, "; "))
	}
	return nil
}

func envStr(key string, dst *string) {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		*dst = v
	}
}

func envBool(key string, dst *bool, fail func(string, error)) {
	raw, ok := os.LookupEnv(key)
	if !ok {
		return
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		fail(key, err)
		return
	}
	*dst = v
}

func envInt(key string, dst *int, fail func(string, error)) {
	raw, ok := os.LookupEnv(key)
	if !ok {
		return
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		fail(key, err)
		return
	}
	*dst = v
}

func envDur(key string, dst *Duration, fail func(string, error)) {
	raw, ok := os.LookupEnv(key)
	if !ok {
		return
	}
	var d Duration
	if err := d.parse(raw); err != nil {
		fail(key, err)
		return
	}
	*dst = d
}

// Validate rejects configurations that would fail confusingly at runtime. It is
// deliberately strict: a server that refuses to start is easier to diagnose than
// one that starts and quietly serves wrong results.
func (c Config) Validate() error {
	var errs []string
	add := func(format string, args ...any) { errs = append(errs, fmt.Sprintf(format, args...)) }

	if c.Server.Addr == "" {
		add("server.addr must not be empty")
	}
	if !strings.HasPrefix(c.Upstream.BaseURL, "http://") && !strings.HasPrefix(c.Upstream.BaseURL, "https://") {
		add("upstream.base_url must be an absolute http(s) URL, got %q", c.Upstream.BaseURL)
	}
	if c.Upstream.MaxAttempts < 1 {
		add("upstream.max_attempts must be at least 1, got %d", c.Upstream.MaxAttempts)
	}
	if c.Upstream.Timeout <= 0 {
		add("upstream.timeout must be positive")
	}
	if c.Upstream.MaxResponseBytes <= 0 {
		add("upstream.max_response_bytes must be positive")
	}
	// A request deadline shorter than the retry sequence would cancel fetches
	// mid-retry and make the retry configuration a lie. The worst case is every
	// attempt timing out plus the backoff waited between them; omitting the
	// backoff would understate it and let an apparently valid config still cut
	// the final attempt short.
	attempts := time.Duration(c.Upstream.MaxAttempts)
	worstCase := time.Duration(c.Upstream.Timeout)*attempts +
		time.Duration(c.Upstream.RetryMaxDelay)*(attempts-1)
	if time.Duration(c.Server.RequestTimeout) < worstCase {
		add("server.request_timeout (%s) must cover the worst-case retry sequence "+
			"(upstream.timeout x max_attempts + retry_max_delay x (max_attempts-1) = %s)",
			c.Server.RequestTimeout, worstCase)
	}
	// The write deadline has to outlast a request or the server would cut off a
	// response the handler was still entitled to be producing.
	if c.Server.WriteTimeout > 0 && c.Server.WriteTimeout < c.Server.RequestTimeout {
		add("server.write_timeout (%s) must be at least server.request_timeout (%s)",
			c.Server.WriteTimeout, c.Server.RequestTimeout)
	}
	switch c.Cache.Backend {
	case "memory", "redis":
	default:
		add("cache.backend must be \"memory\" or \"redis\", got %q", c.Cache.Backend)
	}
	if c.Cache.Backend == "redis" && c.Cache.Redis.Addr == "" {
		add("cache.redis.addr is required when cache.backend is \"redis\"")
	}
	if c.Cache.MaxBodyBytes <= 0 {
		add("cache.max_body_bytes must be positive")
	}
	if c.Cache.Memory.MaxEntries <= 0 {
		add("cache.memory.max_entries must be positive")
	}
	if c.Cache.Memory.MaxNegativeEntries <= 0 {
		add("cache.memory.max_negative_entries must be positive")
	}
	switch c.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		add("log.level must be one of debug|info|warn|error, got %q", c.Log.Level)
	}
	switch c.Log.Format {
	case "json", "text":
	default:
		add("log.format must be \"json\" or \"text\", got %q", c.Log.Format)
	}
	if c.Validation.MaxResourceID <= 0 {
		add("validation.max_resource_id must be positive")
	}
	if c.Validation.MaxUserID <= 0 {
		add("validation.max_user_id must be positive")
	}
	if c.RateLimit.Enabled && c.RateLimit.RequestsPerSecond <= 0 {
		add("rate_limit.requests_per_second must be positive when rate limiting is enabled")
	}
	for _, name := range []string{RoutePostsList, RoutePostsItem, RouteTodosList, RouteTodosItem} {
		if _, ok := c.Cache.Routes[name]; !ok {
			add("cache.routes is missing required policy %q", name)
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("invalid configuration:\n  - %s", strings.Join(errs, "\n  - "))
	}
	return nil
}
