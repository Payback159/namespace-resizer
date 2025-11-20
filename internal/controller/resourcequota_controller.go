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
	"errors"
	"fmt"
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

	resizerConfig "github.com/payback159/namespace-resizer/internal/config"
	"github.com/payback159/namespace-resizer/internal/git"
	"github.com/payback159/namespace-resizer/internal/lock"
)

// ResourceQuotaReconciler reconciles a ResourceQuota object
type ResourceQuotaReconciler struct {
	client.Client
	Scheme          *runtime.Scheme
	Recorder        record.EventRecorder
	GitProvider     git.Provider
	Locker          *lock.LeaseLocker
	EnableAutoMerge bool
}

// +kubebuilder:rbac:groups=core,resources=resourcequotas,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=resourcequotas/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core,resources=resourcequotas/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch;list;watch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=deployments;replicasets;statefulsets;daemonsets,verbs=get;list;watch
// +kubebuilder:rbac:groups=batch,resources=jobs;cronjobs,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *ResourceQuotaReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Reconciling ResourceQuota", "name", req.Name, "namespace", req.Namespace)

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

	// 5. Calculate Recommendations (Metrics + Events)
	recommendations, needsResize, err := r.calculateRecommendations(ctx, quota, config)
	if err != nil {
		logger.Error(err, "failed to calculate recommendations")
		// Continue execution, maybe metrics worked but events failed?
		// For now, we proceed if we have any recommendations.
	}

	// 6. GitOps Workflow
	// Check Lock first (regardless of whether we need a resize now)
	prID, err := r.Locker.GetLock(ctx, req.Namespace, quota.Name)
	if err != nil {
		logger.Error(err, "failed to get lock")
		return ctrl.Result{}, err
	}

	if prID != 0 {
		// Case A: Lock exists -> Handle Active PR
		return r.handleActivePR(ctx, req, quota, ns, prID, recommendations, needsResize)
	}

	if needsResize {
		// Case B: No Lock AND Needs Resize -> Handle New Proposal
		return r.handleNewProposal(ctx, req, quota, ns, config, recommendations)
	}

	// Case C: No Lock, No Resize needed -> Idle
	return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
}

// calculateRecommendations combines metric-based and event-based analysis
func (r *ResourceQuotaReconciler) calculateRecommendations(ctx context.Context, quota corev1.ResourceQuota, config ResizerConfig) (map[corev1.ResourceName]resource.Quantity, bool, error) {
	logger := log.FromContext(ctx)
	recommendations := make(map[corev1.ResourceName]resource.Quantity)
	needsResize := false

	// A. Metric Analysis
	for resName, hardLimit := range quota.Status.Hard {
		used, ok := quota.Status.Used[resName]
		if !ok {
			continue
		}

		limitVal := hardLimit.MilliValue()
		usedVal := used.MilliValue()

		if limitVal == 0 {
			continue
		}

		percentage := (float64(usedVal) / float64(limitVal)) * 100

		if percentage >= config.GetThreshold(resName) {
			logger.Info("Threshold exceeded", "resource", resName, "usage", percentage, "threshold", config.GetThreshold(resName))

			increment := float64(limitVal) * config.GetIncrement(resName)
			newLimitVal := int64(float64(limitVal) + increment)
			newLimit := convertToReadableFormat(resName, newLimitVal, hardLimit.Format)

			recommendations[resName] = newLimit
			needsResize = true
		}
	}

	// B. Event Analysis
	eventRecommendations, err := r.analyzeEvents(ctx, quota, config)
	if err != nil {
		return recommendations, needsResize, err
	}

	for res, recLimit := range eventRecommendations {
		// If event recommendation is higher than metric recommendation, use it
		if existing, ok := recommendations[res]; !ok || recLimit.Cmp(existing) > 0 {
			recommendations[res] = recLimit
			needsResize = true
			logger.Info("Event-based recommendation triggered", "resource", res, "newLimit", recLimit.String())
		}
	}

	return recommendations, needsResize, nil
}

