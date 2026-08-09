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

// TestParsePolicy_WarningsCarryTheirKind covers B2: a rejected annotation
// value must be distinguishable from an honoured-but-deprecated one, so a
// caller can give it more than a debug-level log line. A deprecated
// annotation that was actually applied and a rejected one that was ignored
// are not the same kind of thing an operator needs to see.
func TestParsePolicy_WarningsCarryTheirKind(t *testing.T) {
	_, warnings := ParsePolicy(map[string]string{
		"resizer.io/threshold":       "80", // deprecated, honoured
		"resizer.io/max-shrink-step": "25", // rejected: missing the leading "0."
	}, DefaultPolicy())

	if len(warnings) != 2 {
		t.Fatalf("warnings = %v, want exactly two", warnings)
	}

	var sawDeprecated, sawRejected bool
	for _, w := range warnings {
		switch w.Kind {
		case WarningDeprecated:
			sawDeprecated = true
		case WarningRejected:
			sawRejected = true
		default:
			t.Fatalf("warning %+v has an unrecognised kind", w)
		}
		if w.Message == "" {
			t.Fatalf("warning %+v has an empty message", w)
		}
	}
	if !sawDeprecated {
		t.Error("no warning carried WarningDeprecated for the honoured threshold annotation")
	}
	if !sawRejected {
		t.Error("no warning carried WarningRejected for the out-of-range max-shrink-step annotation")
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

func TestParsePolicy_ShrinkOptOutCannotOverrideTheFlag(t *testing.T) {
	// The global flag is expressed as the base policy. A namespace may opt
	// out of shrinking, but it may not opt in when the operator disabled it.
	base := DefaultPolicy()
	base.ShrinkEnabled = false

	p, _ := ParsePolicy(map[string]string{
		"resizer.io/shrink-enabled": "true",
	}, base)

	if p.ShrinkEnabled {
		t.Fatal("namespace annotation enabled shrinking against the global flag")
	}
}

func TestParsePolicy_ShrinkOptOutApplies(t *testing.T) {
	base := DefaultPolicy()
	base.ShrinkEnabled = true

	p, _ := ParsePolicy(map[string]string{
		"resizer.io/shrink-enabled": "false",
	}, base)

	if p.ShrinkEnabled {
		t.Fatal("namespace opt-out was ignored")
	}
}

func TestParsePolicy_ShrinkOptOut_TrueLeavesFlagAlone(t *testing.T) {
	cases := []struct {
		name string
		flag bool
	}{
		{name: "flag on stays on", flag: true},
		{name: "flag off stays off", flag: false}, // the security property
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := DefaultPolicy()
			base.ShrinkEnabled = tc.flag

			p, warnings := ParsePolicy(map[string]string{
				"resizer.io/shrink-enabled": "true",
			}, base)

			if p.ShrinkEnabled != tc.flag {
				t.Fatalf("ShrinkEnabled = %v, want %v (unchanged from the flag)",
					p.ShrinkEnabled, tc.flag)
			}
			if len(warnings) != 0 {
				t.Errorf("warnings = %v, want none", warnings)
			}
		})
	}
}

func TestParsePolicy_ShrinkOptOut_FalseVariantsOptOut(t *testing.T) {
	for _, value := range []string{"false", "False"} {
		t.Run(value, func(t *testing.T) {
			base := DefaultPolicy()
			base.ShrinkEnabled = true

			p, warnings := ParsePolicy(map[string]string{
				"resizer.io/shrink-enabled": value,
			}, base)

			if p.ShrinkEnabled {
				t.Fatalf("value %q did not opt out", value)
			}
			if len(warnings) != 0 {
				t.Errorf("warnings = %v, want none", warnings)
			}
		})
	}
}

func TestParsePolicy_ShrinkOptOut_UnrecognisedValueOptsOutWithWarning(t *testing.T) {
	base := DefaultPolicy()
	base.ShrinkEnabled = true

	p, warnings := ParsePolicy(map[string]string{
		"resizer.io/shrink-enabled": "disabled",
	}, base)

	if p.ShrinkEnabled {
		t.Fatal("unrecognised value did not opt out")
	}
	if len(warnings) != 1 {
		t.Fatalf("warnings = %v, want exactly one", warnings)
	}
}

func TestParsePolicy_ShrinkOptOut_EmptyValueOptsOut(t *testing.T) {
	base := DefaultPolicy()
	base.ShrinkEnabled = true

	p, warnings := ParsePolicy(map[string]string{
		"resizer.io/shrink-enabled": "",
	}, base)

	if p.ShrinkEnabled {
		t.Fatal("empty value did not opt out")
	}
	if len(warnings) != 1 {
		t.Fatalf("warnings = %v, want exactly one", warnings)
	}
}

func TestParsePolicy_RejectsOutOfRangeFractionsWithWarning(t *testing.T) {
	// The two motivating cases: a value written without the leading "0."
	// that a plausible typo would produce. Silently keeping the default is
	// not enough on its own — the operator asked for a tighter cap (or a
	// narrower band) and got none at all, so a warning is mandatory.
	base := DefaultPolicy()

	cases := []struct {
		name  string
		key   string
		value string
		get   func(Policy) float64
		want  float64
	}{
		{
			name:  "tolerance without the leading 0. keeps the default",
			key:   "resizer.io/tolerance",
			value: "15",
			get:   func(p Policy) float64 { return p.Tolerance },
			want:  base.Tolerance,
		},
		{
			name:  "max-shrink-step without the leading 0. keeps the default",
			key:   "resizer.io/max-shrink-step",
			value: "25",
			get:   func(p Policy) float64 { return p.MaxShrinkStep },
			want:  base.MaxShrinkStep,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, warnings := ParsePolicy(map[string]string{tc.key: tc.value}, base)

			if got := tc.get(p); got != tc.want {
				t.Fatalf("value = %v, want default %v (annotation must be rejected, not applied)",
					got, tc.want)
			}
			if len(warnings) != 1 {
				t.Fatalf("warnings = %v, want exactly one", warnings)
			}
		})
	}
}

