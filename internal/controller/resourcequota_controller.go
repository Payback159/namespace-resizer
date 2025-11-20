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
	"math"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

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

const defaultKey = "default"

type ResizerConfig struct {
	Thresholds       map[corev1.ResourceName]float64
	IncrementFactors map[corev1.ResourceName]float64
	Cooldown         time.Duration
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
		key := getWorkloadKey(evt.InvolvedObject.Name)

		// Initialize map for this resource if needed
		if _, ok := deficits[resName]; !ok {
			deficits[resName] = make(map[string]int64)
		}

		// Store the max requested value seen for this specific workload key
		if reqQty.MilliValue() > deficits[resName][key] {
			deficits[resName][key] = reqQty.MilliValue()
		}
	}

	// Now calculate recommendations based on SUM of MAX deficits per workload
	for resName, workloadMap := range deficits {
		var totalDeficit int64
		for _, val := range workloadMap {
			totalDeficit += val
		}

		if currentHard, ok := quota.Status.Hard[resName]; ok {
			currentUsed := quota.Status.Used[resName]

			// Calculate base need (Used + Total Deficit from distinct workloads)
			baseMilli := currentUsed.MilliValue() + totalDeficit

			// Calculate buffer based on the NEW total need
			bufferMilli := float64(baseMilli) * config.GetIncrement(resName)

			// Total
			totalMilli := baseMilli + int64(bufferMilli)

			// Create new Quantity with correct format/rounding
			needed := convertToReadableFormat(resName, totalMilli, currentHard.Format)

			// Only recommend if the new limit is actually higher than the current limit
			if needed.Cmp(currentHard) > 0 {
				recommendations[resName] = needed
			}
		}
	}

	return recommendations, nil
}

func parseEventMessage(message string) (corev1.ResourceName, resource.Quantity, error) {
	// Parse message: "exceeded quota: my-quota, requested: cpu=1, used: cpu=10, limited: cpu=10"
	parts := strings.Split(message, ",")
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
					return resName, reqQty, nil
				}
			}
		}
	}
	return "", resource.Quantity{}, fmt.Errorf("failed to parse message")
}

func (r *ResourceQuotaReconciler) isObjectAlive(ctx context.Context, ref corev1.ObjectReference, namespace string) bool {
	logger := log.FromContext(ctx)
	// Construct Unstructured object to query API
	u := &unstructured.Unstructured{}
	gv, err := schema.ParseGroupVersion(ref.APIVersion)
	if err != nil {
		logger.Error(err, "Failed to parse GroupVersion", "apiVersion", ref.APIVersion)
		// Fallback: try to guess or just fail safe (assume not alive if we can't parse)
		// But APIVersion should be valid in Event.
		return false
	}
	u.SetGroupVersionKind(gv.WithKind(ref.Kind))

	key := types.NamespacedName{Name: ref.Name, Namespace: namespace}
	if err := r.Get(ctx, key, u); err != nil {
		return false
	}
	return true
}

func (c ResizerConfig) GetThreshold(res corev1.ResourceName) float64 {
	// Check for specific resource match
	if v, ok := c.Thresholds[res]; ok {
		return v
	}
	// Check for resource type match (e.g. requests.cpu -> cpu)
	if strings.Contains(string(res), "cpu") {
		if v, ok := c.Thresholds[corev1.ResourceCPU]; ok {
			return v
		}
	}
	if strings.Contains(string(res), "memory") {
		if v, ok := c.Thresholds[corev1.ResourceMemory]; ok {
			return v
		}
	}
	if strings.Contains(string(res), "storage") {
		if v, ok := c.Thresholds[corev1.ResourceStorage]; ok {
			return v
		}
	}
	// Fallback to default
	if v, ok := c.Thresholds[defaultKey]; ok {
		return v
	}
	return 80.0
}

func (c ResizerConfig) GetIncrement(res corev1.ResourceName) float64 {
	if v, ok := c.IncrementFactors[res]; ok {
		return v
	}
	if strings.Contains(string(res), "cpu") {
		if v, ok := c.IncrementFactors[corev1.ResourceCPU]; ok {
			return v
		}
	}
	if strings.Contains(string(res), "memory") {
		if v, ok := c.IncrementFactors[corev1.ResourceMemory]; ok {
			return v
		}
	}
	if strings.Contains(string(res), "storage") {
		if v, ok := c.IncrementFactors[corev1.ResourceStorage]; ok {
			return v
		}
	}
	if v, ok := c.IncrementFactors[defaultKey]; ok {
		return v
	}
	return 0.2
}

