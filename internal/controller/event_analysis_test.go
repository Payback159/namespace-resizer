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
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestAnalyzeEvents_Concurrency(t *testing.T) {
	g := NewWithT(t)

	// Setup Scheme
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)

	// 1. Setup Quota (Fully Used)
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

	// 2. Setup Events
	// We want to ensure the LARGE request is processed FIRST, then the SMALL one.
	// If the controller just overwrites, the small one will win (bug).
	// Fake client might sort by name. So we name the large one "a" and small one "b".

	// Event Large: Needs 5 CPU (Total 15)
	eventLarge := corev1.Event{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "event-a-large",
			Namespace: "default",
		},
		Type:          corev1.EventTypeWarning,
		Reason:        "FailedCreate",
		Message:       "exceeded quota: test-quota, requested: cpu=5, used: cpu=10, limited: cpu=10",
		LastTimestamp: metav1.Time{Time: time.Now()},
	}

	// Event Small: Needs 2 CPU (Total 12)
	eventSmall := corev1.Event{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "event-b-small",
			Namespace: "default",
		},
		Type:          corev1.EventTypeWarning,
		Reason:        "FailedCreate",
		Message:       "exceeded quota: test-quota, requested: cpu=2, used: cpu=10, limited: cpu=10",
		LastTimestamp: metav1.Time{Time: time.Now()},
	}

	// Create Fake Client with these objects
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithLists(&corev1.EventList{Items: []corev1.Event{eventLarge, eventSmall}}).
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
	// We expect the recommendation to be the MAX of the needs.
	// Need A = 10 + 2 = 12
	// Need B = 10 + 5 = 15
	// Expected: 15

	cpuRec, ok := recs[corev1.ResourceCPU]
	g.Expect(ok).To(BeTrue(), "Should have a CPU recommendation")

	// Check value
	// We expect 15. If the bug exists, it might be 12 (if A is processed last) or 15 (if B is processed last).
	// Since the fake client list order might be stable, let's see.
	// To be sure we catch the bug, we might need to ensure B is processed before A, or just assert it is 15.

	g.Expect(cpuRec.Value()).To(Equal(int64(15)), "Should recommend the maximum needed amount (15)")
}

func TestAnalyzeEvents_Memory(t *testing.T) {
	g := NewWithT(t)

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)

	// 1. Setup Quota (Fully Used Memory)
	quota := corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "mem-quota",
			Namespace: "default",
		},
		Status: corev1.ResourceQuotaStatus{
			Hard: corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse("1Gi"),
			},
			Used: corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse("1Gi"),
			},
		},
	}

	// 2. Setup Event (Memory Burst)
	// "requested: memory=512Mi"
	eventMem := corev1.Event{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "event-mem",
			Namespace: "default",
		},
		Type:          corev1.EventTypeWarning,
		Reason:        "FailedCreate",
		Message:       "exceeded quota: mem-quota, requested: memory=512Mi, used: memory=1Gi, limited: memory=1Gi",
		LastTimestamp: metav1.Time{Time: time.Now()},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithLists(&corev1.EventList{Items: []corev1.Event{eventMem}}).
		Build()

	r := &ResourceQuotaReconciler{
		Client: fakeClient,
	}

	config := ResizerConfig{
		Thresholds:       map[corev1.ResourceName]float64{"default": 80.0},
		IncrementFactors: map[corev1.ResourceName]float64{"default": 0.0},
		Cooldown:         time.Minute,
	}

	// 3. Run Analysis
	recs, err := r.analyzeEvents(context.TODO(), quota, config)
	g.Expect(err).ToNot(HaveOccurred())

	// 4. Verify
	// Used 1Gi + Req 512Mi = 1.5Gi
	memRec, ok := recs[corev1.ResourceMemory]
	g.Expect(ok).To(BeTrue(), "Should have a Memory recommendation")

	expected := resource.MustParse("1536Mi") // 1024 + 512
	g.Expect(memRec.Equal(expected)).To(BeTrue(), "Expected %s, got %s", expected.String(), memRec.String())
}
