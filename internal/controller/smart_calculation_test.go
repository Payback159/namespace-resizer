package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestCalculateWorkloadDeficit_StatefulSet_SmartCalculation(t *testing.T) {
	// Setup Scheme
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = appsv1.AddToScheme(scheme)

	// Define Resources
	cpuReq := resource.MustParse("200m")
	memReq := resource.MustParse("100Mi")
	storageReq := resource.MustParse("1Gi")

	replicas := int32(3)
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "burst-sts",
			Namespace: "default",
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: &replicas,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name: "nginx",
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
					ObjectMeta: metav1.ObjectMeta{Name: "data"},
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
			Replicas: 0, // All 3 missing
		},
	}

	// Setup Client
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sts).Build()
	r := &ResourceQuotaReconciler{
		Client: client,
	}

	// Create Event
	evt := corev1.Event{
		InvolvedObject: corev1.ObjectReference{
			Kind:      "StatefulSet",
			Name:      "burst-sts",
			Namespace: "default",
		},
	}

	// Call Function
	// We simulate a CPU failure
	failedRes := corev1.ResourceCPU
	failedQty := resource.MustParse("200m")

	_, deficits := r.calculateWorkloadDeficit(context.Background(), evt, failedRes, failedQty)

	// Assertions
	// Expected: 3 replicas * (200m CPU, 100Mi Mem, 1Gi Storage)
	// CPU: 600m
	// Mem: 300Mi
	// Storage: 3Gi

	assert.Equal(t, int64(600), deficits[corev1.ResourceRequestsCPU], "CPU deficit should be 600m")

	expectedMem := memReq.MilliValue() * 3
	assert.Equal(t, expectedMem, deficits[corev1.ResourceRequestsMemory], "Memory deficit should be 300Mi")

	expectedStorage := storageReq.MilliValue() * 3
	assert.Equal(t, expectedStorage, deficits[corev1.ResourceRequestsStorage], "Storage deficit should be 3Gi")
}

func TestCalculateWorkloadDeficit_DaemonSet_SmartCalculation(t *testing.T) {
	// Setup Scheme
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = appsv1.AddToScheme(scheme)

	// Define Resources
	cpuReq := resource.MustParse("100m")
	memReq := resource.MustParse("50Mi")

	ds := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "burst-ds",
			Namespace: "default",
		},
		Spec: appsv1.DaemonSetSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name: "agent",
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
		},
		Status: appsv1.DaemonSetStatus{
			DesiredNumberScheduled: 5,
			CurrentNumberScheduled: 2, // 3 missing
		},
	}

	// Setup Client
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ds).Build()
	r := &ResourceQuotaReconciler{
		Client: client,
	}

	// Create Event
	evt := corev1.Event{
		InvolvedObject: corev1.ObjectReference{
			Kind:      "DaemonSet",
			Name:      "burst-ds",
			Namespace: "default",
		},
	}

	// Call Function
	failedRes := corev1.ResourceCPU
	failedQty := resource.MustParse("100m")

	_, deficits := r.calculateWorkloadDeficit(context.Background(), evt, failedRes, failedQty)

	// Assertions
	// Expected: 3 missing * (100m CPU, 50Mi Mem)
	// CPU: 300m
	// Mem: 150Mi

	assert.Equal(t, int64(300), deficits[corev1.ResourceRequestsCPU], "CPU deficit should be 300m")

	expectedMem := memReq.MilliValue() * 3
	assert.Equal(t, expectedMem, deficits[corev1.ResourceRequestsMemory], "Memory deficit should be 150Mi")
}

func TestCalculateWorkloadDeficit_ReplicaSet_SmartCalculation(t *testing.T) {
	// Setup Scheme
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = appsv1.AddToScheme(scheme)

	// Define Resources
	cpuReq := resource.MustParse("500m")
	memReq := resource.MustParse("200Mi")

	replicas := int32(4)
	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "burst-rs",
			Namespace: "default",
		},
		Spec: appsv1.ReplicaSetSpec{
			Replicas: &replicas,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name: "app",
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
		},
		Status: appsv1.ReplicaSetStatus{
			Replicas: 1, // 3 missing
		},
	}

	// Setup Client
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(rs).Build()
	r := &ResourceQuotaReconciler{
		Client: client,
	}

	// Create Event
	evt := corev1.Event{
		InvolvedObject: corev1.ObjectReference{
			Kind:      "ReplicaSet",
			Name:      "burst-rs",
			Namespace: "default",
		},
	}

	// Call Function
	failedRes := corev1.ResourceMemory
	failedQty := resource.MustParse("200Mi")

	_, deficits := r.calculateWorkloadDeficit(context.Background(), evt, failedRes, failedQty)

	// Assertions
	// Expected: 3 missing * (500m CPU, 200Mi Mem)
	// CPU: 1500m
	// Mem: 600Mi

	expectedCPU := cpuReq.MilliValue() * 3
	assert.Equal(t, expectedCPU, deficits[corev1.ResourceRequestsCPU], "CPU deficit should be 1500m")

	expectedMem := memReq.MilliValue() * 3
	assert.Equal(t, expectedMem, deficits[corev1.ResourceRequestsMemory], "Memory deficit should be 600Mi")
}
