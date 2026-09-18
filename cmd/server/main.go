// Command server runs the caching proxy.
//
// It is a thin composition root: it builds the dependency graph, starts the HTTP
// server, and shuts it down cleanly. All behaviour lives in internal packages so
// it can be tested without starting a process.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/lemonn/typecode-middleware/internal/cache"
	"github.com/lemonn/typecode-middleware/internal/cachemw"
	"github.com/lemonn/typecode-middleware/internal/config"
	"github.com/lemonn/typecode-middleware/internal/lock"
	"github.com/lemonn/typecode-middleware/internal/service"
	"github.com/lemonn/typecode-middleware/internal/transport/httpapi"
	"github.com/lemonn/typecode-middleware/internal/upstream"
)

func main() {
	if err := run(); err != nil {
		// The logger may not exist yet when configuration fails, so startup
		// errors go to stderr directly.
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", "deploy/config.yaml",
		"path to the YAML configuration file; missing is not an error, defaults apply")
	healthcheck := flag.Bool("healthcheck", false,
		"probe this instance's /healthz and exit; lets a distroless image check itself")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}

	// Runs as a separate, short-lived invocation of the same binary. The
	// container image has no shell and no curl, so the server ships its own
	// probe rather than the image carrying tools it would otherwise never need.
	if *healthcheck {
		return probeHealth(cfg.Server.Addr)
	}
	log := newLogger(cfg.Log)

	up, err := upstream.New(cfg.Upstream, log)
	if err != nil {
		return err
	}
	svc := service.New(up, log)

	backend, locker, err := buildCache(cfg, log)
	if err != nil {
		return err
	}
	defer func() {
		if err := backend.Close(); err != nil {
			log.Error("failed to close cache backend", slog.String("error", err.Error()))
		}
	}()

	cacheMW := cachemw.New(backend, locker, cachemw.Options{
		Enabled:      cfg.Cache.Enabled,
		MaxBodyBytes: cfg.Cache.MaxBodyBytes,
		StoreHeaders: cfg.Cache.StoreHeaders,
		DebugHeaders: cfg.Cache.DebugHeaders,
		// A fill outlives the request that triggered it, so it gets the whole
		// request budget rather than whatever is left of one caller's deadline.
		FetchTimeout:     cfg.Server.RequestTimeout.Std(),
		LockWaitTimeout:  cfg.Cache.Redis.Lock.WaitTimeout.Std(),
		LockPollInterval: cfg.Cache.Redis.Lock.PollInterval.Std(),
	}, log)

	api := httpapi.NewServer(cfg, log, svc, cacheMW, backend, up)

	httpServer := &http.Server{
		Addr:              cfg.Server.Addr,
		Handler:           api.Routes(),
		ReadHeaderTimeout: cfg.Server.ReadHeaderTimeout.Std(),
		ReadTimeout:       cfg.Server.ReadTimeout.Std(),
		WriteTimeout:      cfg.Server.WriteTimeout.Std(),
		IdleTimeout:       cfg.Server.IdleTimeout.Std(),
		MaxHeaderBytes:    cfg.Server.MaxHeaderBytes,
	}

	log.Info("starting caching proxy",
		slog.String("addr", cfg.Server.Addr),
		slog.String("env", cfg.Env),
		slog.String("upstream", cfg.Upstream.BaseURL),
		slog.String("cache_backend", backend.Name()),
		slog.String("lock", locker.Name()),
		slog.Bool("cache_enabled", cfg.Cache.Enabled))

	return serve(httpServer, cfg, log, cacheMW, up)
}

// serve runs the server until interrupted, then drains in-flight requests.
func serve(
	httpServer *http.Server,
	cfg config.Config,
	log *slog.Logger,
	cacheMW *cachemw.Middleware,
	up *upstream.Client,
) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	serverErr := make(chan error, 1)
	go func() {
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
			return
		}
		serverErr <- nil
	}()

	select {
	case err := <-serverErr:
		return err
	case <-ctx.Done():
		log.Info("shutdown signal received; draining in-flight requests")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout.Std())
	defer cancel()

	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("graceful shutdown failed: %w", err)
	}

	// The final tally is the point of the whole service, so it is worth one line
	// on the way out.
	stats := cacheMW.Stats()
	log.Info("shutdown complete",
		slog.Uint64("cache_hits", stats.Hits),
		slog.Uint64("cache_misses", stats.Misses),
		slog.Float64("hit_ratio", stats.HitRatio),
		slog.Int64("upstream_calls", up.Calls()))
	return nil
}

// buildCache assembles the cache tiers.
//
// The memory tier is always present: the whole upstream dataset is about 51 KB,
// so a single process can hold all of it and needs nothing else. Redis is added
// only when configured, and its job is coordination between replicas rather than
// capacity — without it, N instances each make their own first call for the same
// key.
func buildCache(cfg config.Config, log *slog.Logger) (cache.Cache, lock.Locker, error) {
	memory := cache.NewMemory(cache.MemoryOptions{
		MaxEntries:         cfg.Cache.Memory.MaxEntries,
		MaxNegativeEntries: cfg.Cache.Memory.MaxNegativeEntries,
		MaxBytes:           cfg.Cache.Memory.MaxBytes,
		JanitorInterval:    cfg.Cache.Memory.JanitorInterval.Std(),
	})

	if cfg.Cache.Backend != "redis" {
		// singleflight alone gives the exactly-once guarantee within a process,
		// so a single instance needs no lock at all.
		return memory, lock.NoOp{}, nil
	}

	rc := cfg.Cache.Redis
	shared := cache.NewRedis(cache.RedisOptions{
		Addr:         rc.Addr,
		Password:     rc.Password,
		DB:           rc.DB,
		KeyPrefix:    rc.KeyPrefix,
		DialTimeout:  rc.DialTimeout.Std(),
		ReadTimeout:  rc.ReadTimeout.Std(),
		WriteTimeout: rc.WriteTimeout.Std(),
		PoolSize:     rc.PoolSize,
	})

	// Fail fast rather than silently degrading. The operator explicitly asked for
	// the redis backend; starting up pretending to have it would hide the
	// problem until the bill arrived.
	pingCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := shared.Ping(pingCtx); err != nil {
		_ = shared.Close()
		_ = memory.Close()
		return nil, nil, fmt.Errorf("cache backend is configured as redis but is unreachable: %w", err)
	}

	var locker lock.Locker = lock.NoOp{}
	if rc.Lock.Enabled {
		locker = lock.NewRedis(shared.Client(), rc.KeyPrefix, rc.Lock.TTL.Std(), log)
	}
	return cache.NewTiered(memory, shared, log), locker, nil
}

// probeHealth performs a one-shot liveness check against a locally running
// instance and reports the result as the process exit status.
func probeHealth(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("parse server address %q: %w", addr, err)
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + net.JoinHostPort(host, port) + "/healthz")
	if err != nil {
		return fmt.Errorf("healthcheck request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthcheck returned status %d", resp.StatusCode)
	}
	return nil
}

func newLogger(cfg config.Log) *slog.Logger {
	var level slog.Level
	switch cfg.Level {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{Level: level}
	if cfg.Format == "text" {
		return slog.New(slog.NewTextHandler(os.Stdout, opts))
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, opts))
}
