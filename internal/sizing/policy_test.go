package sizing

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
)

func TestParsePolicy_Defaults(t *testing.T) {
	p, warnings := ParsePolicy(nil, DefaultPolicy())

	if got := p.HeadroomFor(corev1.ResourceRequestsCPU); got != 0.25 {
		t.Errorf("headroom = %v, want 0.25", got)
	}
	if p.Tolerance != 0.15 {
		t.Errorf("tolerance = %v, want 0.15", p.Tolerance)
	}
	if p.WindowDays != 14 {
		t.Errorf("windowDays = %d, want 14", p.WindowDays)
	}
	if p.ShrinkCooldown != 7*24*time.Hour {
		t.Errorf("shrinkCooldown = %v, want 168h", p.ShrinkCooldown)
	}
	if p.MaxShrinkStep != 0.25 {
		t.Errorf("maxShrinkStep = %v, want 0.25", p.MaxShrinkStep)
	}
	if p.GrowCooldown != 60*time.Minute {
		t.Errorf("growCooldown = %v, want 60m", p.GrowCooldown)
	}
	if !p.Enabled || !p.ShrinkEnabled {
		t.Errorf("enabled = %v, shrinkEnabled = %v, want true/true",
			p.Enabled, p.ShrinkEnabled)
	}
	if len(warnings) != 0 {
		t.Errorf("warnings = %v, want none", warnings)
	}
}

func TestParsePolicy_MigrationChain(t *testing.T) {
	cases := []struct {
		name        string
		annotations map[string]string
		res         corev1.ResourceName
		want        float64
		wantWarning bool
	}{
		{
			name: "headroom wins over increment and threshold",
			annotations: map[string]string{
				"resizer.io/cpu-headroom":  "0.4",
				"resizer.io/cpu-increment": "0.2",
				"resizer.io/cpu-threshold": "80",
			},
			res:  corev1.ResourceRequestsCPU,
			want: 0.4,
		},
		{
			name:        "increment is taken verbatim",
			annotations: map[string]string{"resizer.io/cpu-increment": "0.2"},
			res:         corev1.ResourceRequestsCPU,
			want:        0.2,
			wantWarning: true,
		},
		{
			name:        "percent suffix on increment",
			annotations: map[string]string{"resizer.io/cpu-increment": "20%"},
			res:         corev1.ResourceRequestsCPU,
			want:        0.2,
			wantWarning: true,
		},
		{
			name:        "threshold 80 derives headroom 0.25",
			annotations: map[string]string{"resizer.io/memory-threshold": "80"},
			res:         corev1.ResourceRequestsMemory,
			want:        0.25,
			wantWarning: true,
		},
		{
			name:        "bare headroom applies as default",
			annotations: map[string]string{"resizer.io/headroom": "0.5"},
			res:         corev1.ResourceName("hugepages-2Mi"),
			want:        0.5,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, warnings := ParsePolicy(tc.annotations, DefaultPolicy())
			got := p.HeadroomFor(tc.res)
			if diff := got - tc.want; diff > 1e-9 || diff < -1e-9 {
				t.Fatalf("HeadroomFor(%s) = %v, want %v", tc.res, got, tc.want)
			}
			if tc.wantWarning && len(warnings) == 0 {
				t.Errorf("expected a deprecation warning, got none")
			}
			if !tc.wantWarning && len(warnings) != 0 {
				t.Errorf("unexpected warnings: %v", warnings)
			}
		})
	}
}

func TestParsePolicy_ScalarsAndMin(t *testing.T) {
	p, _ := ParsePolicy(map[string]string{
		"resizer.io/tolerance":            "0.1",
		"resizer.io/window-days":          "30",
		"resizer.io/shrink-cooldown-days": "14",
		"resizer.io/max-shrink-step":      "15%",
		"resizer.io/shrink-pr-ttl-days":   "3",
		"resizer.io/cooldown-minutes":     "120",
		"resizer.io/enabled":              "false",
		"resizer.io/shrink-enabled":       "false",
		"resizer.io/requests.cpu-min":     "2",
	}, DefaultPolicy())

	if p.Tolerance != 0.1 {
		t.Errorf("tolerance = %v, want 0.1", p.Tolerance)
	}
	if p.WindowDays != 30 {
		t.Errorf("windowDays = %d, want 30", p.WindowDays)
	}
	if p.ShrinkCooldown != 14*24*time.Hour {
		t.Errorf("shrinkCooldown = %v, want 336h", p.ShrinkCooldown)
	}
	if p.MaxShrinkStep != 0.15 {
		t.Errorf("maxShrinkStep = %v, want 0.15", p.MaxShrinkStep)
	}
	if p.ShrinkPRTTL != 3*24*time.Hour {
		t.Errorf("shrinkPRTTL = %v, want 72h", p.ShrinkPRTTL)
	}
	if p.GrowCooldown != 120*time.Minute {
		t.Errorf("growCooldown = %v, want 120m", p.GrowCooldown)
	}
	if p.Enabled || p.ShrinkEnabled {
		t.Errorf("enabled = %v, shrinkEnabled = %v, want false/false",
			p.Enabled, p.ShrinkEnabled)
	}
	min, ok := p.MinFor(corev1.ResourceRequestsCPU)
	if !ok || min.String() != "2" {
		t.Errorf("MinFor(requests.cpu) = %v/%v, want 2/true", min.String(), ok)
	}
	if _, ok := p.MinFor(corev1.ResourceRequestsMemory); ok {
		t.Errorf("MinFor(requests.memory) reported a value, want none")
	}
}

func TestHeadroomFor_FamilyFallback(t *testing.T) {
	p, _ := ParsePolicy(map[string]string{
		"resizer.io/cpu-headroom": "0.6",
	}, DefaultPolicy())

	if got := p.HeadroomFor(corev1.ResourceLimitsCPU); got != 0.6 {
		t.Errorf("limits.cpu headroom = %v, want 0.6 via cpu family", got)
	}
	if got := p.HeadroomFor(corev1.ResourceRequestsMemory); got != 0.25 {
		t.Errorf("requests.memory headroom = %v, want 0.25 default", got)
	}
}
