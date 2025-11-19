package lock

import (
	"context"
	"fmt"

	coordinationv1 "k8s.io/api/coordination/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// The namespace where the controller runs and stores leases
	ControllerNamespace = "namespace-resizer-system"
)

type LeaseLocker struct {
	client client.Client
}

func NewLeaseLocker(c client.Client) *LeaseLocker {
	return &LeaseLocker{client: c}
}

// GetLock returns the PR ID if a lock exists, or 0 if not.
func (l *LeaseLocker) GetLock(ctx context.Context, targetNS, quotaName string) (int, error) {
	leaseName := fmt.Sprintf("lock-%s-%s", targetNS, quotaName)
	var lease coordinationv1.Lease

	err := l.client.Get(ctx, client.ObjectKey{Name: leaseName, Namespace: ControllerNamespace}, &lease)
	if err != nil {
		if errors.IsNotFound(err) {
			return 0, nil
		}
		return 0, err
	}

	// We store the PR ID in the HolderIdentity or an Annotation
	// Let's use HolderIdentity for simplicity as "pr-<id>"
	if lease.Spec.HolderIdentity == nil {
		return 0, nil
	}

	idStr := *lease.Spec.HolderIdentity
	// Format: "pr-123"
	var id int
	_, err = fmt.Sscanf(idStr, "pr-%d", &id)
	if err != nil {
		return 0, fmt.Errorf("invalid lock identity format: %s", idStr)
	}

	return id, nil
}

func (l *LeaseLocker) AcquireLock(ctx context.Context, targetNS, quotaName string, prID int) error {
	leaseName := fmt.Sprintf("lock-%s-%s", targetNS, quotaName)
	identity := fmt.Sprintf("pr-%d", prID)

	// Create Lease
	lease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Name:      leaseName,
			Namespace: ControllerNamespace,
			Labels: map[string]string{
				"resizer.io/target-ns": targetNS,
				"resizer.io/quota":     quotaName,
			},
		},
		Spec: coordinationv1.LeaseSpec{
			HolderIdentity: &identity,
			AcquireTime:    &metav1.MicroTime{Time: metav1.Now().Time},
			// We could set LeaseDurationSeconds if we wanted auto-expiry,
			// but for PRs we want it to persist until merged/closed.
		},
	}

	err := l.client.Create(ctx, lease)
	if err != nil {
		if errors.IsAlreadyExists(err) {
			// Update existing? Usually Acquire means "create new".
			// If it exists, we should have found it in GetLock.
			// But maybe we want to update the PR ID?
			return l.UpdateLock(ctx, targetNS, quotaName, prID)
		}
		return err
	}
	return nil
}

func (l *LeaseLocker) UpdateLock(ctx context.Context, targetNS, quotaName string, prID int) error {
	leaseName := fmt.Sprintf("lock-%s-%s", targetNS, quotaName)
	var lease coordinationv1.Lease
	if err := l.client.Get(ctx, client.ObjectKey{Name: leaseName, Namespace: ControllerNamespace}, &lease); err != nil {
		return err
	}

	identity := fmt.Sprintf("pr-%d", prID)
	lease.Spec.HolderIdentity = &identity
	lease.Spec.RenewTime = &metav1.MicroTime{Time: metav1.Now().Time}

	return l.client.Update(ctx, &lease)
}

func (l *LeaseLocker) ReleaseLock(ctx context.Context, targetNS, quotaName string) error {
	leaseName := fmt.Sprintf("lock-%s-%s", targetNS, quotaName)
	var lease coordinationv1.Lease

	// We delete the lease to release the lock
	lease.Name = leaseName
	lease.Namespace = ControllerNamespace

	return client.IgnoreNotFound(l.client.Delete(ctx, &lease))
}
