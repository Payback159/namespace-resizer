package lock

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	coordinationv1 "k8s.io/api/coordination/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const (
	testNamespace = "default"
	testQuotaName = "my-quota"
)

func TestLeaseLocker_Locking(t *testing.T) {
	g := NewWithT(t)

	// Setup
	scheme := runtime.NewScheme()
	_ = coordinationv1.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	locker := NewLeaseLocker(fakeClient)
	ctx := context.TODO()

	ns := testNamespace
	quota := testQuotaName
	prID := 123

	// 1. Test AcquireLock
	err := locker.AcquireLock(ctx, ns, quota, prID)
	g.Expect(err).ToNot(HaveOccurred())

	// 2. Verify the lock is held
	state, err := locker.GetState(ctx, ns, quota)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(state.PRID).To(Equal(prID))

	// 3. Release the lock
	err = locker.MutateState(ctx, ns, quota, func(s *State) {
		s.PRID = 0
	})
	g.Expect(err).ToNot(HaveOccurred())

	// 4. Verify Lock is gone (HolderIdentity is nil)
	state, err = locker.GetState(ctx, ns, quota)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(state.PRID).To(Equal(0))

	// 5. Verify Lease still exists (Persistent Lease)
	leaseName := "state-" + ns + "-" + quota
	var lease coordinationv1.Lease
	err = fakeClient.Get(ctx, client.ObjectKey{Name: leaseName, Namespace: ControllerNamespace}, &lease)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(lease.Spec.HolderIdentity).To(BeNil())
}
