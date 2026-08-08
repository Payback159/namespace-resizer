package lock

import (
	"context"
	"fmt"
	"time"

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

// GetLock returns the PR ID if a lock exists, or 0 if not.
func (l *LeaseLocker) GetLock(ctx context.Context, targetNS, quotaName string) (int, error) {
	leaseName := l.getLeaseName(targetNS, quotaName)
	var lease coordinationv1.Lease

	err := l.client.Get(ctx, client.ObjectKey{Name: leaseName, Namespace: ControllerNamespace}, &lease)
	if err != nil {
		if errors.IsNotFound(err) {
			return 0, nil
		}
		return 0, err
	}

	// We store the PR ID in the HolderIdentity
	if lease.Spec.HolderIdentity == nil {
		return 0, nil
	}

	idStr := *lease.Spec.HolderIdentity
	// Format: "pr-123"
	var id int
	_, err = fmt.Sscanf(idStr, "pr-%d", &id)
	if err != nil {
		// If format is invalid, we assume it's not our lock or corrupted
		return 0, fmt.Errorf("invalid lock identity format: %s", idStr)
	}

	return id, nil
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

func (l *LeaseLocker) UpdateLock(ctx context.Context, targetNS, quotaName string, prID int) error {
	leaseName := l.getLeaseName(targetNS, quotaName)
	var lease coordinationv1.Lease
	if err := l.client.Get(ctx, client.ObjectKey{Name: leaseName, Namespace: ControllerNamespace}, &lease); err != nil {
		return err
	}

	identity := fmt.Sprintf("pr-%d", prID)
	lease.Spec.HolderIdentity = &identity
	lease.Spec.RenewTime = &metav1.MicroTime{Time: metav1.Now().Time}

	return l.client.Update(ctx, &lease)
}

// ReleaseLock releases the lock by clearing the HolderIdentity, but keeps the Lease object.
func (l *LeaseLocker) ReleaseLock(ctx context.Context, targetNS, quotaName string) error {
	return l.ReleaseLockWithTimestamp(ctx, targetNS, quotaName, nil)
}

// ReleaseLockWithTimestamp releases the lock and optionally sets the last-modified
// timestamp in a single atomic update. This avoids optimistic concurrency conflicts
// that occur when SetLastModified and ReleaseLock are called sequentially, because
// the cached client may return a stale resourceVersion on the second Get.
func (l *LeaseLocker) ReleaseLockWithTimestamp(ctx context.Context, targetNS, quotaName string, timestamp *time.Time) error {
	leaseName := l.getLeaseName(targetNS, quotaName)

	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var lease coordinationv1.Lease
		if err := l.client.Get(ctx, client.ObjectKey{Name: leaseName, Namespace: ControllerNamespace}, &lease); err != nil {
			return client.IgnoreNotFound(err)
		}

		// Set timestamp if provided
		if timestamp != nil {
			if lease.Annotations == nil {
				lease.Annotations = make(map[string]string)
			}
			lease.Annotations[AnnotationLastModified] = timestamp.Format(time.RFC3339)
		}

		// Clear identity to release lock
		lease.Spec.HolderIdentity = nil
		return l.client.Update(ctx, &lease)
	})
}

// SetLastModified updates the last-modified timestamp in the lease annotation
func (l *LeaseLocker) SetLastModified(ctx context.Context, targetNS, quotaName string, timestamp time.Time) error {
	leaseName := l.getLeaseName(targetNS, quotaName)

	// We expect the lease to exist (created during AcquireLock), but handle NotFound just in case
	err := l.ensureLeaseExists(ctx, leaseName, targetNS, quotaName)
	if err != nil {
		return err
	}

	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var lease coordinationv1.Lease
		if err := l.client.Get(ctx, client.ObjectKey{Name: leaseName, Namespace: ControllerNamespace}, &lease); err != nil {
			return err
		}
		if lease.Annotations == nil {
			lease.Annotations = make(map[string]string)
		}
		lease.Annotations[AnnotationLastModified] = timestamp.Format(time.RFC3339)
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

// GetLastModified returns the last-modified timestamp from the lease, or zero time if not set
func (l *LeaseLocker) GetLastModified(ctx context.Context, targetNS, quotaName string) (time.Time, error) {
	leaseName := l.getLeaseName(targetNS, quotaName)
	var lease coordinationv1.Lease

	err := l.client.Get(ctx, client.ObjectKey{Name: leaseName, Namespace: ControllerNamespace}, &lease)
	if err != nil {
		if errors.IsNotFound(err) {
			return time.Time{}, nil
		}
		return time.Time{}, err
	}

	val, ok := lease.Annotations[AnnotationLastModified]
	if !ok {
		return time.Time{}, nil
	}

	return time.Parse(time.RFC3339, val)
}

// CheckCooldown returns true if we are still in cooldown period based on LastModified
func (l *LeaseLocker) CheckCooldown(ctx context.Context, targetNS, quotaName string, duration time.Duration) (bool, error) {
	lastMod, err := l.GetLastModified(ctx, targetNS, quotaName)
	if err != nil {
		return false, err
	}

	if lastMod.IsZero() {
		return false, nil
	}

	// Check if LastModified + Duration > Now
	expiry := lastMod.Add(duration)
	if time.Now().Before(expiry) {
		return true, nil
	}

	return false, nil
}
