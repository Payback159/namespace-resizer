package sizing

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

var testNow = time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)

// baseInput builds an Input whose window is fully covered at the given peak,
// so every shrink gate passes unless a test deliberately breaks one.
func baseInput(hard, usedNow, peak string) Input {
	policy := DefaultPolicy()
	return Input{
		Now: testNow,
		Hard: corev1.ResourceList{
			corev1.ResourceRequestsCPU: resource.MustParse(hard),
		},
		Used: corev1.ResourceList{
			corev1.ResourceRequestsCPU: resource.MustParse(usedNow),
		},
		Window: fillWindow(testNow, policy.WindowDays, peak),
		Policy: policy,
	}
}

func targetCPU(t *testing.T, d Decision) string {
	t.Helper()
	qty, ok := d.Targets[corev1.ResourceRequestsCPU]
	if !ok {
		t.Fatalf("no target for requests.cpu, decision = %+v", d)
	}
	return qty.String()
}

func TestDecide_GrowOnDeficit(t *testing.T) {
	in := baseInput("10", "10", "10")
	in.Deficits = map[corev1.ResourceName]int64{
		corev1.ResourceRequestsCPU: 6000,
	}

	got := Decide(in)

	if got.Direction != DirectionGrow {
		t.Fatalf("direction = %v, want grow", got.Direction)
	}
	// peak = used + deficit = 16, target = 16 * 1.25 = 20
	if want := "20"; targetCPU(t, got) != want {
		t.Fatalf("target = %s, want %s", targetCPU(t, got), want)
	}
}

func TestDecide_ShrinkIsStepCapped(t *testing.T) {
	// hard 16, peak 4, used 3.5 -> target 5, but the 25% cap allows only 12.
	in := baseInput("16", "3500m", "4")

	got := Decide(in)

	if got.Direction != DirectionShrink {
		t.Fatalf("direction = %v, want shrink (blocked by %v)", got.Direction, got.BlockedBy)
	}
	if want := "12"; targetCPU(t, got) != want {
		t.Fatalf("target = %s, want %s", targetCPU(t, got), want)
	}
}

func TestDecide_ToleranceBandIsQuiet(t *testing.T) {
	// used = peak = 4, target = 5. The band is hard in [4.35 .. 5.88].
	for _, hard := range []string{"4500m", "5", "5800m"} {
		t.Run("hard="+hard, func(t *testing.T) {
			got := Decide(baseInput(hard, "4", "4"))
			if got.Direction != DirectionNone {
				t.Fatalf("direction = %v, want none for hard=%s", got.Direction, hard)
			}
		})
	}
}

func TestDecide_HardFloorFromCurrentUsage(t *testing.T) {
	// The window says 1 CPU, but 8 CPU are in use right now. The floor wins.
	in := baseInput("16", "8", "1")

	got := Decide(in)

	if got.Direction != DirectionShrink {
		t.Fatalf("direction = %v, want shrink", got.Direction)
	}
	// floor = 8 * 1.25 = 10, which is above the step cap of 12? No: the cap
	// allows 12 and the floor allows 10, so the higher of the two wins.
	if want := "12"; targetCPU(t, got) != want {
		t.Fatalf("target = %s, want %s", targetCPU(t, got), want)
	}
}

func TestDecide_MinAnnotationIsRespected(t *testing.T) {
	in := baseInput("16", "1", "1")
	in.Policy.Min = map[corev1.ResourceName]resource.Quantity{
		corev1.ResourceRequestsCPU: resource.MustParse("14"),
	}

	got := Decide(in)

	if got.Direction != DirectionShrink {
		t.Fatalf("direction = %v, want shrink", got.Direction)
	}
	if want := "14"; targetCPU(t, got) != want {
		t.Fatalf("target = %s, want %s (min annotation)", targetCPU(t, got), want)
	}
}

