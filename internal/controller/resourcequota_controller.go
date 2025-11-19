/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/payback159/namespace-resizer/internal/git"
	"github.com/payback159/namespace-resizer/internal/lock"
)

// ResourceQuotaReconciler reconciles a ResourceQuota object
type ResourceQuotaReconciler struct {
	client.Client
	Scheme      *runtime.Scheme
	Recorder    record.EventRecorder
	GitProvider git.Provider
	Locker      *lock.LeaseLocker
}

// +kubebuilder:rbac:groups=core,resources=resourcequotas,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=resourcequotas/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core,resources=resourcequotas/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch;list;watch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch;delete

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *ResourceQuotaReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// 1. Fetch ResourceQuota
	var quota corev1.ResourceQuota
	if err := r.Get(ctx, req.NamespacedName, &quota); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// 2. Fetch Namespace to check for annotations
	var ns corev1.Namespace
	if err := r.Get(ctx, client.ObjectKey{Name: req.Namespace}, &ns); err != nil {
		logger.Error(err, "unable to fetch Namespace")
		return ctrl.Result{}, err
	}

	// 3. Check Opt-Out
	if val, ok := ns.Annotations["resizer.io/enabled"]; ok && val == "false" {
		logger.V(1).Info("Namespace is opted out", "namespace", req.Namespace)
		return ctrl.Result{}, nil
	}

	// 4. Parse Configuration (Defaults + Overrides)
	config := parseConfig(ns.Annotations)

	// 5. Analyze Quota Usage (Metric-based)
	needsResize := false
	recommendations := make(map[corev1.ResourceName]resource.Quantity)

	for resName, hardLimit := range quota.Status.Hard {
		used, ok := quota.Status.Used[resName]
		if !ok {
			continue
		}

		// Calculate usage percentage
		limitVal := hardLimit.MilliValue()
		usedVal := used.MilliValue()

		if limitVal == 0 {
			continue
		}

		percentage := (float64(usedVal) / float64(limitVal)) * 100

		if percentage >= config.Threshold {
			logger.Info("Threshold exceeded", "resource", resName, "usage", percentage, "threshold", config.Threshold)

			// Calculate new limit
			increment := float64(limitVal) * config.IncrementFactor
			newLimitVal := int64(float64(limitVal) + increment)

			newLimit := *resource.NewMilliQuantity(newLimitVal, hardLimit.Format)
			recommendations[resName] = newLimit
			needsResize = true
		}
	}

	// 6. Analyze Events (Event-based / Deficit Filling)
	eventRecommendations, err := r.analyzeEvents(ctx, quota, config)
	if err != nil {
		logger.Error(err, "failed to analyze events")
	} else {
		for res, recLimit := range eventRecommendations {
			// If event recommendation is higher than metric recommendation, use it
			if existing, ok := recommendations[res]; !ok || recLimit.Cmp(existing) > 0 {
				recommendations[res] = recLimit
				needsResize = true
				logger.Info("Event-based recommendation triggered", "resource", res, "newLimit", recLimit.String())
			}
		}
	}

	// 7. Act (GitOps Mode)

	// Check Lock first (regardless of whether we need a resize now)
	prID, err := r.Locker.GetLock(ctx, req.Namespace, quota.Name)
	if err != nil {
		logger.Error(err, "failed to get lock")
		return ctrl.Result{}, err
	}

	if prID != 0 {
		// Lock exists -> Check PR Status
		logger.Info("Lock found, checking PR status", "prID", prID)
		status, err := r.GitProvider.GetPRStatus(ctx, prID)
		if err != nil {
			logger.Error(err, "failed to get PR status")
			return ctrl.Result{}, err
		}

		if !status.IsOpen {
			// PR is merged or closed -> Release Lock
			logger.Info("PR is closed/merged, releasing lock", "prID", prID)
			if err := r.Locker.ReleaseLock(ctx, req.Namespace, quota.Name); err != nil {
				logger.Error(err, "failed to release lock")
				return ctrl.Result{}, err
			}
			// Requeue to start fresh
			return ctrl.Result{Requeue: true}, nil
		} else {
			// PR is open
			if needsResize {
				// Update if needed
				logger.Info("PR is open, updating if needed", "prID", prID)
				if err := r.GitProvider.UpdatePR(ctx, prID, quota.Name, req.Namespace, recommendations); err != nil {
					logger.Error(err, "failed to update PR")
					return ctrl.Result{}, err
				}
			} else {
				// PR is open but no resize needed anymore (maybe manual update?)
				// We could close the PR here, or just leave it.
				// For now, we do nothing and let it be.
				logger.Info("PR is open but no resize needed currently", "prID", prID)
			}
		}
	} else if needsResize {
		// No Lock AND Needs Resize -> Create PR & Acquire Lock

		// Log recommendation
		for res, newLimit := range recommendations {
			currentLimit := quota.Status.Hard[res]
			msg := fmt.Sprintf("Recommendation: Increase %s from %s to %s",
				res, currentLimit.String(), newLimit.String())
			logger.Info(msg)
			r.Recorder.Event(&quota, corev1.EventTypeWarning, "QuotaResizeRecommended", msg)
		}

		logger.Info("No lock found, creating PR")
		newPRID, err := r.GitProvider.CreatePR(ctx, quota.Name, req.Namespace, recommendations)
		if err != nil {
			logger.Error(err, "failed to create PR")
			return ctrl.Result{}, err
		}

		logger.Info("PR created, acquiring lock", "prID", newPRID)
		if err := r.Locker.AcquireLock(ctx, req.Namespace, quota.Name, newPRID); err != nil {
			logger.Error(err, "failed to acquire lock")
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
}

func (r *ResourceQuotaReconciler) analyzeEvents(ctx context.Context, quota corev1.ResourceQuota, config ResizerConfig) (map[corev1.ResourceName]resource.Quantity, error) {
	recommendations := make(map[corev1.ResourceName]resource.Quantity)

	// List events in the namespace
	var eventList corev1.EventList
	if err := r.List(ctx, &eventList, client.InNamespace(quota.Namespace)); err != nil {
		return nil, err
	}

	// Look for recent FailedCreate events mentioning this quota
	cutoff := time.Now().Add(-1 * time.Hour) // Only look at events from last hour

	for _, evt := range eventList.Items {
		if evt.LastTimestamp.Time.Before(cutoff) {
			continue
		}
		if evt.Type != corev1.EventTypeWarning || evt.Reason != "FailedCreate" {
			continue
		}
		if !strings.Contains(evt.Message, "exceeded quota") || !strings.Contains(evt.Message, quota.Name) {
			continue
		}

		// Parse message: "exceeded quota: my-quota, requested: cpu=1, used: cpu=10, limited: cpu=10"
		// This is a simplified parser. Real world messages vary.
		// We look for "requested: <res>=<val>"

		// Example logic for parsing (simplified)
		// In a real implementation, we would need a robust regex or parser.
		// For MVP, let's assume we can extract the resource name and requested amount if it follows standard format.

		// TODO: Implement robust parsing.
		// For now, we just check if we can find "requested: " and try to guess.
		// Actually, without robust parsing, we can't calculate exact deficit.
		// But we can fallback to a larger increment if we detect an event.

		// Let's try to parse "requested: cpu=500m"
		// Split by comma
		parts := strings.Split(evt.Message, ",")
		for _, part := range parts {
			part = strings.TrimSpace(part)
			if strings.HasPrefix(part, "requested: ") {
				// "requested: cpu=500m"
				reqPart := strings.TrimPrefix(part, "requested: ")
				// "cpu=500m"
				kv := strings.Split(reqPart, "=")
				if len(kv) == 2 {
					resName := corev1.ResourceName(kv[0])
					reqQty, err := resource.ParseQuantity(kv[1])
					if err == nil {
						// Calculate Deficit
						// Deficit = (Used + Requested) - Hard
						// NewLimit = Hard + Deficit + Buffer
						// Buffer could be 10% or fixed.

						if currentHard, ok := quota.Status.Hard[resName]; ok {
							currentUsed := quota.Status.Used[resName]

							// deficit = (used + requested) - hard
							// actually, if it failed, used + requested > hard.
							// so we need hard + (used + requested - hard) = used + requested.
							// So NewLimit >= Used + Requested.

							needed := currentUsed.DeepCopy()
							needed.Add(reqQty)

							// Add buffer (e.g. 10% or config.Increment)
							// Let's use config.IncrementFactor as buffer
							buffer := float64(needed.MilliValue()) * config.IncrementFactor
							needed.Add(*resource.NewMilliQuantity(int64(buffer), currentHard.Format))

							// Only recommend if the new limit is actually higher than the current limit
							if needed.Cmp(currentHard) > 0 {
								recommendations[resName] = needed
							}
						}
					}
				}
			}
		}
	}
	return recommendations, nil
}

type ResizerConfig struct {
	Threshold       float64 // e.g. 80.0
	IncrementFactor float64 // e.g. 0.2 for 20%
	Cooldown        time.Duration
}

func parseConfig(annotations map[string]string) ResizerConfig {
	// Defaults
	config := ResizerConfig{
		Threshold:       80.0,
		IncrementFactor: 0.2,
		Cooldown:        60 * time.Minute,
	}

	if val, ok := annotations["resizer.io/cpu-threshold"]; ok {
		if v, err := strconv.ParseFloat(val, 64); err == nil {
			config.Threshold = v
		}
	}
	// Note: In a real implementation, we would parse per-resource annotations.
	// For MVP, we use global or cpu-specific as generic.

	if val, ok := annotations["resizer.io/cpu-increment"]; ok {
		// Handle "20%" or "0.2"
		clean := strings.TrimSuffix(val, "%")
		if v, err := strconv.ParseFloat(clean, 64); err == nil {
			if strings.HasSuffix(val, "%") {
				config.IncrementFactor = v / 100.0
			} else {
				config.IncrementFactor = v
			}
		}
	}

	return config
}

// SetupWithManager sets up the controller with the Manager.
func (r *ResourceQuotaReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.ResourceQuota{}).
		Named("resourcequota").
		Watches(&corev1.Event{}, handler.EnqueueRequestsFromMapFunc(r.mapEventToQuota)).
		Complete(r)
}

