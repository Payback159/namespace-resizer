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
	"math"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const defaultKey = "default"

type ResizerConfig struct {
	Thresholds       map[corev1.ResourceName]float64
	IncrementFactors map[corev1.ResourceName]float64
	Cooldown         time.Duration
}

func (c ResizerConfig) GetThreshold(res corev1.ResourceName) float64 {
	// Check for specific resource match
	if v, ok := c.Thresholds[res]; ok {
		return v
	}

	// Check for resource type match (e.g. requests.cpu -> cpu)
	switch {
	case strings.Contains(string(res), "cpu"):
		if v, ok := c.Thresholds[corev1.ResourceCPU]; ok {
			return v
		}
	case strings.Contains(string(res), "memory"):
		if v, ok := c.Thresholds[corev1.ResourceMemory]; ok {
			return v
		}
	case strings.Contains(string(res), "storage"):
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

func extractPodNameFromMessage(message string) string {
	// Handle combined events prefix
	cleanMsg := strings.TrimPrefix(message, "(combined from similar events): ")

	// Pattern 1: "... for Pod <pod-name> in StatefulSet ..."
	// Example: "create Claim data-0 for Pod web-0 in StatefulSet web failed ..."
	if idx := strings.Index(cleanMsg, "for Pod "); idx != -1 {
		rest := cleanMsg[idx+len("for Pod "):]
		if spaceIdx := strings.Index(rest, " "); spaceIdx != -1 {
			return rest[:spaceIdx]
		}
		return rest
	}

	// Pattern 2: "create Pod <pod-name> in StatefulSet ..."
	// Example: "create Pod web-0 in StatefulSet web failed ..."
	if idx := strings.Index(cleanMsg, "create Pod "); idx != -1 {
		rest := cleanMsg[idx+len("create Pod "):]
		if spaceIdx := strings.Index(rest, " "); spaceIdx != -1 {
			return rest[:spaceIdx]
		}
		return rest
	}
	return ""
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

func (r *ResourceQuotaReconciler) calculateWorkloadDeficit(ctx context.Context, evt corev1.Event, failedRes corev1.ResourceName, failedQty resource.Quantity) (string, map[corev1.ResourceName]int64) {
	key := getWorkloadKey(evt.InvolvedObject.Name)
	logger := log.FromContext(ctx)

	// Default: just the failed resource from the event
	deficits := map[corev1.ResourceName]int64{
		failedRes: failedQty.MilliValue(),
	}

	// Helper to apply multiplier and replace deficits with spec-based values
	applySmartCalculation := func(podSpec corev1.PodSpec, pvcTemplates []corev1.PersistentVolumeClaim, missing int64) {
		if missing <= 0 {
			return
		}

		// 1. Calculate Pod Resources (CPU, Memory)
		// Effective Request = Max(Max(Init), Sum(Containers))
		reqs := getPodRequests(podSpec)

		// 2. Calculate Storage Resources (if PVC templates exist)
		if len(pvcTemplates) > 0 {
			pvcReqs := getPVCRequests(pvcTemplates)
			for k, v := range pvcReqs {
				reqs[k] += v
			}
		}

		// 3. Apply Multiplier (Missing Replicas)
		newDeficits := make(map[corev1.ResourceName]int64)
		for res, val := range reqs {
			newDeficits[res] = val * missing
		}

		// Overwrite the default event-based deficit
		deficits = newDeficits
	}

	logger.Info("Calculating deficit", "kind", evt.InvolvedObject.Kind, "name", evt.InvolvedObject.Name, "failedRes", failedRes, "failedQty", failedQty)

	switch evt.InvolvedObject.Kind {
	case "StatefulSet":
		var sts appsv1.StatefulSet
		if err := r.Get(ctx, types.NamespacedName{Name: evt.InvolvedObject.Name, Namespace: evt.InvolvedObject.Namespace}, &sts); err == nil {
			if sts.Spec.Replicas != nil {
				desired := *sts.Spec.Replicas
				current := sts.Status.Replicas
				logger.Info("StatefulSet stats", "desired", desired, "current", current)
				if desired > current {
					applySmartCalculation(sts.Spec.Template.Spec, sts.Spec.VolumeClaimTemplates, int64(desired-current))
				}
			}
		} else {
			logger.Error(err, "Failed to get StatefulSet", "name", evt.InvolvedObject.Name)
		}

	case "DaemonSet":
		var ds appsv1.DaemonSet
		if err := r.Get(ctx, types.NamespacedName{Name: evt.InvolvedObject.Name, Namespace: evt.InvolvedObject.Namespace}, &ds); err == nil {
			desired := ds.Status.DesiredNumberScheduled
			current := ds.Status.CurrentNumberScheduled
			if desired > current {
				applySmartCalculation(ds.Spec.Template.Spec, nil, int64(desired-current))
			}
		} else {
			logger.Error(err, "Failed to get DaemonSet", "name", evt.InvolvedObject.Name)
		}

	case "ReplicaSet":
		var rs appsv1.ReplicaSet
		if err := r.Get(ctx, types.NamespacedName{Name: evt.InvolvedObject.Name, Namespace: evt.InvolvedObject.Namespace}, &rs); err == nil {
			if rs.Spec.Replicas != nil {
				desired := *rs.Spec.Replicas
				current := rs.Status.Replicas
				if desired > current {
					applySmartCalculation(rs.Spec.Template.Spec, nil, int64(desired-current))
				}
			}
		} else {
			logger.Error(err, "Failed to get ReplicaSet", "name", evt.InvolvedObject.Name)
		}

	case "Pod":
		// Fallback for Pod events (e.g. if the event is on the Pod directly)
		// Try to find the owner (StatefulSet, ReplicaSet, DaemonSet)
		var pod corev1.Pod
		if err := r.Get(ctx, types.NamespacedName{Name: evt.InvolvedObject.Name, Namespace: evt.InvolvedObject.Namespace}, &pod); err == nil {
			// Check owner references
			for _, ref := range pod.OwnerReferences {
				if ref.Controller != nil && *ref.Controller {
					// Recursively call for the owner?
					// Or just handle known types here.
					// Construct a fake event for the owner?
					// This is getting complex.
					// Let's just log it for now.
					logger.Info("Event on Pod, owner found", "ownerKind", ref.Kind, "ownerName", ref.Name)
				}
			}
		}
	}

	return key, deficits
}

func getPodRequests(spec corev1.PodSpec) map[corev1.ResourceName]int64 {
	// 1. Sum of App Containers
	reqs := make(map[corev1.ResourceName]int64)
	for _, c := range spec.Containers {
		for name, qty := range c.Resources.Requests {
			reqs[name] += qty.MilliValue()
		}
	}

	// 2. Max of Init Containers (Effective Request logic)
	for _, c := range spec.InitContainers {
		for name, qty := range c.Resources.Requests {
			val := qty.MilliValue()
			if val > reqs[name] {
				reqs[name] = val
			}
		}
	}
	return reqs
}

func getPVCRequests(templates []corev1.PersistentVolumeClaim) map[corev1.ResourceName]int64 {
	reqs := make(map[corev1.ResourceName]int64)
	for _, pvc := range templates {
		for name, qty := range pvc.Spec.Resources.Requests {
			reqs[name] += qty.MilliValue()
		}
	}
	return reqs
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