func TestDecide_GrowBeatsShrink(t *testing.T) {
	policy := DefaultPolicy()
	in := Input{
		Now: testNow,
		Hard: corev1.ResourceList{
			corev1.ResourceRequestsCPU:    resource.MustParse("4"),
			corev1.ResourceRequestsMemory: resource.MustParse("64Gi"),
		},
		Used: corev1.ResourceList{
			corev1.ResourceRequestsCPU:    resource.MustParse("4"),
			corev1.ResourceRequestsMemory: resource.MustParse("1Gi"),
		},
		Window: fillWindow(testNow, policy.WindowDays, "4"),
		Policy: policy,
	}

	got := Decide(in)

	if got.Direction != DirectionGrow {
		t.Fatalf("direction = %v, want grow", got.Direction)
	}
	if _, ok := got.Targets[corev1.ResourceRequestsMemory]; ok {
		t.Fatal("memory shrink target leaked into a grow decision")
	}
}

func TestDecide_ShrinkGates(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Input)
		wantBad Gate
	}{
		{
			name:    "shrink disabled",
			mutate:  func(in *Input) { in.Policy.ShrinkEnabled = false },
			wantBad: GateEnabled,
		},
		{
			name:    "window incomplete",
			mutate:  func(in *Input) { in.Window = fillWindow(testNow, 3, "4") },
			wantBad: GateWindow,
		},
		{
			name:    "grow inside the window",
			mutate:  func(in *Input) { in.LastGrow = testNow.Add(-48 * time.Hour) },
			wantBad: GateRecentGrow,
		},
		{
			name:    "shrink cooldown running",
			mutate:  func(in *Input) { in.LastShrink = testNow.Add(-24 * time.Hour) },
			wantBad: GateCooldown,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := baseInput("16", "3500m", "4")
			tc.mutate(&in)

			got := Decide(in)

			if got.Direction != DirectionNone {
				t.Fatalf("direction = %v, want none", got.Direction)
			}
			found := false
			for _, gate := range got.BlockedBy {
				if gate == tc.wantBad {
					found = true
				}
			}
			if !found {
				t.Fatalf("BlockedBy = %v, want it to contain %q", got.BlockedBy, tc.wantBad)
			}
		})
	}
}

func TestDecide_BlockedShrinkStillReportsAPreview(t *testing.T) {
	in := baseInput("16", "3500m", "4")
	in.Policy.ShrinkEnabled = false

	got := Decide(in)

	if got.Direction != DirectionNone {
		t.Fatalf("direction = %v, want none", got.Direction)
	}
	if len(got.Targets) != 0 {
		t.Fatalf("Targets = %v, want empty when a gate blocks", got.Targets)
	}
	qty, ok := got.ShrinkPreview[corev1.ResourceRequestsCPU]
	if !ok {
		t.Fatal("ShrinkPreview is empty, want the would-be target for dry-run metrics")
	}
	if qty.String() != "12" {
		t.Fatalf("preview = %s, want 12", qty.String())
	}
}

func TestDecide_GatesDoNotBlockGrow(t *testing.T) {
	in := baseInput("10", "10", "10")
	in.Deficits = map[corev1.ResourceName]int64{corev1.ResourceRequestsCPU: 6000}
	in.Policy.ShrinkEnabled = false
	in.LastShrink = testNow.Add(-1 * time.Hour)
	in.Window = Window{Version: WindowVersion}

	got := Decide(in)

	if got.Direction != DirectionGrow {
		t.Fatalf("direction = %v, want grow regardless of shrink gates", got.Direction)
	}
}

func TestDecide_DisabledNamespaceDoesNothing(t *testing.T) {
	in := baseInput("16", "3500m", "4")
	in.Policy.Enabled = false

	if got := Decide(in); got.Direction != DirectionNone {
		t.Fatalf("direction = %v, want none for a disabled namespace", got.Direction)
	}
}

func TestDecide_ZeroHardIsSkipped(t *testing.T) {
	in := baseInput("0", "0", "4")

	if got := Decide(in); got.Direction != DirectionNone {
		t.Fatalf("direction = %v, want none when hard is zero", got.Direction)
	}
}
