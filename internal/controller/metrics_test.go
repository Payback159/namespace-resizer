package controller

import (
	"strings"
	"testing"
	"time"

	"github.com/payback159/namespace-resizer/internal/sizing"
	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// fullyCoveredWindow samples once every 5 minutes across windowDays
// completed days ending yesterday, so every shrink gate but the one a test
// deliberately breaks passes.
func fullyCoveredWindow(now time.Time, windowDays int, cpu string) sizing.Window {
	w := sizing.Window{Version: sizing.WindowVersion}
	start := now.UTC().AddDate(0, 0, -windowDays).Truncate(24 * time.Hour)
	used := corev1.ResourceList{corev1.ResourceRequestsCPU: resource.MustParse(cpu)}
	for t := start; t.Before(now.UTC().Truncate(24 * time.Hour)); t = t.Add(5 * time.Minute) {
		w.Observe(t, "test-uid", used, windowDays)
	}
	return w
}

func TestRecordDecision_ExposesWasteRatio(t *testing.T) {
	quotaTarget.Reset()
	quotaWasteRatio.Reset()
	shrinkBlockedBy.Reset()

	// hard 16, peak 4, used 3.5: Decide's real formula puts the uncapped
	// target at 4 * 1.25 = 5, but the per-PR step cap only allows a drop to
	// hard * 0.75 = 12 (see decide_test.go's TestDecide_ShrinkIsStepCapped,
	// same inputs). Shrinking is switched off here so the gate lets us
	// assert on decision.RawTargets — the shrink preview alone — without the
	// step-capped Targets/ShrinkPreview values leaking into the metric.
	policy := sizing.DefaultPolicy()
	policy.ShrinkEnabled = false
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)

	hard := corev1.ResourceList{
		corev1.ResourceRequestsCPU: resource.MustParse("16"),
	}
	decision := sizing.Decide(sizing.Input{
		Now:    now,
		Hard:   hard,
		Used:   corev1.ResourceList{corev1.ResourceRequestsCPU: resource.MustParse("3500m")},
		Window: fullyCoveredWindow(now, policy.WindowDays, "4"),
		Policy: policy,
	})

	if decision.Direction != sizing.DirectionNone {
		t.Fatalf("direction = %v, want none (shrink blocked by the enabled gate)", decision.Direction)
	}

	recordDecision("team-a", "compute", hard, decision)

	// 16 / 5 = 3.2, not 16 / 12 = 1.333: the waste ratio has to be built on
	// the uncapped target so a 4x oversized namespace stays distinguishable
	// from one that is 40x oversized instead of both saturating at the step
	// cap.
	expected := `
# HELP resizer_quota_waste_ratio Current hard limit divided by the computed target.
# TYPE resizer_quota_waste_ratio gauge
resizer_quota_waste_ratio{namespace="team-a",quota="compute",resource="requests.cpu"} 3.2
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
		RawTargets: map[corev1.ResourceName]resource.Quantity{
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
		RawTargets: map[corev1.ResourceName]resource.Quantity{
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
