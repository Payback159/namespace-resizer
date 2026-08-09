package controller

import (
	"context"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	"github.com/payback159/namespace-resizer/internal/lock"
	"github.com/payback159/namespace-resizer/internal/sizing"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func observedQuota(usedCPU string) *corev1.ResourceQuota {
	return &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "compute",
			Namespace: "team-a",
			UID:       types.UID("uid-1"),
		},
		Status: corev1.ResourceQuotaStatus{
			Hard: corev1.ResourceList{
				corev1.ResourceRequestsCPU: resource.MustParse("16"),
			},
			Used: corev1.ResourceList{
				corev1.ResourceRequestsCPU: resource.MustParse(usedCPU),
			},
		},
	}
}

func newObserverHarness() (*Observer, *lock.LeaseLocker, *time.Time) {
	scheme := runtime.NewScheme()
	_ = coordinationv1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	locker := lock.NewLeaseLocker(c)

	now := time.Date(2026, 8, 8, 0, 0, 0, 0, time.UTC)
	clock := &now
	return NewObserver(locker, func() time.Time { return *clock }), locker, clock
}

func TestObserver_PersistsFirstSample(t *testing.T) {
	g := NewWithT(t)
	observer, locker, _ := newObserverHarness()
	ctx := context.Background()

	window, err := observer.Observe(ctx, observedQuota("4"), 14)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(window.Days).To(HaveLen(1))

	state, err := locker.GetState(ctx, "team-a", "compute")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(state.Window).NotTo(BeEmpty())
}

func TestObserver_SkipsWriteWhenNothingChanged(t *testing.T) {
	g := NewWithT(t)
	observer, locker, clock := newObserverHarness()
	ctx := context.Background()
	quota := observedQuota("4")

	_, err := observer.Observe(ctx, quota, 14)
	g.Expect(err).NotTo(HaveOccurred())

	before, err := locker.GetState(ctx, "team-a", "compute")
	g.Expect(err).NotTo(HaveOccurred())

	// Five minutes later, same usage: no new peak, no day roll-over, and the
	// hourly heartbeat has not elapsed.
	*clock = clock.Add(5 * time.Minute)
	_, err = observer.Observe(ctx, quota, 14)
	g.Expect(err).NotTo(HaveOccurred())

	after, err := locker.GetState(ctx, "team-a", "compute")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(after.Window).To(Equal(before.Window))
}

func TestObserver_WritesOnHeartbeat(t *testing.T) {
	g := NewWithT(t)
	observer, locker, clock := newObserverHarness()
	ctx := context.Background()
	quota := observedQuota("4")

	_, err := observer.Observe(ctx, quota, 14)
	g.Expect(err).NotTo(HaveOccurred())
	before, err := locker.GetState(ctx, "team-a", "compute")
	g.Expect(err).NotTo(HaveOccurred())

	*clock = clock.Add(90 * time.Minute)
	_, err = observer.Observe(ctx, quota, 14)
	g.Expect(err).NotTo(HaveOccurred())

	after, err := locker.GetState(ctx, "team-a", "compute")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(after.Window).NotTo(Equal(before.Window))
}

func TestObserver_TracksRisingPeak(t *testing.T) {
	g := NewWithT(t)
	observer, _, clock := newObserverHarness()
	ctx := context.Background()

	_, err := observer.Observe(ctx, observedQuota("4"), 14)
	g.Expect(err).NotTo(HaveOccurred())

	*clock = clock.Add(10 * time.Minute)
	window, err := observer.Observe(ctx, observedQuota("9"), 14)
	g.Expect(err).NotTo(HaveOccurred())

	peaks := window.Days[0].Peaks
	g.Expect(peaks).To(HaveKeyWithValue(string(corev1.ResourceRequestsCPU), "9"))
}

// TestObserver_ForgetForcesReloadFromLease verifies the cache-eviction path
// Forget exists for: once forgotten, the next Observe must read the Lease
// again instead of continuing to serve the in-memory window, even though
// nothing about the sample itself changed.
func TestObserver_ForgetForcesReloadFromLease(t *testing.T) {
	g := NewWithT(t)
	observer, locker, clock := newObserverHarness()
	ctx := context.Background()
	quota := observedQuota("4")

	// Prime the cache with a single day.
	_, err := observer.Observe(ctx, quota, 14)
	g.Expect(err).NotTo(HaveOccurred())

	// Overwrite the Lease behind the observer's back, as if the state had
	// moved on without going through this cache -- the general case Forget
	// has to handle, not just the "quota deleted" one. Both extra days sit
	// inside the 14-day window so the next Observe's prune keeps them.
	mutated := sizing.Window{
		Version: sizing.WindowVersion,
		UID:     "uid-1",
		Days: []sizing.DayBucket{
			{Date: "2026-08-06", N: 1, First: "12:00", Last: "12:00",
				Peaks: map[string]string{"requests.cpu": "1"}},
			{Date: "2026-08-07", N: 1, First: "12:00", Last: "12:00",
				Peaks: map[string]string{"requests.cpu": "1"}},
		},
	}
	encoded, err := sizing.EncodeWindow(mutated)
	g.Expect(err).NotTo(HaveOccurred())
	err = locker.MutateState(ctx, "team-a", "compute", func(s *lock.State) {
		s.Window = encoded
	})
	g.Expect(err).NotTo(HaveOccurred())

	// Without Forget, the cache still wins: same day, same usage, nothing
	// about the mutated Lease is visible yet.
	*clock = clock.Add(time.Minute)
	stale, err := observer.Observe(ctx, quota, 14)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(stale.Days).To(HaveLen(1),
		"the cache must still be serving the pre-mutation window")

	observer.Forget("team-a", "compute")

	*clock = clock.Add(time.Minute)
	fresh, err := observer.Observe(ctx, quota, 14)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(fresh.Days).To(HaveLen(3),
		"Forget must force the next Observe to reload from the Lease")
}

func TestObserver_ReloadsFromLeaseOnColdCache(t *testing.T) {
	g := NewWithT(t)
	observer, locker, clock := newObserverHarness()
	ctx := context.Background()

	_, err := observer.Observe(ctx, observedQuota("4"), 14)
	g.Expect(err).NotTo(HaveOccurred())

	// A fresh Observer stands in for a restarted controller.
	restarted := NewObserver(locker, func() time.Time { return *clock })
	*clock = clock.Add(20 * time.Minute)

	window, err := restarted.Observe(ctx, observedQuota("4"), 14)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(window.Days).To(HaveLen(1))
	g.Expect(window.Days[0].N).To(BeNumerically(">=", 2))
}