// handleActivePR manages the lifecycle of an existing Pull Request
func (r *ResourceQuotaReconciler) handleActivePR(ctx context.Context, req ctrl.Request, quota corev1.ResourceQuota, ns corev1.Namespace, prID int, recommendations map[corev1.ResourceName]resource.Quantity, needsResize bool) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("Lock found, checking PR status", "prID", prID)

	status, err := r.GitProvider.GetPRStatus(ctx, prID)
	if err != nil {
		logger.Error(err, "failed to get PR status")
		return ctrl.Result{}, err
	}

	if !status.IsOpen {
		// PR is merged or closed -> Release Lock
		logger.Info("PR is closed/merged, releasing lock", "prID", prID)

		if status.IsMerged {
			logger.Info("PR merged, updating last-modified timestamp", "timestamp", time.Now())
			if err := r.Locker.SetLastModified(ctx, req.Namespace, quota.Name, time.Now()); err != nil {
				logger.Error(err, "failed to set last-modified timestamp")
			}
		}

		if err := r.Locker.ReleaseLock(ctx, req.Namespace, quota.Name); err != nil {
			logger.Error(err, "failed to release lock")
			return ctrl.Result{}, err
		}

		// Requeue immediately to start fresh (check cooldown, etc.)
		return ctrl.Result{Requeue: true}, nil
	}

	// PR is open -> Check Auto-Merge
	shouldAutoMerge := r.EnableAutoMerge
	if val, ok := ns.Annotations[resizerConfig.AnnotationAutoMerge]; ok && val == "false" {
		shouldAutoMerge = false
	}

	if shouldAutoMerge {
		if strings.ToLower(status.MergeableState) == "unknown" {
			logger.Info("Mergeable state unknown from GitHub; requeueing to allow computation", "prID", prID)
			return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
		}

		canAttemptMerge := status.Mergeable &&
			(status.MergeableState == "clean" ||
				(status.MergeableState == "blocked" && (status.ChecksState == "success" || status.ChecksTotalCount == 0)))

		if canAttemptMerge {
			logger.Info("Auto-merging PR", "prID", prID, "state", status.MergeableState, "checks", status.ChecksState, "checksCount", status.ChecksTotalCount)
			if err := r.GitProvider.MergePR(ctx, prID, "squash"); err != nil {
				logger.Error(err, "failed to auto-merge PR")
			} else {
				return ctrl.Result{Requeue: true}, nil
			}
		} else {
			logger.Info("Auto-merge enabled but PR is not ready",
				"mergeable", status.Mergeable,
				"state", status.MergeableState,
				"checks", status.ChecksState,
				"checksCount", status.ChecksTotalCount)
		}
	}

	// Update PR if recommendations changed
	if needsResize {
		logger.Info("PR is open, updating if needed", "prID", prID)
		if err := r.GitProvider.UpdatePR(ctx, prID, quota.Name, req.Namespace, ns.Annotations, recommendations); err != nil {
			if errors.Is(err, git.ErrFileNotFound) {
				logger.Info("Quota file not found in Git repository during update. Retrying later.", "error", err.Error())
				return ctrl.Result{RequeueAfter: 1 * time.Minute}, nil
			}
			logger.Error(err, "failed to update PR")
			return ctrl.Result{}, err
		}
	} else {
		logger.Info("PR is open but no resize needed currently", "prID", prID)
	}

	return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
}

// handleNewProposal manages the creation of new Pull Requests
func (r *ResourceQuotaReconciler) handleNewProposal(ctx context.Context, req ctrl.Request, quota corev1.ResourceQuota, ns corev1.Namespace, config ResizerConfig, recommendations map[corev1.ResourceName]resource.Quantity) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// 1. Detect No-Op
	isNoop := true
	for res, rec := range recommendations {
		cur, ok := quota.Spec.Hard[res]
		if !ok {
			isNoop = false
			break
		}
		if cur.Cmp(rec) != 0 {
			isNoop = false
			break
		}
	}
	if isNoop {
		logger.Info("Detected no-op recommendation; skipping PR creation", "namespace", req.Namespace, "quota", quota.Name)
		return ctrl.Result{RequeueAfter: 10 * time.Minute}, nil
	}

	// 2. Smart Cooldown Check
	lastMod, err := r.Locker.GetLastModified(ctx, req.Namespace, quota.Name)
	if err != nil {
		logger.Error(err, "failed to get last modified time")
		return ctrl.Result{}, err
	}

	if !lastMod.IsZero() {
		elapsed := time.Since(lastMod)
		if elapsed < config.Cooldown {
			remaining := config.Cooldown - elapsed
			logger.Info("Skipping resize due to cooldown", "cooldown", config.Cooldown, "remaining", remaining)
			// Requeue exactly when cooldown expires (plus a small buffer)
			return ctrl.Result{RequeueAfter: remaining + 1*time.Second}, nil
		}
	}

	// 3. Create PR
	// Log recommendation
	for res, newLimit := range recommendations {
		currentLimit := quota.Status.Hard[res]
		msg := fmt.Sprintf("Recommendation: Increase %s from %s to %s",
			res, currentLimit.String(), newLimit.String())
		logger.Info(msg)
		r.Recorder.Event(&quota, corev1.EventTypeWarning, "QuotaResizeRecommended", msg)
	}

	logger.Info("No lock found, creating PR")
	newPRID, err := r.GitProvider.CreatePR(ctx, quota.Name, req.Namespace, ns.Annotations, recommendations)
	if err != nil {
		if errors.Is(err, git.ErrFileNotFound) {
			logger.Info("Quota file not found in Git repository. Retrying later.", "error", err.Error())
			return ctrl.Result{RequeueAfter: 1 * time.Minute}, nil
		}
		logger.Error(err, "failed to create PR")
		return ctrl.Result{}, err
	}

	logger.Info("PR created, acquiring lock", "prID", newPRID)
	if err := r.Locker.AcquireLock(ctx, req.Namespace, quota.Name, newPRID); err != nil {
		logger.Error(err, "failed to acquire lock")
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
}