func parseConfig(annotations map[string]string) ResizerConfig {
	config := ResizerConfig{
		Thresholds:       make(map[corev1.ResourceName]float64),
		IncrementFactors: make(map[corev1.ResourceName]float64),
		Cooldown:         60 * time.Minute,
	}

	// Set Defaults
	config.Thresholds[defaultKey] = 80.0
	config.IncrementFactors[defaultKey] = 0.2

	// Helper to parse percentage
	parsePercent := func(val string) (float64, bool) {
		clean := strings.TrimSuffix(val, "%")
		v, err := strconv.ParseFloat(clean, 64)
		if err != nil {
			return 0, false
		}
		if strings.HasSuffix(val, "%") {
			return v / 100.0, true
		}
		return v, true // Assume raw float (0.2) or int (80) depending on context?
		// For threshold we expect 80. For increment we expect 0.2 or 20%.
		// Let's handle them separately in the loop if needed, or just be smart.
	}

	for k, v := range annotations {
		if !strings.HasPrefix(k, "resizer.io/") {
			continue
		}
		key := strings.TrimPrefix(k, "resizer.io/")

		// Thresholds
		if strings.HasSuffix(key, "-threshold") {
			// e.g. "threshold", "cpu-threshold", "requests.memory-threshold"
			res := strings.TrimSuffix(key, "-threshold")
			if res == "" {
				res = defaultKey
			}

			if val, err := strconv.ParseFloat(v, 64); err == nil {
				config.Thresholds[corev1.ResourceName(res)] = val
			}
		}

		// Increments
		if strings.HasSuffix(key, "-increment") {
			res := strings.TrimSuffix(key, "-increment")
			if res == "" {
				res = defaultKey
			}

			if val, ok := parsePercent(v); ok {
				// If user wrote "20", parsePercent returns 20. But for increment we want 0.2?
				// Or maybe we standardize on "0.2" or "20%".
				// If > 1, assume percentage? No, 2.0 means 200%.
				// Let's stick to: if "%" suffix -> /100. If no suffix -> raw value.
				// But for threshold "80" means 80%.
				// Increment: "0.2" = 20%. "20%" = 20%.
				config.IncrementFactors[corev1.ResourceName(res)] = val
			}
		}

		// Cooldown
		if key == "cooldown-minutes" {
			if val, err := strconv.Atoi(v); err == nil {
				config.Cooldown = time.Duration(val) * time.Minute
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

func getWorkloadKey(name string) string {
	// Heuristic: Strip the last segment (after the last hyphen) to identify the workload.
	// e.g. "app-a-6b474476c4-xfg2z" -> "app-a-6b474476c4" (ReplicaSet name)
	// e.g. "app-b-deployment-12345" -> "app-b-deployment"
	// e.g. "web-0" -> "web" (StatefulSet)
	lastHyphen := strings.LastIndex(name, "-")
	if lastHyphen == -1 {
		return name
	}
	return name[:lastHyphen]
}

func convertToReadableFormat(resName corev1.ResourceName, milliValue int64, format resource.Format) resource.Quantity {
	if strings.Contains(string(resName), "memory") || strings.Contains(string(resName), "storage") {
		// Memory/Storage Fix: Convert from Milli-Bytes back to Bytes
		// 1000 Millis = 1 Byte
		bytesValue := float64(milliValue) / 1000.0

		// Round up to the nearest Mebibyte (Mi) to ensure readable output (e.g. "123Mi" instead of raw bytes)
		// Kubernetes resource.Quantity prefers multiples of 1024 for BinarySI to display friendly units.
		const bytesPerMi = 1024 * 1024
		miValue := math.Ceil(bytesValue / float64(bytesPerMi))
		newBytesValue := int64(miValue * float64(bytesPerMi))

		return *resource.NewQuantity(newBytesValue, resource.BinarySI)
	}
	return *resource.NewMilliQuantity(milliValue, format)
}
