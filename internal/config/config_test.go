package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDefaultsAreValid(t *testing.T) {
	t.Parallel()
	if err := Default().Validate(); err != nil {
		t.Fatalf("the default configuration must be usable as-is: %v", err)
	}
}

// TestDefaultBackendNeedsNoInfrastructure: the service must run with no file,
// no environment and no Redis.
func TestDefaultBackendNeedsNoInfrastructure(t *testing.T) {
	t.Parallel()
	if got := Default().Cache.Backend; got != "memory" {
		t.Errorf("default cache backend = %q, want \"memory\"", got)
	}
}

func TestLoadMissingFileFallsBackToDefaults(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "absent.yaml"))
	if err != nil {
		t.Fatalf("a missing config file must not be an error: %v", err)
	}
	if cfg.Server.Addr != Default().Server.Addr {
		t.Errorf("addr = %q, want the default", cfg.Server.Addr)
	}
}

func TestLoadRejectsUnknownFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("env: test\nnot_a_real_field: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected unknown fields to be rejected; a silently ignored typo is how misconfiguration hides")
	}
}

// TestDurationsMustBeStrings guards a specific foot-gun: yaml.v3 would decode a
// bare `ttl: 10` into 10 nanoseconds, which looks like the cache simply not
// working.
func TestDurationsMustBeStrings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("upstream:\n  timeout: 10\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected a bare numeric duration to be rejected")
	}
}

func TestRequestTimeoutMustCoverRetrySequence(t *testing.T) {
	t.Parallel()
	cfg := Default()
	cfg.Upstream.Timeout = Duration(5 * time.Second)
	cfg.Upstream.MaxAttempts = 3 // 15s worst case
	cfg.Server.RequestTimeout = Duration(2 * time.Second)

	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "request_timeout") {
		t.Fatalf("expected the retry budget to be validated against the request deadline, got %v", err)
	}
}

func TestRedisBackendRequiresAddress(t *testing.T) {
	t.Parallel()
	cfg := Default()
	cfg.Cache.Backend = "redis"
	cfg.Cache.Redis.Addr = ""

	if err := cfg.Validate(); err == nil {
		t.Fatal("expected redis backend without an address to be rejected")
	}
}

func TestEnvironmentOverridesScalars(t *testing.T) {
	t.Setenv("SERVER_ADDR", ":9999")
	t.Setenv("CACHE_ENABLED", "false")
	t.Setenv("UPSTREAM_TIMEOUT", "2s")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.Addr != ":9999" {
		t.Errorf("addr = %q, want :9999", cfg.Server.Addr)
	}
	if cfg.Cache.Enabled {
		t.Error("CACHE_ENABLED=false was not applied")
	}
	if cfg.Upstream.Timeout.Std() != 2*time.Second {
		t.Errorf("upstream timeout = %v, want 2s", cfg.Upstream.Timeout)
	}
}

// TestPerRouteEnvOverride is the "disable caching for one route without a
// redeploy" requirement.
func TestPerRouteEnvOverride(t *testing.T) {
	t.Setenv("CACHE_ROUTE_TODOS_LIST_ENABLED", "false")
	t.Setenv("CACHE_ROUTE_POSTS_ITEM_TTL", "30m")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	todos := cfg.Cache.Routes[RouteTodosList]
	if todos.Enabled == nil || *todos.Enabled {
		t.Error("todos_list should have been disabled by the environment")
	}
	posts := cfg.Cache.Routes[RoutePostsItem]
	if posts.TTL == nil || posts.TTL.Std() != 30*time.Minute {
		t.Errorf("posts_item ttl = %v, want 30m", posts.TTL)
	}
	// Untouched routes keep their configured values.
	if todosItem := cfg.Cache.Routes[RouteTodosItem]; todosItem.Enabled == nil || !*todosItem.Enabled {
		t.Error("todos_item should have been left enabled")
	}
}

func TestInvalidEnvValueIsReported(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "not-a-bool")
	if _, err := Load(""); err == nil {
		t.Fatal("expected an unparseable environment value to fail loudly")
	}
}

func TestShippedConfigFileIsValid(t *testing.T) {
	path := filepath.Join("..", "..", "deploy", "config.yaml")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("deploy/config.yaml not present: %v", err)
	}
	if _, err := Load(path); err != nil {
		t.Fatalf("the shipped config file must load cleanly: %v", err)
	}
}