func (r *ResourceQuotaReconciler) analyzeEvents(ctx context.Context, quota corev1.ResourceQuota, config ResizerConfig) (map[corev1.ResourceName]resource.Quantity, error) {
	logger := log.FromContext(ctx)
	recommendations := make(map[corev1.ResourceName]resource.Quantity)

	// List events in the namespace
	var eventList corev1.EventList
	if err := r.List(ctx, &eventList, client.InNamespace(quota.Namespace)); err != nil {
		return nil, err
	}

	// Get LastModified to filter old events (Deduplication)
	lastMod, err := r.Locker.GetLastModified(ctx, quota.Namespace, quota.Name)
	if err != nil {
		// Log error but continue, assuming no last modified (process all events)
		// We don't have a logger here easily, but we can ignore or return error.
		// Returning error might be safer to avoid double counting if DB is down.
		// return nil, fmt.Errorf("failed to get last modified time: %w", err)
		logger.Error(err, "failed to get last modified time")
	}

	// Look for recent FailedCreate events mentioning this quota
	cutoff := time.Now().Add(-1 * time.Hour) // Only look at events from last hour

	// Map to store max requested per resource per workload key
	// map[ResourceName]map[WorkloadKey]int64 (milli-value)
	deficits := make(map[corev1.ResourceName]map[string]int64)

	for _, evt := range eventList.Items {
		if evt.LastTimestamp.Time.Before(cutoff) {
			continue
		}
		// Deduplication: Ignore events that happened before the last successful resize
		if !lastMod.IsZero() && evt.LastTimestamp.Time.Before(lastMod) {
			continue
		}

		if evt.Type != corev1.EventTypeWarning || evt.Reason != "FailedCreate" {
			continue
		}

		if !strings.Contains(evt.Message, "exceeded quota") || !strings.Contains(evt.Message, quota.Name) {
			continue
		}

		// 1. Parse Message
		resName, reqQty, err := parseEventMessage(evt.Message)
		if err != nil {
			logger.Error(err, "Failed to parse event message", "message", evt.Message)
			continue
		}

		// 2. Liveness Check
		// Ensure the object causing the event still exists.
		// This prevents "ghost" deficits from deleted workloads (e.g. during rollback/restart).
		if !r.isObjectAlive(ctx, evt.InvolvedObject, quota.Namespace) {
			continue
		}

		// 3. Update Deficits (Grouped by Workload Prefix)
		// We group by the "Workload Key" (e.g. ReplicaSet name) to distinguish between
		// "Same workload retrying" (use MAX) and "Different workloads failing" (use SUM).
		key, workloadDeficits := r.calculateWorkloadDeficit(ctx, evt, resName, reqQty)

		// Iterate over all resources returned by the smart calculation
		for rName, deficitValue := range workloadDeficits {
			// Initialize map for this resource if needed
			if _, ok := deficits[rName]; !ok {
				deficits[rName] = make(map[string]int64)
			}

			// Store the max requested value seen for this specific workload key
			// Note: If smart calculation returns a value, it's the TOTAL deficit for that workload.
			// If we see multiple events for the same workload (e.g. CPU failed, then Mem failed?),
			// we should take the max. But usually smart calculation returns the same total.
			if deficitValue > deficits[rName][key] {
				deficits[rName][key] = deficitValue
			}
		}
	}

	// Now calculate recommendations based on SUM of MAX deficits per workload
	for resName, workloadMap := range deficits {
		var totalDeficit int64
		for _, val := range workloadMap {
			totalDeficit += val
		}

		// Resolve Quota Resource Name (Map cpu -> requests.cpu if needed)
		quotaResName := resName
		if _, ok := quota.Status.Hard[quotaResName]; !ok {
			if resName == corev1.ResourceCPU {
				quotaResName = corev1.ResourceRequestsCPU
			} else if resName == corev1.ResourceMemory {
				quotaResName = corev1.ResourceRequestsMemory
			} else if resName == corev1.ResourceStorage {
				quotaResName = corev1.ResourceRequestsStorage
			}
		}

		if currentHard, ok := quota.Status.Hard[quotaResName]; ok {
			currentUsed := quota.Status.Used[quotaResName]

			// Calculate base need (Used + Total Deficit from distinct workloads)
			baseMilli := currentUsed.MilliValue() + totalDeficit

			// Calculate buffer based on the NEW total need
			bufferMilli := float64(baseMilli) * config.GetIncrement(quotaResName)

			// Total
			totalMilli := baseMilli + int64(bufferMilli)

			// Create new Quantity with correct format/rounding
			needed := convertToReadableFormat(quotaResName, totalMilli, currentHard.Format)

			// Only recommend if the new limit is actually higher than the current limit
			if needed.Cmp(currentHard) > 0 {
				recommendations[quotaResName] = needed
			}
		}
	}

	return recommendations, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ResourceQuotaReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.ResourceQuota{}).
		Named("resourcequota").
		Watches(&corev1.Event{}, handler.EnqueueRequestsFromMapFunc(r.mapEventToQuota)).
		Complete(r)
}
