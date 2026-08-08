// Package sizing computes quota resize decisions. It has no Kubernetes client
// dependency: every function takes plain values and an explicit clock, so the
// time-dependent shrink gates can be tested exhaustively.
package sizing

import (
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// ParseEventMessage extracts the requested resource name and quantity from a
// ResourceQuota "exceeded quota" event message, e.g.
// "exceeded quota: my-quota, requested: cpu=1, used: cpu=10, limited: cpu=10".
func ParseEventMessage(message string) (corev1.ResourceName, resource.Quantity, error) {
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

// WorkloadKey strips the last segment (after the last hyphen) of a Pod or
// object name to identify the owning workload.
// e.g. "app-a-6b474476c4-xfg2z" -> "app-a-6b474476c4" (ReplicaSet name)
// e.g. "app-b-deployment-12345" -> "app-b-deployment"
// e.g. "web-0" -> "web" (StatefulSet)
func WorkloadKey(name string) string {
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

// PodRequests computes the effective resource requests/limits for a PodSpec,
// mapping short resource names onto their quota-scoped long keys (e.g. "cpu"
// -> "requests.cpu"). The effective request is
// max(sum(app containers), max(init containers)) per resource key.
func PodRequests(spec corev1.PodSpec) map[corev1.ResourceName]int64 {
	reqs := make(map[corev1.ResourceName]int64)

	// Helper to add resources with proper mapping
	addResources := func(list corev1.ResourceList, isLimit bool) {
		for name, qty := range list {
			key := name
			if isLimit {
				switch name {
				case corev1.ResourceCPU:
					key = corev1.ResourceLimitsCPU
				case corev1.ResourceMemory:
					key = corev1.ResourceLimitsMemory
				}
			} else {
				switch name {
				case corev1.ResourceCPU:
					key = corev1.ResourceRequestsCPU
				case corev1.ResourceMemory:
					key = corev1.ResourceRequestsMemory
				case corev1.ResourceStorage:
					key = corev1.ResourceRequestsStorage
				}
			}
			reqs[key] += qty.MilliValue()
		}
	}

	// 1. Sum of App Containers
	for _, c := range spec.Containers {
		addResources(c.Resources.Requests, false)
		addResources(c.Resources.Limits, true)
	}

	// 2. Max of Init Containers (Effective Request logic)
	// Note: For limits, the logic is similar (max of init vs sum of app).
	// However, K8s resource formula is complex.
	// For simplicity/safety in Quota resizing, taking the MAX of Init Containers
	// and adding it to the App Containers (if Init is larger) is a safe upper bound.
	// But strictly speaking: Effective = max( max(init), sum(app) )
	// Our current logic was: sum(app) + max(init - sum(app), 0) ?
	// No, the previous logic was:
	// reqs[name] += qty (for app)
	// if val > reqs[name] { reqs[name] = val } (for init)
	// This implements max(sum(app), max(init)). Correct.

	// We need to do this per-resource-key.
	// Since we now map keys, we can just iterate and compare.
	for _, c := range spec.InitContainers {
		// We need a temporary map for this init container to handle the mapping
		initReqs := make(map[corev1.ResourceName]int64)

		// Helper for single container
		addInit := func(list corev1.ResourceList, isLimit bool) {
			for name, qty := range list {
				key := name
				if isLimit {
					switch name {
					case corev1.ResourceCPU:
						key = corev1.ResourceLimitsCPU
					case corev1.ResourceMemory:
						key = corev1.ResourceLimitsMemory
					}
				} else {
					switch name {
					case corev1.ResourceCPU:
						key = corev1.ResourceRequestsCPU
					case corev1.ResourceMemory:
						key = corev1.ResourceRequestsMemory
					case corev1.ResourceStorage:
						key = corev1.ResourceRequestsStorage
					}
				}
				initReqs[key] = qty.MilliValue()
			}
		}

		addInit(c.Resources.Requests, false)
		addInit(c.Resources.Limits, true)

		// Compare with current total
		for k, v := range initReqs {
			if v > reqs[k] {
				reqs[k] = v
			}
		}
	}
	return reqs
}

// PVCRequests sums the storage requests across a set of PVC templates,
// mapping the short "storage" key onto the quota-scoped "requests.storage" key.
func PVCRequests(templates []corev1.PersistentVolumeClaim) map[corev1.ResourceName]int64 {
	reqs := make(map[corev1.ResourceName]int64)
	for _, pvc := range templates {
		// Requests
		for name, qty := range pvc.Spec.Resources.Requests {
			key := name
			if name == corev1.ResourceStorage {
				key = corev1.ResourceRequestsStorage
			}
			reqs[key] += qty.MilliValue()
		}
	}
	return reqs
}
