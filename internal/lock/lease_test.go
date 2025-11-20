package lock

import (
	"context"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	coordinationv1 "k8s.io/api/coordination/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestLeaseLocker_Locking(t *testing.T) {
	g := NewWithT(t)

	// Setup
	scheme := runtime.NewScheme()
	_ = coordinationv1.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	locker := NewLeaseLocker(fakeClient)
	ctx := context.TODO()

	ns := "default"
	quota := "my-quota"
	prID := 123

	// 1. Test AcquireLock
	err := locker.AcquireLock(ctx, ns, quota, prID)
	g.Expect(err).ToNot(HaveOccurred())

	// 2. Test GetLock
	id, err := locker.GetLock(ctx, ns, quota)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(id).To(Equal(prID))

	// 3. Test ReleaseLock
	err = locker.ReleaseLock(ctx, ns, quota)
	g.Expect(err).ToNot(HaveOccurred())

	// 4. Verify Lock is gone (HolderIdentity is nil)
	id, err = locker.GetLock(ctx, ns, quota)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(id).To(Equal(0))

	// 5. Verify Lease still exists (Persistent Lease)
	leaseName := "state-" + ns + "-" + quota
	var lease coordinationv1.Lease
	err = fakeClient.Get(ctx, client.ObjectKey{Name: leaseName, Namespace: ControllerNamespace}, &lease)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(lease.Spec.HolderIdentity).To(BeNil())
}

func TestLeaseLocker_LastModified_Cooldown(t *testing.T) {
	g := NewWithT(t)

	// Setup
	scheme := runtime.NewScheme()
	_ = coordinationv1.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	locker := NewLeaseLocker(fakeClient)
	ctx := context.TODO()

	ns := "default"
	quota := "my-quota"
	duration := 1 * time.Hour

	// 1. Check Cooldown (Should be false initially)
	active, err := locker.CheckCooldown(ctx, ns, quota, duration)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(active).To(BeFalse())

	// 2. Set LastModified (starts cooldown)
	now := time.Now()
	err = locker.SetLastModified(ctx, ns, quota, now)
	g.Expect(err).ToNot(HaveOccurred())

	// 3. Check Cooldown (Should be true now)
	active, err = locker.CheckCooldown(ctx, ns, quota, duration)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(active).To(BeTrue())

	// 4. Simulate Expiry
	// We need to manually modify the lease annotation in the fake client
	leaseName := "state-" + ns + "-" + quota
	var lease coordinationv1.Lease
	err = fakeClient.Get(ctx, client.ObjectKey{Name: leaseName, Namespace: ControllerNamespace}, &lease)
	g.Expect(err).ToNot(HaveOccurred())

	// Set time to 2 hours ago
	past := now.Add(-2 * time.Hour)
	lease.Annotations[AnnotationLastModified] = past.Format(time.RFC3339)
	err = fakeClient.Update(ctx, &lease)
	g.Expect(err).ToNot(HaveOccurred())

	// 5. Check Cooldown (Should be false)
	active, err = locker.CheckCooldown(ctx, ns, quota, duration)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(active).To(BeFalse())

	// Verify Lease still exists
	err = fakeClient.Get(ctx, client.ObjectKey{Name: leaseName, Namespace: ControllerNamespace}, &lease)
	g.Expect(err).ToNot(HaveOccurred())
}
