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

func TestCollectDeficits_StatefulSet_MassiveScaling(t *testing.T) {
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
	deficits, err := r.collectDeficits(context.TODO(), quota, time.Time{})
	g.Expect(err).ToNot(HaveOccurred())

	// 6. Verify
	// 3 missing replicas, 1 CPU each. All 3 events are retries of the same
	// workload ("web"), so the deficit is the MAX seen for that workload, not
	// the sum: 3 missing * 1 CPU = 3 CPU raw deficit.
	g.Expect(deficits).To(HaveKey(corev1.ResourceCPU))
	g.Expect(deficits[corev1.ResourceCPU]).To(Equal(int64(3000)))
}

func TestCollectDeficits_ReplicaSet_MassiveScaling(t *testing.T) {
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
	deficits, err := r.collectDeficits(context.TODO(), quota, time.Time{})
	g.Expect(err).ToNot(HaveOccurred())

	// 6. Verify
	// 10 missing replicas * 1 CPU = 10 CPU raw deficit.
	g.Expect(deficits).To(HaveKey(corev1.ResourceCPU))
	g.Expect(deficits[corev1.ResourceCPU]).To(Equal(int64(10000)))
}
