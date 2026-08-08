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
