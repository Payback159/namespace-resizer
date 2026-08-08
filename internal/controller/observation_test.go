package controller

import (
	"context"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	"github.com/payback159/namespace-resizer/internal/lock"
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
