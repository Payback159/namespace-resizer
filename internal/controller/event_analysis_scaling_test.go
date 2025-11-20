package controller

import (
	"context"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	"github.com/payback159/namespace-resizer/internal/lock"
	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestAnalyzeEvents_StatefulSet_MassiveScaling(t *testing.T) {
	g := NewWithT(t)

	// Setup Scheme
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = appsv1.AddToScheme(scheme)
	_ = coordinationv1.AddToScheme(scheme)

	// 1. Setup Quota (Fully Used)
	// Limit: 10, Used: 10
	quota := corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-quota",
			Namespace: "default",
		},
		Status: corev1.ResourceQuotaStatus{
			Hard: corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse("10"),
			},
			Used: corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse("10"),
			},
		},
	}

	// 2. Setup StatefulSet
	// We need the StatefulSet to exist for the liveness check.
	sts := appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "web",
			Namespace: "default",
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: func(i int32) *int32 { return &i }(3),
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name: "nginx",
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU: resource.MustParse("1"),
								},
							},
						},
					},
				},
			},
		},
	}

	// 3. Setup Events
	events := []corev1.Event{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "evt-1", Namespace: "default"},
			InvolvedObject: corev1.ObjectReference{
				Kind:       "StatefulSet",
				Name:       "web",
				Namespace:  "default",
				APIVersion: "apps/v1",
			},
			Type:          corev1.EventTypeWarning,
			Reason:        "FailedCreate",
			Message:       "pods \"web-0\" is forbidden: exceeded quota: test-quota, requested: cpu=1, used: cpu=10, limited: cpu=10",
			LastTimestamp: metav1.Time{Time: time.Now()},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "evt-2", Namespace: "default"},
			InvolvedObject: corev1.ObjectReference{
				Kind:       "StatefulSet",
				Name:       "web",
				Namespace:  "default",
				APIVersion: "apps/v1",
			},
			Type:          corev1.EventTypeWarning,
			Reason:        "FailedCreate",
			Message:       "pods \"web-1\" is forbidden: exceeded quota: test-quota, requested: cpu=1, used: cpu=10, limited: cpu=10",
			LastTimestamp: metav1.Time{Time: time.Now()},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "evt-3", Namespace: "default"},
			InvolvedObject: corev1.ObjectReference{
				Kind:       "StatefulSet",
				Name:       "web",
				Namespace:  "default",
				APIVersion: "apps/v1",
			},
			Type:          corev1.EventTypeWarning,
			Reason:        "FailedCreate",
			Message:       "pods \"web-2\" is forbidden: exceeded quota: test-quota, requested: cpu=1, used: cpu=10, limited: cpu=10",
			LastTimestamp: metav1.Time{Time: time.Now()},
		},
	}

	// 4. Setup Reconciler
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(&quota, &sts, &events[0], &events[1], &events[2]).
		Build()

	r := &ResourceQuotaReconciler{
		Client: fakeClient,
		Locker: lock.NewLeaseLocker(fakeClient),
	}

	// 5. Run Analysis
	config := ResizerConfig{
		Thresholds:       map[corev1.ResourceName]float64{corev1.ResourceCPU: 80},
		IncrementFactors: map[corev1.ResourceName]float64{corev1.ResourceCPU: 0.0}, // 0 buffer for exact calculation check
		Cooldown:         time.Minute,
	}

	recs, err := r.analyzeEvents(context.TODO(), quota, config)
	g.Expect(err).ToNot(HaveOccurred())

	// 6. Verify
	// Used: 10
	// Deficit: 3 (1 per pod)
	// Total Needed: 13
	// Buffer: 0
	// Recommendation: 13

	g.Expect(recs).To(HaveKey(corev1.ResourceCPU))
	val := recs[corev1.ResourceCPU]
	g.Expect(val.String()).To(Equal("13"))
}

func TestAnalyzeEvents_ReplicaSet_MassiveScaling(t *testing.T) {
	g := NewWithT(t)

	// Setup Scheme
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = appsv1.AddToScheme(scheme)
	_ = coordinationv1.AddToScheme(scheme)

	// 1. Setup Quota (Fully Used)
	// Limit: 10, Used: 10
	quota := corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-quota",
			Namespace: "default",
		},
		Status: corev1.ResourceQuotaStatus{
			Hard: corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse("10"),
			},
			Used: corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse("10"),
			},
		},
	}

	// 2. Setup ReplicaSet
	// Desired: 10, Current: 0 (Massive scale up blocked)
	rs := appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "app-v1",
			Namespace: "default",
		},
		Spec: appsv1.ReplicaSetSpec{
			Replicas: func(i int32) *int32 { return &i }(10),
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name: "app",
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU: resource.MustParse("1"),
								},
							},
						},
					},
				},
			},
		},
		Status: appsv1.ReplicaSetStatus{
			Replicas: 0,
		},
	}

	// 3. Setup Event
	// Single event for one pod failure.
	// Request: 1 CPU.
	// Logic should see 10 missing pods and multiply deficit: 1 * 10 = 10.
	event := corev1.Event{
		ObjectMeta: metav1.ObjectMeta{Name: "evt-1", Namespace: "default"},
		InvolvedObject: corev1.ObjectReference{
			Kind:       "ReplicaSet",
			Name:       "app-v1",
			Namespace:  "default",
			APIVersion: "apps/v1",
		},
		Type:          corev1.EventTypeWarning,
		Reason:        "FailedCreate",
		Message:       "exceeded quota: test-quota, requested: cpu=1, used: cpu=10, limited: cpu=10",
		LastTimestamp: metav1.Time{Time: time.Now()},
	}

	// 4. Setup Reconciler
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(&quota, &rs, &event).
		Build()

	r := &ResourceQuotaReconciler{
		Client: fakeClient,
		Locker: lock.NewLeaseLocker(fakeClient),
	}

	// 5. Run Analysis
	config := ResizerConfig{
		Thresholds:       map[corev1.ResourceName]float64{corev1.ResourceCPU: 80},
		IncrementFactors: map[corev1.ResourceName]float64{corev1.ResourceCPU: 0.0}, // 0 buffer
		Cooldown:         time.Minute,
	}

	recs, err := r.analyzeEvents(context.TODO(), quota, config)
	g.Expect(err).ToNot(HaveOccurred())

	// 6. Verify
	// Used: 10
	// Deficit: 10 (1 * 10 missing pods)
	// Total Needed: 20
	// Buffer: 0
	// Recommendation: 20

	g.Expect(recs).To(HaveKey(corev1.ResourceCPU))
	val := recs[corev1.ResourceCPU]
	g.Expect(val.String()).To(Equal("20"))
}