func (r *ResourceQuotaReconciler) mapEventToQuota(ctx context.Context, obj client.Object) []reconcile.Request {
	evt, ok := obj.(*corev1.Event)
	if !ok {
		return nil
	}

	// Filter for FailedCreate
	if evt.Type != corev1.EventTypeWarning || evt.Reason != "FailedCreate" {
		return nil
	}

	// Check if message contains "exceeded quota"
	if !strings.Contains(evt.Message, "exceeded quota") {
		return nil
	}

	// Extract quota name
	// Message format: "exceeded quota: <quota-name>, ..."
	// We can split by ": "
	parts := strings.Split(evt.Message, ": ")
	if len(parts) < 2 {
		return nil
	}

	// "exceeded quota" is likely one of the parts, followed by the name
	// Example: "Forbidden: exceeded quota: my-quota, ..."
	// Or "exceeded quota: my-quota"

	// Let's look for the part starting with "exceeded quota"
	var quotaName string
	for _, part := range parts {
		if strings.Contains(part, "exceeded quota") {
			// The next part might be the quota name, or it's in this part?
			// Usually "exceeded quota: my-quota" -> part 1: "exceeded quota", part 2: "my-quota, requested..."

			// Actually strings.Split(": ") might be tricky.
			// Let's use a simpler approach.

			idx := strings.Index(evt.Message, "exceeded quota: ")
			if idx != -1 {
				rest := evt.Message[idx+len("exceeded quota: "):]
				// "my-quota, requested: ..."
				// Take until comma or end
				commaIdx := strings.Index(rest, ",")
				if commaIdx != -1 {
					quotaName = rest[:commaIdx]
				} else {
					quotaName = rest
				}
			}
			break
		}
	}

	if quotaName == "" {
		return nil
	}

	return []reconcile.Request{
		{NamespacedName: client.ObjectKey{
			Name:      quotaName,
			Namespace: evt.Namespace,
		}},
	}
}
