package controller

import (
	"strings"
	"testing"

	"github.com/payback159/namespace-resizer/internal/sizing"
	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestRecordDecision_ExposesWasteRatio(t *testing.T) {
	quotaTarget.Reset()
	quotaWasteRatio.Reset()
	shrinkBlockedBy.Reset()

	hard := corev1.ResourceList{
		corev1.ResourceRequestsCPU: resource.MustParse("16"),
	}
	decision := sizing.Decision{
		Direction: sizing.DirectionNone,
		ShrinkPreview: map[corev1.ResourceName]resource.Quantity{
			corev1.ResourceRequestsCPU: resource.MustParse("4"),
		},
		BlockedBy: []sizing.Gate{sizing.GateEnabled},
	}

	recordDecision("team-a", "compute", hard, decision)

	expected := `
# HELP resizer_quota_waste_ratio Current hard limit divided by the computed target.
# TYPE resizer_quota_waste_ratio gauge
resizer_quota_waste_ratio{namespace="team-a",quota="compute",resource="requests.cpu"} 4
`
	err := testutil.CollectAndCompare(
		quotaWasteRatio, strings.NewReader(expected), "resizer_quota_waste_ratio")
	if err != nil {
		t.Fatal(err)
	}

	if got := testutil.ToFloat64(shrinkBlockedBy.WithLabelValues(
		"team-a", "compute", "enabled")); got != 1 {
		t.Fatalf("shrinkBlockedBy{gate=enabled} = %v, want 1", got)
	}
}

func TestRecordDecision_ClearsStaleGates(t *testing.T) {
	shrinkBlockedBy.Reset()
	hard := corev1.ResourceList{
		corev1.ResourceRequestsCPU: resource.MustParse("16"),
	}

	recordDecision("team-a", "compute", hard, sizing.Decision{
		Direction: sizing.DirectionNone,
		BlockedBy: []sizing.Gate{sizing.GateCooldown},
	})

	// Assert the gate went up first. Without this, a recordDecision that did
	// nothing at all would satisfy the assertion below, because reading an
	// unset gauge yields zero.
	blocked := testutil.ToFloat64(shrinkBlockedBy.WithLabelValues(
		"team-a", "compute", "cooldown"))
	if blocked != 1 {
		t.Fatalf("gate = %v after a blocked decision, want 1", blocked)
	}

	recordDecision("team-a", "compute", hard, sizing.Decision{
		Direction: sizing.DirectionGrow,
		Targets: map[corev1.ResourceName]resource.Quantity{
			corev1.ResourceRequestsCPU: resource.MustParse("20"),
		},
	})

	got := testutil.ToFloat64(shrinkBlockedBy.WithLabelValues(
		"team-a", "compute", "cooldown"))
	if got != 0 {
		t.Fatalf("stale gate still set to %v, want 0", got)
	}
}

func TestRecordDecision_ClearsResolvedTargets(t *testing.T) {
	quotaTarget.Reset()
	quotaWasteRatio.Reset()

	hard := corev1.ResourceList{
		corev1.ResourceRequestsCPU: resource.MustParse("16"),
	}

	recordDecision("team-a", "compute", hard, sizing.Decision{
		Direction: sizing.DirectionNone,
		ShrinkPreview: map[corev1.ResourceName]resource.Quantity{
			corev1.ResourceRequestsCPU: resource.MustParse("4"),
		},
		BlockedBy: []sizing.Gate{sizing.GateEnabled},
	})

	if got := testutil.CollectAndCount(quotaWasteRatio); got != 1 {
		t.Fatalf("waste ratio series = %d, want 1 after a shrink preview", got)
	}

	// The quota now tracks demand: no target, so the series must disappear
	// rather than freeze at a waste ratio that is no longer true.
	recordDecision("team-a", "compute", hard, sizing.Decision{
		Direction: sizing.DirectionNone,
	})

	if got := testutil.CollectAndCount(quotaWasteRatio); got != 0 {
		t.Fatalf("waste ratio series = %d, want 0 once the target is gone", got)
	}
	if got := testutil.CollectAndCount(quotaTarget); got != 0 {
		t.Fatalf("target series = %d, want 0 once the target is gone", got)
	}
}
