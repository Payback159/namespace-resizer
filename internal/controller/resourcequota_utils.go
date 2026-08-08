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
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/payback159/namespace-resizer/internal/sizing"
)

// Kubernetes event reason and involved-object kinds handled by event analysis.
const (
	reasonFailedCreate = "FailedCreate"
	kindStatefulSet    = "StatefulSet"
	kindDaemonSet      = "DaemonSet"
	kindReplicaSet     = "ReplicaSet"
	kindPod            = "Pod"
)

func (r *ResourceQuotaReconciler) mapEventToQuota(ctx context.Context, obj client.Object) []reconcile.Request {
	evt, ok := obj.(*corev1.Event)
	if !ok {
		return nil
	}

	// Filter for FailedCreate
	if evt.Type != corev1.EventTypeWarning || evt.Reason != reasonFailedCreate {
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
	key := sizing.WorkloadKey(evt.InvolvedObject.Name)
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
		reqs := sizing.PodRequests(podSpec)

		// 2. Calculate Storage Resources (if PVC templates exist)
		if len(pvcTemplates) > 0 {
			pvcReqs := sizing.PVCRequests(pvcTemplates)
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
	case kindStatefulSet:
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

	case kindDaemonSet:
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

	case kindReplicaSet:
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

	case kindPod:
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
