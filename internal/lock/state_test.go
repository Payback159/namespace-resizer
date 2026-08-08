package lock

import (
	"context"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	coordinationv1 "k8s.io/api/coordination/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newStateLocker() *LeaseLocker {
	scheme := runtime.NewScheme()
	_ = coordinationv1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	return NewLeaseLocker(c)
}

func TestGetState_MissingLeaseIsZero(t *testing.T) {
	g := NewWithT(t)
	locker := newStateLocker()

	state, err := locker.GetState(context.Background(), testNamespace, testQuotaName)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(state.PRID).To(Equal(0))
	g.Expect(state.PRDirection).To(BeEmpty())
	g.Expect(state.Window).To(BeEmpty())
	g.Expect(state.LastGrow.IsZero()).To(BeTrue())
}

func TestMutateState_RoundTrip(t *testing.T) {
	g := NewWithT(t)
	locker := newStateLocker()
	ctx := context.Background()
	grownAt := time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)

	err := locker.MutateState(ctx, testNamespace, testQuotaName, func(s *State) {
		s.PRID = 42
		s.PRDirection = "shrink"
		s.LastGrow = grownAt
		s.Window = `{"v":1,"days":[]}`
	})
	g.Expect(err).NotTo(HaveOccurred())

	state, err := locker.GetState(ctx, testNamespace, testQuotaName)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(state.PRID).To(Equal(42))
	g.Expect(state.PRDirection).To(Equal("shrink"))
	g.Expect(state.LastGrow.Equal(grownAt)).To(BeTrue())
	g.Expect(state.Window).To(Equal(`{"v":1,"days":[]}`))
}

func TestMutateState_ClearingPRIDReleasesTheLock(t *testing.T) {
	g := NewWithT(t)
	locker := newStateLocker()
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
}

func TestMutateState_PreservesExistingLastModified(t *testing.T) {
	g := NewWithT(t)
	locker := newStateLocker()
	ctx := context.Background()
	modified := time.Date(2026, 7, 30, 8, 0, 0, 0, time.UTC)

	g.Expect(locker.SetLastModified(ctx, testNamespace, testQuotaName, modified)).To(Succeed())

	g.Expect(locker.MutateState(ctx, testNamespace, testQuotaName, func(s *State) {
		s.Window = "{}"
	})).To(Succeed())

	state, err := locker.GetState(ctx, testNamespace, testQuotaName)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(state.LastModified.Equal(modified)).To(BeTrue())
}
