package lock

import (
	"context"
	"sync/atomic"
	"testing"

	. "github.com/onsi/gomega"
	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func leaseGR() schema.GroupResource {
	return schema.GroupResource{Group: coordinationv1.GroupName, Resource: "leases"}
}

// preCreateUnlockedLease inserts a persistent, unlocked lease so AcquireLock
// follows the update path rather than the create path.
func preCreateUnlockedLease(g *WithT, c client.Client, ns, quota string) {
	lease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "state-" + ns + "-" + quota,
			Namespace: ControllerNamespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "namespace-resizer",
			},
		},
	}
	g.Expect(c.Create(context.TODO(), lease)).To(Succeed())
}

// TestAcquireLock_RetriesOnUpdateConflict verifies that a transient optimistic
// concurrency conflict on Update is retried instead of failing the caller.
func TestAcquireLock_RetriesOnUpdateConflict(t *testing.T) {
	g := NewWithT(t)
	ctx := context.TODO()

	scheme := runtime.NewScheme()
	_ = coordinationv1.AddToScheme(scheme)

	var updateCalls int32
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				// Fail the first update with a conflict, succeed afterwards.
				if atomic.AddInt32(&updateCalls, 1) == 1 {
					return apierrors.NewConflict(leaseGR(), obj.GetName(), nil)
				}
				return c.Update(ctx, obj, opts...)
			},
		}).
		Build()

	preCreateUnlockedLease(g, fakeClient, testNamespace, testQuotaName)

	locker := NewLeaseLocker(fakeClient)
	err := locker.AcquireLock(ctx, testNamespace, testQuotaName, 99)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(atomic.LoadInt32(&updateCalls)).To(BeNumerically(">=", 2), "update should have been retried")

	state, err := locker.GetState(ctx, testNamespace, testQuotaName)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(state.PRID).To(Equal(99))
}

// TestAcquireLock_RetriesOnAlreadyExists verifies that losing the create race
// (Get says NotFound but Create reports AlreadyExists) is converted to a
// conflict and retried, eventually acquiring the lock via update.
func TestAcquireLock_RetriesOnAlreadyExists(t *testing.T) {
	g := NewWithT(t)
	ctx := context.TODO()

	scheme := runtime.NewScheme()
	_ = coordinationv1.AddToScheme(scheme)

	// Lease actually exists (unlocked), but we trick the first Get into
	// reporting NotFound so the code takes the create branch and hits
	// AlreadyExists from the real store.
	var getCalls int32
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if atomic.AddInt32(&getCalls, 1) == 1 {
					return apierrors.NewNotFound(leaseGR(), key.Name)
				}
				return c.Get(ctx, key, obj, opts...)
			},
		}).
		Build()

	preCreateUnlockedLease(g, fakeClient, testNamespace, testQuotaName)

	locker := NewLeaseLocker(fakeClient)
	err := locker.AcquireLock(ctx, testNamespace, testQuotaName, 7)
	g.Expect(err).ToNot(HaveOccurred())

	state, err := locker.GetState(ctx, testNamespace, testQuotaName)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(state.PRID).To(Equal(7))
}

// TestAcquireLock_RejectsForeignHolder verifies a lease already held by a
// different PR is reported as an error (not silently overwritten).
func TestAcquireLock_RejectsForeignHolder(t *testing.T) {
	g := NewWithT(t)
	ctx := context.TODO()

	scheme := runtime.NewScheme()
	_ = coordinationv1.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	locker := NewLeaseLocker(fakeClient)
	g.Expect(locker.AcquireLock(ctx, testNamespace, testQuotaName, 100)).To(Succeed())

	// Acquiring for a different PR while held must fail.
	err := locker.AcquireLock(ctx, testNamespace, testQuotaName, 200)
	g.Expect(err).To(HaveOccurred())

	// Re-acquiring for the SAME PR is idempotent.
	g.Expect(locker.AcquireLock(ctx, testNamespace, testQuotaName, 100)).To(Succeed())
}
