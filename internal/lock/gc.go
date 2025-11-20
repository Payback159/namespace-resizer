package lock

import (
	"context"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// LeaseGarbageCollector cleans up orphaned leases for non-existent namespaces
type LeaseGarbageCollector struct {
	client   client.Client
	interval time.Duration
}

// NewLeaseGarbageCollector creates a new GC instance
func NewLeaseGarbageCollector(c client.Client, interval time.Duration) *LeaseGarbageCollector {
	return &LeaseGarbageCollector{
		client:   c,
		interval: interval,
	}
}

// Start implements manager.Runnable to run in the controller manager
func (gc *LeaseGarbageCollector) Start(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("lease-gc")
	logger.Info("Starting Lease Garbage Collector", "interval", gc.interval)

	// Run once immediately
	if err := gc.cleanup(ctx); err != nil {
		logger.Error(err, "Failed to run initial cleanup")
	}

	ticker := time.NewTicker(gc.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			logger.Info("Stopping Lease Garbage Collector")
			return nil
		case <-ticker.C:
			if err := gc.cleanup(ctx); err != nil {
				logger.Error(err, "Failed to cleanup leases")
			}
		}
	}
}

func (gc *LeaseGarbageCollector) cleanup(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("lease-gc")

	// List all leases managed by us in the controller namespace
	var leaseList coordinationv1.LeaseList
	if err := gc.client.List(ctx, &leaseList,
		client.InNamespace(ControllerNamespace),
		client.MatchingLabels{"app.kubernetes.io/managed-by": "namespace-resizer"}); err != nil {
		return err
	}

	for _, lease := range leaseList.Items {
		targetNS := lease.Labels["resizer.io/target-ns"]
		if targetNS == "" {
			// Skip leases without target namespace label (should not happen for ours)
			continue
		}

		// Check if namespace exists
		var ns corev1.Namespace
		err := gc.client.Get(ctx, client.ObjectKey{Name: targetNS}, &ns)
		if errors.IsNotFound(err) {
			// Namespace gone, delete lease
			logger.Info("Deleting orphaned lease", "lease", lease.Name, "targetNamespace", targetNS)
			if err := gc.client.Delete(ctx, &lease); err != nil {
				logger.Error(err, "Failed to delete orphaned lease", "lease", lease.Name)
			}
		} else if err != nil {
			logger.Error(err, "Failed to check namespace existence", "namespace", targetNS)
		}
	}
	return nil
}
