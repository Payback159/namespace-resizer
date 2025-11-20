package controller

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/payback159/namespace-resizer/internal/lock"
)

func TestAnalyzeEvents(t *testing.T) {
	// Setup Scheme
	scheme := runtime.NewScheme()
	corev1.AddToScheme(scheme)
	appsv1.AddToScheme(scheme)
	coordinationv1.AddToScheme(scheme)

	// 1. Setup Objects
	nsName := "demo-ns"
	quotaName := "demo-quota"

	quota := &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{
			Name:      quotaName,
			Namespace: nsName,
		},
		Status: corev1.ResourceQuotaStatus{
			Hard: corev1.ResourceList{
				corev1.ResourceRequestsCPU:     resource.MustParse("100m"),
				corev1.ResourceRequestsMemory:  resource.MustParse("100Mi"),
				corev1.ResourceRequestsStorage: resource.MustParse("1Gi"),
			},
			Used: corev1.ResourceList{
				corev1.ResourceRequestsCPU:     resource.MustParse("0"),
				corev1.ResourceRequestsMemory:  resource.MustParse("0"),
				corev1.ResourceRequestsStorage: resource.MustParse("1Gi"),
			},
		},
	}

	replicas := int32(3)
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "burst-sts",
			Namespace: nsName,
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: &replicas,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name: "main",
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("200m"),
									corev1.ResourceMemory: resource.MustParse("100Mi"),
								},
							},
						},
					},
				},
			},
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{
				{
					Spec: corev1.PersistentVolumeClaimSpec{
						Resources: corev1.VolumeResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceStorage: resource.MustParse("1Gi"),
							},
						},
					},
				},
			},
		},
		Status: appsv1.StatefulSetStatus{
			Replicas: 0, // All failed
		},
	}

	// Event
	evt := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "evt-1",
			Namespace: nsName,
		},
		InvolvedObject: corev1.ObjectReference{
			APIVersion: "apps/v1",
			Kind:       "StatefulSet",
			Name:       "burst-sts",
			Namespace:  nsName,
		},
		Type:          corev1.EventTypeWarning,
		Reason:        "FailedCreate",
		Message:       `create Pod burst-sts-0 in StatefulSet burst-sts failed error: pods "burst-sts-0" is forbidden: exceeded quota: demo-quota, requested: requests.cpu=200m, used: requests.cpu=0, limited: requests.cpu=100m`,
		LastTimestamp: metav1.Time{Time: time.Now()},
	}

	// Fake Client
	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(quota, sts, evt).
		Build()

	// Reconciler
	r := &ResourceQuotaReconciler{
		Client: client,
		Locker: lock.NewLeaseLocker(client),
	}

	// Config
	config := ResizerConfig{
		Thresholds:       map[corev1.ResourceName]float64{"default": 80.0},
		IncrementFactors: map[corev1.ResourceName]float64{"default": 0.2},
		Cooldown:         time.Minute,
	}

	// Set Logger
	ctx := context.Background()
	logger := zap.New(zap.UseDevMode(true))
	ctx = ctrl.LoggerInto(ctx, logger)

	// Run analyzeEvents
	recs, err := r.analyzeEvents(ctx, *quota, config)
	assert.NoError(t, err)

	// Assertions
	// We expect CPU recommendation:
	// Deficit per pod: 200m. Missing replicas: 3. Total deficit: 600m.
	// Base need: 0 + 600m = 600m.
	// Buffer: 600m * 0.2 = 120m.
	// Total: 720m.
	// Current Limit: 100m.
	// Recommendation: 720m.

	if val, ok := recs[corev1.ResourceRequestsCPU]; ok {
		assert.Equal(t, "720m", val.String())
	} else {
		assert.Fail(t, "CPU recommendation missing")
	}

	// We also expect Memory recommendation (Smart Calc includes all resources in PodSpec)
	// Deficit per pod: 100Mi. Total: 300Mi.
	// Buffer: 60Mi. Total: 360Mi.
	if val, ok := recs[corev1.ResourceRequestsMemory]; ok {
		assert.Equal(t, "360Mi", val.String())
	} else {
		assert.Fail(t, "Memory recommendation missing")
	}

	// We also expect Storage recommendation (Smart Calc includes PVCs)
	// Deficit per pod: 1Gi. Total: 3Gi.
	// Used: 1Gi.
	// Base: 1Gi + 3Gi = 4Gi.
	// Buffer: 4Gi * 0.2 = 0.8Gi = 819Mi (approx).
	// Total: 4.8Gi.
	if val, ok := recs[corev1.ResourceRequestsStorage]; ok {
		// 4Gi + 20% = 4.8Gi = 4915Mi approx
		// 4 * 1024 = 4096. 4096 * 1.2 = 4915.2
		// 4916Mi
		assert.Equal(t, "4916Mi", val.String())
	} else {
		assert.Fail(t, "Storage recommendation missing")
	}
}
