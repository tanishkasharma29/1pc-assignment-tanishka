// Package lock provides cross-instance mutual exclusion for cache fills.
//
// Within one process, singleflight already guarantees that only one goroutine
// fetches a given key. That guarantee stops at the process boundary: N replicas
// with cold caches will each elect their own leader and each make a billable
// call. This package closes that gap.
package lock

import "context"

// Release relinquishes a held lock. It is always safe to call, including after
// the lock has expired, and takes its own context because the request context
// may already be done by the time the lock is released.
type Release func(context.Context)

// Locker acquires short-lived named locks.
type Locker interface {
	// Acquire attempts to take the lock without blocking. It returns
	// acquired=false when another holder has it, which is a normal outcome and
	// not an error.
	Acquire(ctx context.Context, key string) (Release, bool, error)
	Name() string
}

// NoOp always grants the lock. It is the correct implementation for a
// single-instance deployment, where singleflight alone is already sufficient and
// a lock would add latency for no benefit.
type NoOp struct{}

func (NoOp) Name() string { return "noop" }

func (NoOp) Acquire(context.Context, string) (Release, bool, error) {
	return func(context.Context) {}, true, nil
}
