package lock

import (
	"context"
	"fmt"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// The namespace where the controller runs and stores leases
	ControllerNamespace = "namespace-resizer-system"
	// AnnotationLastModified stores the timestamp of the last successful resize action
	AnnotationLastModified = "resizer.io/last-modified"
)

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

	var lease coordinationv1.Lease
	err := l.client.Get(ctx, client.ObjectKey{Name: leaseName, Namespace: ControllerNamespace}, &lease)

	if errors.IsNotFound(err) {
		// Create new lease
		lease = coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{
				Name:      leaseName,
				Namespace: ControllerNamespace,
				Labels: map[string]string{
					"resizer.io/target-ns":         targetNS,
					"resizer.io/quota":             quotaName,
					"app.kubernetes.io/managed-by": "namespace-resizer",
				},
			},
			Spec: coordinationv1.LeaseSpec{
				HolderIdentity: &identity,
				AcquireTime:    &metav1.MicroTime{Time: metav1.Now().Time},
			},
		}
		return l.client.Create(ctx, &lease)
	} else if err != nil {
		return err
	}

	// Lease exists, check if locked
	if lease.Spec.HolderIdentity != nil {
		return fmt.Errorf("lease is already locked by %s", *lease.Spec.HolderIdentity)
	}

	// Not locked, acquire it
	lease.Spec.HolderIdentity = &identity
	lease.Spec.AcquireTime = &metav1.MicroTime{Time: metav1.Now().Time}
	return l.client.Update(ctx, &lease)
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
}

// SetLastModified updates the last-modified timestamp in the lease annotation
func (l *LeaseLocker) SetLastModified(ctx context.Context, targetNS, quotaName string, timestamp time.Time) error {
	leaseName := l.getLeaseName(targetNS, quotaName)
	var lease coordinationv1.Lease

	// We expect the lease to exist (created during AcquireLock), but handle NotFound just in case
	err := l.client.Get(ctx, client.ObjectKey{Name: leaseName, Namespace: ControllerNamespace}, &lease)
	if errors.IsNotFound(err) {
		// Create if not exists (should be rare for SetLastModified)
		lease = coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{
				Name:      leaseName,
				Namespace: ControllerNamespace,
				Labels: map[string]string{
					"resizer.io/target-ns":         targetNS,
					"resizer.io/quota":             quotaName,
					"app.kubernetes.io/managed-by": "namespace-resizer",
				},
			},
		}
		if err := l.client.Create(ctx, &lease); err != nil {
			return err
		}
		// Fetch again to get latest version
		if err := l.client.Get(ctx, client.ObjectKey{Name: leaseName, Namespace: ControllerNamespace}, &lease); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}

	if lease.Annotations == nil {
		lease.Annotations = make(map[string]string)
	}
	lease.Annotations[AnnotationLastModified] = timestamp.Format(time.RFC3339)
	return l.client.Update(ctx, &lease)
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