func TestParsePolicy_RejectsInvalidValuesWithWarning(t *testing.T) {
	base := DefaultPolicy()

	cases := []struct {
		name  string
		key   string
		value string
		get   func(Policy) any
		want  any
	}{
		{"tolerance >= 1", "resizer.io/tolerance", "1",
			func(p Policy) any { return p.Tolerance }, base.Tolerance},
		{"tolerance negative", "resizer.io/tolerance", "-0.1",
			func(p Policy) any { return p.Tolerance }, base.Tolerance},
		{"tolerance not a number", "resizer.io/tolerance", "high",
			func(p Policy) any { return p.Tolerance }, base.Tolerance},
		{"max-shrink-step zero", "resizer.io/max-shrink-step", "0",
			func(p Policy) any { return p.MaxShrinkStep }, base.MaxShrinkStep},
		{"max-shrink-step >= 1", "resizer.io/max-shrink-step", "1",
			func(p Policy) any { return p.MaxShrinkStep }, base.MaxShrinkStep},
		{"cpu-headroom negative", "resizer.io/cpu-headroom", "-0.1",
			func(p Policy) any { return p.HeadroomFor(corev1.ResourceRequestsCPU) },
			base.HeadroomFor(corev1.ResourceRequestsCPU)},
		{"cpu-increment negative", "resizer.io/cpu-increment", "-0.1",
			func(p Policy) any { return p.HeadroomFor(corev1.ResourceRequestsCPU) },
			base.HeadroomFor(corev1.ResourceRequestsCPU)},
		{"cpu-threshold zero", "resizer.io/cpu-threshold", "0",
			func(p Policy) any { return p.HeadroomFor(corev1.ResourceRequestsCPU) },
			base.HeadroomFor(corev1.ResourceRequestsCPU)},
		{"cpu-threshold above 100", "resizer.io/cpu-threshold", "150",
			func(p Policy) any { return p.HeadroomFor(corev1.ResourceRequestsCPU) },
			base.HeadroomFor(corev1.ResourceRequestsCPU)},
		{"window-days zero", "resizer.io/window-days", "0",
			func(p Policy) any { return p.WindowDays }, base.WindowDays},
		{"window-days not a number", "resizer.io/window-days", "abc",
			func(p Policy) any { return p.WindowDays }, base.WindowDays},
		{"shrink-cooldown-days negative", "resizer.io/shrink-cooldown-days", "-1",
			func(p Policy) any { return p.ShrinkCooldown }, base.ShrinkCooldown},
		{"shrink-pr-ttl-days zero", "resizer.io/shrink-pr-ttl-days", "0",
			func(p Policy) any { return p.ShrinkPRTTL }, base.ShrinkPRTTL},
		{"cooldown-minutes negative", "resizer.io/cooldown-minutes", "-1",
			func(p Policy) any { return p.GrowCooldown }, base.GrowCooldown},
		{"requests.cpu-min not a quantity", "resizer.io/requests.cpu-min", "not-a-quantity",
			func(p Policy) any { _, ok := p.MinFor(corev1.ResourceRequestsCPU); return ok }, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, warnings := ParsePolicy(map[string]string{tc.key: tc.value}, base)

			if got := tc.get(p); got != tc.want {
				t.Fatalf("value = %v, want default %v (annotation must be rejected, not applied)",
					got, tc.want)
			}
			if len(warnings) != 1 {
				t.Fatalf("warnings = %v, want exactly one", warnings)
			}
		})
	}
}

func TestMinFor_FamilyFallback(t *testing.T) {
	// resizer.io/cpu-min is the annotation name docs/INSTALLATION.md and
	// docs/OPERATIONS.md give as the example, but "cpu" never matches a
	// quota key exactly — the controller only ever sees keys like
	// requests.cpu. Going through ParsePolicy (rather than seeding
	// Policy.Min with a quota key directly) is what exposes the gap: the
	// exact-match shortcut used elsewhere in this file would hide it.
	p, warnings := ParsePolicy(map[string]string{
		"resizer.io/cpu-min": "2",
	}, DefaultPolicy())
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v, want none", warnings)
	}

	min, ok := p.MinFor(corev1.ResourceRequestsCPU)
	if !ok || min.String() != "2" {
		t.Fatalf("MinFor(requests.cpu) = %v/%v, want 2/true via the cpu family", min.String(), ok)
	}
	if _, ok := p.MinFor(corev1.ResourceRequestsMemory); ok {
		t.Fatal("MinFor(requests.memory) resolved via the cpu family, want none")
	}
}

func TestHeadroomFor_DoesNotMistakeAScopedClaimCountForStorage(t *testing.T) {
	// resourceFamily used to test the raw annotation name with
	// strings.Contains("storage"), which also matches a storage-class scope
	// even though the key it scopes counts claims, not bytes.
	// gold.storageclass.storage.k8s.io/persistentvolumeclaims is exactly
	// that: "storage" appears in the scope, not in what is measured.
	p, _ := ParsePolicy(map[string]string{
		"resizer.io/storage-headroom": "0.9",
	}, DefaultPolicy())

	claimKey := corev1.ResourceName("gold.storageclass.storage.k8s.io/persistentvolumeclaims")
	if got := p.HeadroomFor(claimKey); got != 0.25 {
		t.Errorf("headroom = %v, want the default 0.25 — a claim count is not the storage family", got)
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
