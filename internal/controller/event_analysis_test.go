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

func TestCollectDeficits(t *testing.T) {
	// Setup Scheme
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = appsv1.AddToScheme(scheme)
	_ = coordinationv1.AddToScheme(scheme)

	// 1. Setup Objects
	nsName := "demo-ns"
	quotaName := "demo-quota"

	cpuReq := resource.MustParse("200m")
	memReq := resource.MustParse("100Mi")
	storageReq := resource.MustParse("1Gi")

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
									corev1.ResourceCPU:    cpuReq,
									corev1.ResourceMemory: memReq,
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
								corev1.ResourceStorage: storageReq,
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

	// Set Logger
	ctx := context.Background()
	logger := zap.New(zap.UseDevMode(true))
	ctx = ctrl.LoggerInto(ctx, logger)

	// Run collectDeficits
	deficits, err := r.collectDeficits(ctx, *quota, time.Time{})
	assert.NoError(t, err)

	// Assertions
	// Deficit per pod: 200m CPU, 100Mi Memory, 1Gi Storage (from the PVC
	// template). Missing replicas: 3. The raw deficit is Requested * missing,
	// independent of Used — the buffer/target math now lives in sizing.Decide.
	assert.Equal(t, cpuReq.MilliValue()*3, deficits[corev1.ResourceRequestsCPU], "CPU deficit should be 600m")
	assert.Equal(t, memReq.MilliValue()*3, deficits[corev1.ResourceRequestsMemory], "Memory deficit should be 300Mi")
	assert.Equal(t, storageReq.MilliValue()*3, deficits[corev1.ResourceRequestsStorage], "Storage deficit should be 3Gi")
}
