package lock

import (
	"context"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestLeaseGarbageCollector_Cleanup(t *testing.T) {
	g := NewWithT(t)

	// Setup Scheme
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = coordinationv1.AddToScheme(scheme)

	// 1. Setup Objects
	// Namespace "active" exists
	nsActive := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "active",
		},
	}

	// Lease for "active" (Should be kept)
	leaseActive := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "state-active-quota",
			Namespace: ControllerNamespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "namespace-resizer",
				"resizer.io/target-ns":         "active",
			},
		},
	}

	// Lease for "deleted" (Should be removed, as NS "deleted" does not exist)
	leaseDeleted := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "state-deleted-quota",
			Namespace: ControllerNamespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "namespace-resizer",
				"resizer.io/target-ns":         "deleted",
			},
		},
	}

	// Lease for "other" (Should be ignored, not managed by us)
	leaseOther := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "other-lease",
			Namespace: ControllerNamespace,
			Labels: map[string]string{
				"resizer.io/target-ns": "deleted", // Even if target is gone, we don't touch it if not managed by us
			},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(nsActive, leaseActive, leaseDeleted, leaseOther).
		Build()

	gc := NewLeaseGarbageCollector(fakeClient, 1*time.Hour)

	// 2. Run Cleanup
	err := gc.cleanup(context.TODO())
	g.Expect(err).ToNot(HaveOccurred())

	// 3. Verify
	// Active Lease should exist
	err = fakeClient.Get(context.TODO(), client.ObjectKeyFromObject(leaseActive), &coordinationv1.Lease{})
	g.Expect(err).ToNot(HaveOccurred())

	// Deleted Lease should be gone
	err = fakeClient.Get(context.TODO(), client.ObjectKeyFromObject(leaseDeleted), &coordinationv1.Lease{})
	g.Expect(errors.IsNotFound(err)).To(BeTrue())

	// Other Lease should exist
	err = fakeClient.Get(context.TODO(), client.ObjectKeyFromObject(leaseOther), &coordinationv1.Lease{})
	g.Expect(err).ToNot(HaveOccurred())
}
