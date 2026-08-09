package lock

import (
	"context"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newStateLocker() (*LeaseLocker, client.Client) {
	scheme := runtime.NewScheme()
	_ = coordinationv1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	return NewLeaseLocker(c), c
}

func TestGetState_MissingLeaseIsZero(t *testing.T) {
	g := NewWithT(t)
	locker, _ := newStateLocker()

	state, err := locker.GetState(context.Background(), testNamespace, testQuotaName)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(state.PRID).To(Equal(0))
	g.Expect(state.PRDirection).To(BeEmpty())
	g.Expect(state.Window).To(BeEmpty())
	g.Expect(state.LastGrow.IsZero()).To(BeTrue())
}

func TestMutateState_RoundTrip(t *testing.T) {
	g := NewWithT(t)
	locker, _ := newStateLocker()
	ctx := context.Background()
	modifiedAt := time.Date(2026, 7, 30, 8, 0, 0, 0, time.UTC)
	grownAt := time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)
	shrunkAt := time.Date(2026, 8, 5, 14, 30, 0, 0, time.UTC)

	err := locker.MutateState(ctx, testNamespace, testQuotaName, func(s *State) {
		s.PRID = 42
		s.PRDirection = "shrink"
		s.LastModified = modifiedAt
		s.LastGrow = grownAt
		s.LastShrink = shrunkAt
		s.Window = `{"v":1,"days":[]}`
	})
	g.Expect(err).NotTo(HaveOccurred())

	state, err := locker.GetState(ctx, testNamespace, testQuotaName)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(state.PRID).To(Equal(42))
	g.Expect(state.PRDirection).To(Equal("shrink"))
	g.Expect(state.LastModified.Equal(modifiedAt)).To(BeTrue())
	g.Expect(state.LastGrow.Equal(grownAt)).To(BeTrue())
	g.Expect(state.LastShrink.Equal(shrunkAt)).To(BeTrue())
	g.Expect(state.Window).To(Equal(`{"v":1,"days":[]}`))
}

func TestMutateState_ClearingPRIDReleasesTheLock(t *testing.T) {
	g := NewWithT(t)
	locker, c := newStateLocker()
	ctx := context.Background()

	g.Expect(locker.MutateState(ctx, testNamespace, testQuotaName, func(s *State) {
		s.PRID = 7
		s.PRDirection = "grow"
	})).To(Succeed())

	g.Expect(locker.MutateState(ctx, testNamespace, testQuotaName, func(s *State) {
		s.PRID = 0
		s.PRDirection = ""
		s.LastShrink = time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	})).To(Succeed())

	state, err := locker.GetState(ctx, testNamespace, testQuotaName)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(state.PRID).To(Equal(0))
	g.Expect(state.PRDirection).To(BeEmpty())
	g.Expect(state.LastShrink.IsZero()).To(BeFalse())

	// Verify that the pr-direction annotation key is actually deleted, not just empty.
	var lease coordinationv1.Lease
	key := client.ObjectKey{
		Name:      "state-" + testNamespace + "-" + testQuotaName,
		Namespace: ControllerNamespace,
	}
	g.Expect(c.Get(ctx, key, &lease)).To(Succeed())
	g.Expect(lease.Annotations).NotTo(HaveKey(AnnotationPRDirection))
}

func TestMutateState_PreservesExistingLastModified(t *testing.T) {
	g := NewWithT(t)
	locker, _ := newStateLocker()
	ctx := context.Background()
	modified := time.Date(2026, 7, 30, 8, 0, 0, 0, time.UTC)

	g.Expect(locker.MutateState(ctx, testNamespace, testQuotaName, func(s *State) {
		s.LastModified = modified
	})).To(Succeed())

	g.Expect(locker.MutateState(ctx, testNamespace, testQuotaName, func(s *State) {
		s.Window = "{}"
	})).To(Succeed())

	state, err := locker.GetState(ctx, testNamespace, testQuotaName)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(state.LastModified.Equal(modified)).To(BeTrue())
}

// TestGetState_ParsesNonUTCOffsetStamp verifies that a stamp already on disk
// in a non-UTC offset — what a lease written before setStamp normalised to
// UTC would carry, or any other writer that didn't — is still parsed back to
// the correct instant. setStamp itself always writes UTC; this covers the
// read side finding something it did not write.
func TestGetState_ParsesNonUTCOffsetStamp(t *testing.T) {
	g := NewWithT(t)
	locker, c := newStateLocker()
	ctx := context.Background()

	written := time.Date(2026, 7, 30, 10, 0, 0, 0, time.FixedZone("CEST", 2*60*60))

	lease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "state-" + testNamespace + "-" + testQuotaName,
			Namespace: ControllerNamespace,
			Annotations: map[string]string{
				AnnotationLastModified: written.Format(time.RFC3339),
			},
		},
	}
	g.Expect(c.Create(ctx, lease)).To(Succeed())

	state, err := locker.GetState(ctx, testNamespace, testQuotaName)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(state.LastModified.Equal(written)).To(BeTrue())
}
