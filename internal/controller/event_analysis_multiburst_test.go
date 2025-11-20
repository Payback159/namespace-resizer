package controller

import (
	"context"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestAnalyzeEvents_MultiBurst(t *testing.T) {
	g := NewWithT(t)

	// Setup Scheme
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)

	// 1. Setup Quota (Fully Used)
	// Hard: 10, Used: 10
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

	// 2. Setup Events & Objects (Liveness Check)

	// Scenario:
	// Workload A (UID 1) tries to schedule a pod needing 2 CPU. Fails.
	// Workload B (UID 2) tries to schedule a pod needing 3 CPU. Fails.
	// Workload A (UID 1) retries. Fails again.

	// We need to create the "Alive" objects for the liveness check to pass.
	podA := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod-a",
			Namespace: "default",
			UID:       types.UID("uid-1"),
		},
	}
	podB := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod-b",
			Namespace: "default",
			UID:       types.UID("uid-2"),
		},
	}

	// Event A1 (UID 1)
	eventA1 := corev1.Event{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "event-a-1",
			Namespace: "default",
		},
		InvolvedObject: corev1.ObjectReference{
			Kind:       "Pod",
			APIVersion: "v1",
			Name:       "pod-a",
			Namespace:  "default",
			UID:        types.UID("uid-1"),
		},
		Type:          corev1.EventTypeWarning,
		Reason:        "FailedCreate",
		Message:       "exceeded quota: test-quota, requested: cpu=2, used: cpu=10, limited: cpu=10",
		LastTimestamp: metav1.Time{Time: time.Now()},
	}

	// Event B (UID 2)
	eventB := corev1.Event{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "event-b",
			Namespace: "default",
		},
		InvolvedObject: corev1.ObjectReference{
			Kind:       "Pod",
			APIVersion: "v1",
			Name:       "pod-b",
			Namespace:  "default",
			UID:        types.UID("uid-2"),
		},
		Type:          corev1.EventTypeWarning,
		Reason:        "FailedCreate",
		Message:       "exceeded quota: test-quota, requested: cpu=3, used: cpu=10, limited: cpu=10",
		LastTimestamp: metav1.Time{Time: time.Now()},
	}

	// Event A2 (UID 1) - Retry
	eventA2 := corev1.Event{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "event-a-2",
			Namespace: "default",
		},
		InvolvedObject: corev1.ObjectReference{
			Kind:       "Pod",
			APIVersion: "v1",
			Name:       "pod-a",
			Namespace:  "default",
			UID:        types.UID("uid-1"),
		},
		Type:          corev1.EventTypeWarning,
		Reason:        "FailedCreate",
		Message:       "exceeded quota: test-quota, requested: cpu=2, used: cpu=10, limited: cpu=10",
		LastTimestamp: metav1.Time{Time: time.Now()},
	}

	// Create Fake Client with these objects
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithLists(&corev1.EventList{Items: []corev1.Event{eventA1, eventB, eventA2}}).
		WithObjects(&podA, &podB). // Add Pods so Liveness Check passes
		Build()

	r := &ResourceQuotaReconciler{
		Client: fakeClient,
	}

	// Config with 0 increment to make math easy
	config := ResizerConfig{
		Thresholds:       map[corev1.ResourceName]float64{"default": 80.0},
		IncrementFactors: map[corev1.ResourceName]float64{"default": 0.0}, // No buffer for this test
		Cooldown:         time.Minute,
	}

	// 3. Run Analysis
	recs, err := r.analyzeEvents(context.TODO(), quota, config)
	g.Expect(err).ToNot(HaveOccurred())

	// 4. Verify
	// Logic:
	// UID 1 Deficit: 2 (Max of 2 and 2)
	// UID 2 Deficit: 3
	// Total Deficit: 5
	// Base Need: Used (10) + Total Deficit (5) = 15

	cpuRec, ok := recs[corev1.ResourceCPU]
	g.Expect(ok).To(BeTrue(), "Should have a CPU recommendation")

	// Check value
	// 15
	g.Expect(cpuRec.Value()).To(Equal(int64(15)), "Should recommend 15 CPU (10 used + 2 for A + 3 for B)")
}
