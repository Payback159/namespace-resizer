package controller

import (
	"context"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/payback159/namespace-resizer/internal/lock"
	"github.com/payback159/namespace-resizer/internal/sizing"
)

// observationHeartbeat is the longest a sample may go unpersisted. It bounds
// how much history a controller crash can lose, and therefore how stale the
// lastSampleAt fallback can be after a restart.
const observationHeartbeat = time.Hour

// Observer folds status.used into a rolling window and persists it on the state
// Lease. It keeps a write-behind cache: every reconcile updates the in-memory
// window, but the Lease is written only when a peak rises, a day rolls over, or
// the heartbeat elapses. Without the cache the persisted lastSampleAt would lag
// a full heartbeat behind, and every sample would look like an hour-long gap.
type Observer struct {
	locker *lock.LeaseLocker
	now    func() time.Time

	// mu protects only the cached map, never spanning API calls. controller-runtime
	// never reconciles the same object concurrently, so two Observe calls for the
	// same key cannot overlap; only the map entry (key) itself needs protection, not
	// reads or writes to the window's content. Different keys require only the map's
	// internal synchronization to be safe.
	mu     sync.Mutex
	cached map[string]sizing.Window
}

// NewObserver builds an Observer. now is injected so tests can drive the clock.
func NewObserver(locker *lock.LeaseLocker, now func() time.Time) *Observer {
	if now == nil {
		now = time.Now
	}
	return &Observer{
		locker: locker,
		now:    now,
		cached: map[string]sizing.Window{},
	}
}

// Observe records one sample and returns the current window. A cold cache — a
// freshly started controller — falls back to the persisted window, so the first
// sample after a restart correctly measures the outage as a gap. The returned
// Window shares its backing slice and maps with the observer's cached copy and
// must be treated as a read-only snapshot.
func (o *Observer) Observe(
	ctx context.Context,
	quota *corev1.ResourceQuota,
	windowDays int,
) (sizing.Window, error) {
	key := quota.Namespace + "/" + quota.Name

	// Read from cache, releasing the lock before API calls.
	o.mu.Lock()
	window, cached := o.cached[key]
	o.mu.Unlock()

	if !cached {
		state, err := o.locker.GetState(ctx, quota.Namespace, quota.Name)
		if err != nil {
			return sizing.Window{}, err
		}
		window = sizing.DecodeWindow(state.Window)
	}

	now := o.now()
	changed := window.Observe(now, string(quota.UID), quota.Status.Used, windowDays)

	if changed || o.heartbeatElapsed(window, now) {
		window.LastWriteAt = now.UTC().Format(time.RFC3339)
		encoded, err := sizing.EncodeWindow(window)
		if err != nil {
			return sizing.Window{}, err
		}
		err = o.locker.MutateState(ctx, quota.Namespace, quota.Name, func(s *lock.State) {
			s.Window = encoded
		})
		if err != nil {
			return sizing.Window{}, err
		}
	}

	// Cache the updated window.
	o.mu.Lock()
	o.cached[key] = window
	o.mu.Unlock()

	return window, nil
}

// Forget drops the cached window for a quota, so the next Observe reloads it
// from the Lease. Used when the reconciler sees the quota disappear.
func (o *Observer) Forget(namespace, name string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.cached, namespace+"/"+name)
}

func (o *Observer) heartbeatElapsed(window sizing.Window, now time.Time) bool {
	if window.LastWriteAt == "" {
		return true
	}
	last, err := time.Parse(time.RFC3339, window.LastWriteAt)
	if err != nil {
		return true
	}
	return now.Sub(last) >= observationHeartbeat
}
