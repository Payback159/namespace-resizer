package lock

import (
	"context"
	"fmt"

	coordinationv1 "k8s.io/api/coordination/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// The namespace where the controller runs and stores leases
	ControllerNamespace = "namespace-resizer-system"
	// AnnotationLastModified stores the timestamp of the last successful resize action
	AnnotationLastModified = "resizer.io/last-modified"
	// AnnotationLastGrow stores when the controller last proposed a growth.
	AnnotationLastGrow = "resizer.io/last-grow"
	// AnnotationLastShrink stores when the controller last proposed, closed or
	// expired a shrink. It drives the shrink cooldown gate.
	AnnotationLastShrink = "resizer.io/last-shrink"
	// AnnotationPRDirection records whether the open PR grows or shrinks.
	AnnotationPRDirection = "resizer.io/pr-direction"
	// AnnotationWindow stores the JSON-encoded observation window.
	AnnotationWindow = "resizer.io/observation-window"

	// Lease label keys/value used to identify resizer-managed state leases.
	labelManagedBy = "app.kubernetes.io/managed-by"
	labelTargetNS  = "resizer.io/target-ns"
	labelQuota     = "resizer.io/quota"
	managedByValue = "namespace-resizer"
)

// managedLeaseLabels returns the labels applied to every resizer-managed lease.
func managedLeaseLabels(targetNS, quotaName string) map[string]string {
	return map[string]string{
		labelTargetNS:  targetNS,
		labelQuota:     quotaName,
		labelManagedBy: managedByValue,
	}
}

type LeaseLocker struct {
	client client.Client
}

func NewLeaseLocker(c client.Client) *LeaseLocker {
	return &LeaseLocker{client: c}
}

func (l *LeaseLocker) getLeaseName(targetNS, quotaName string) string {
	// Using "state-" prefix as per architecture doc for persistent leases
	return fmt.Sprintf("state-%s-%s", targetNS, quotaName)
}

func (l *LeaseLocker) AcquireLock(ctx context.Context, targetNS, quotaName string, prID int) error {
	leaseName := l.getLeaseName(targetNS, quotaName)
	identity := fmt.Sprintf("pr-%d", prID)

	// The lease is a read-modify-write resource, so a concurrent writer (GC,
	// controller restart, or a brief leader-election overlap) can change the
	// object between our Get and Create/Update. Retry on conflicts so the
	// acquisition is robust instead of bubbling a transient error up to the
	// reconcile loop (which could otherwise lead to a duplicate PR).
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var lease coordinationv1.Lease
		err := l.client.Get(ctx, client.ObjectKey{Name: leaseName, Namespace: ControllerNamespace}, &lease)

		if errors.IsNotFound(err) {
			newLease := coordinationv1.Lease{
				ObjectMeta: metav1.ObjectMeta{
					Name:      leaseName,
					Namespace: ControllerNamespace,
					Labels:    managedLeaseLabels(targetNS, quotaName),
				},
				Spec: coordinationv1.LeaseSpec{
					HolderIdentity: &identity,
					AcquireTime:    &metav1.MicroTime{Time: metav1.Now().Time},
				},
			}
			createErr := l.client.Create(ctx, &newLease)
			if errors.IsAlreadyExists(createErr) {
				// Another writer created the lease between our Get and Create.
				// Convert to a conflict so RetryOnConflict re-reads and retries.
				return errors.NewConflict(
					schema.GroupResource{Group: coordinationv1.GroupName, Resource: "leases"},
					leaseName, createErr)
			}
			return createErr
		} else if err != nil {
			return err
		}

		// Lease exists. If it is already held by a different PR, this is a real
		// conflict (not retryable) and the caller should back off.
		if lease.Spec.HolderIdentity != nil && *lease.Spec.HolderIdentity != identity {
			return fmt.Errorf("lease is already locked by %s", *lease.Spec.HolderIdentity)
		}

		// Not locked, or already held by us (idempotent) -> acquire/refresh.
		lease.Spec.HolderIdentity = &identity
		lease.Spec.AcquireTime = &metav1.MicroTime{Time: metav1.Now().Time}
		return l.client.Update(ctx, &lease)
	})
}

// ensureLeaseExists creates the persistent lease if it does not already exist.
func (l *LeaseLocker) ensureLeaseExists(ctx context.Context, leaseName, targetNS, quotaName string) error {
	var lease coordinationv1.Lease
	err := l.client.Get(ctx, client.ObjectKey{Name: leaseName, Namespace: ControllerNamespace}, &lease)
	if err == nil {
		return nil
	}
	if !errors.IsNotFound(err) {
		return err
	}

	lease = coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Name:      leaseName,
			Namespace: ControllerNamespace,
			Labels:    managedLeaseLabels(targetNS, quotaName),
		},
	}
	if err := l.client.Create(ctx, &lease); err != nil && !errors.IsAlreadyExists(err) {
		return err
	}
	return nil
}
