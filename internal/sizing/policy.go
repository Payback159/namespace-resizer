package sizing

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// AnnotationPrefix is the namespace annotation prefix all resizer settings use.
const AnnotationPrefix = "resizer.io/"

// DefaultKey is the map key holding the namespace-wide fallback value.
const DefaultKey corev1.ResourceName = "default"

const falseValue = "false"

// Policy is the effective configuration for one quota, after merging global
// defaults with namespace annotations.
type Policy struct {
	Headroom map[corev1.ResourceName]float64
	Min      map[corev1.ResourceName]resource.Quantity

	Tolerance      float64
	WindowDays     int
	MaxShrinkStep  float64
	ShrinkCooldown time.Duration
	ShrinkPRTTL    time.Duration
	GrowCooldown   time.Duration

	Enabled       bool
	ShrinkEnabled bool
}

// DefaultPolicy returns the built-in defaults from spec 7.2.
func DefaultPolicy() Policy {
	return Policy{
		Headroom:       map[corev1.ResourceName]float64{DefaultKey: 0.25},
		Min:            map[corev1.ResourceName]resource.Quantity{},
		Tolerance:      0.15,
		WindowDays:     14,
		MaxShrinkStep:  0.25,
		ShrinkCooldown: 7 * 24 * time.Hour,
		ShrinkPRTTL:    7 * 24 * time.Hour,
		GrowCooldown:   60 * time.Minute,
		Enabled:        true,
		ShrinkEnabled:  true,
	}
}

// HeadroomFor resolves the headroom for a quota key: exact match first, then
// the resource family (cpu/memory/storage), then the namespace default.
func (p Policy) HeadroomFor(res corev1.ResourceName) float64 {
	if v, ok := p.Headroom[res]; ok {
		return v
	}
	if family, ok := resourceFamily(res); ok {
		if v, ok := p.Headroom[family]; ok {
			return v
		}
	}
	if v, ok := p.Headroom[DefaultKey]; ok {
		return v
	}
	return 0.25
}

// MinFor returns the configured absolute lower bound for a quota key.
func (p Policy) MinFor(res corev1.ResourceName) (resource.Quantity, bool) {
	q, ok := p.Min[res]
	return q, ok
}

func resourceFamily(res corev1.ResourceName) (corev1.ResourceName, bool) {
	name := string(res)
	switch {
	case strings.Contains(name, "cpu"):
		return corev1.ResourceCPU, true
	case strings.Contains(name, "memory"):
		return corev1.ResourceMemory, true
	case strings.Contains(name, "storage"):
		return corev1.ResourceStorage, true
	}
	return "", false
}

func parseScalar(name, value string, out *Policy) {
	switch {
	case strings.HasSuffix(name, "-min"):
		if q, err := resource.ParseQuantity(value); err == nil {
			out.Min[corev1.ResourceName(strings.TrimSuffix(name, "-min"))] = q
		}
	case name == "tolerance":
		if v, ok := parseFraction(value); ok {
			out.Tolerance = v
		}
	case name == "max-shrink-step":
		if v, ok := parseFraction(value); ok {
			out.MaxShrinkStep = v
		}
	case name == "window-days":
		if v, err := strconv.Atoi(value); err == nil && v > 0 {
			out.WindowDays = v
		}
	case name == "shrink-cooldown-days":
		if v, err := strconv.Atoi(value); err == nil && v >= 0 {
			out.ShrinkCooldown = time.Duration(v) * 24 * time.Hour
		}
	case name == "shrink-pr-ttl-days":
		if v, err := strconv.Atoi(value); err == nil && v > 0 {
			out.ShrinkPRTTL = time.Duration(v) * 24 * time.Hour
		}
	case name == "cooldown-minutes":
		if v, err := strconv.Atoi(value); err == nil && v >= 0 {
			out.GrowCooldown = time.Duration(v) * time.Minute
		}
	case name == "enabled":
		out.Enabled = value != falseValue
	case name == "shrink-enabled":
		out.ShrinkEnabled = value != falseValue
	}
}

// ParsePolicy merges namespace annotations onto base. It returns the effective
// policy plus one warning per deprecated annotation that was honoured.
func ParsePolicy(annotations map[string]string, base Policy) (Policy, []string) {
	out := base
	out.Headroom = copyFloatMap(base.Headroom)
	out.Min = copyQuantityMap(base.Min)

	// Collected separately so precedence is applied deterministically,
	// independent of Go's random map iteration order.
	fromHeadroom := map[corev1.ResourceName]float64{}
	fromIncrement := map[corev1.ResourceName]float64{}
	fromThreshold := map[corev1.ResourceName]float64{}
	var warnings []string

	for key, value := range annotations {
		if !strings.HasPrefix(key, AnnotationPrefix) {
			continue
		}
		name := strings.TrimPrefix(key, AnnotationPrefix)

		switch {
		case strings.HasSuffix(name, "headroom"):
			if v, ok := parseFraction(value); ok {
				fromHeadroom[suffixKey(name, "headroom")] = v
			}
		case strings.HasSuffix(name, "increment"):
			if v, ok := parseFraction(value); ok {
				fromIncrement[suffixKey(name, "increment")] = v
			}
		case strings.HasSuffix(name, "threshold"):
			if v, err := strconv.ParseFloat(value, 64); err == nil && v > 0 {
				fromThreshold[suffixKey(name, "threshold")] = 100.0/v - 1.0
			}
		default:
			parseScalar(name, value, &out)
		}
	}

	// Precedence: headroom > increment > threshold (spec 7.1). A deprecated
	// annotation only produces a warning when nothing overrode it.
	for res, v := range fromThreshold {
		if _, overridden := fromHeadroom[res]; overridden {
			continue
		}
		if _, overridden := fromIncrement[res]; overridden {
			continue
		}
		out.Headroom[res] = v
		warnings = append(warnings, deprecationWarning(res, "threshold"))
	}
	for res, v := range fromIncrement {
		if _, overridden := fromHeadroom[res]; overridden {
			continue
		}
		out.Headroom[res] = v
		warnings = append(warnings, deprecationWarning(res, "increment"))
	}
	for res, v := range fromHeadroom {
		out.Headroom[res] = v
	}

	return out, warnings
}

// suffixKey turns "cpu-headroom" into "cpu" and a bare "headroom" into the
// namespace default key.
func suffixKey(name, suffix string) corev1.ResourceName {
	trimmed := strings.TrimSuffix(strings.TrimSuffix(name, suffix), "-")
	if trimmed == "" {
		return DefaultKey
	}
	return corev1.ResourceName(trimmed)
}

// parseFraction accepts "0.25" and "25%" and returns a fraction.
func parseFraction(value string) (float64, bool) {
	if strings.HasSuffix(value, "%") {
		v, err := strconv.ParseFloat(strings.TrimSuffix(value, "%"), 64)
		if err != nil {
			return 0, false
		}
		return v / 100.0, true
	}
	v, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

func deprecationWarning(res corev1.ResourceName, old string) string {
	prefix := string(res) + "-"
	if res == DefaultKey {
		prefix = ""
	}
	return fmt.Sprintf(
		"annotation %s%s%s is deprecated, use %s%sheadroom instead",
		AnnotationPrefix, prefix, old, AnnotationPrefix, prefix)
}

func copyFloatMap(in map[corev1.ResourceName]float64) map[corev1.ResourceName]float64 {
	out := make(map[corev1.ResourceName]float64, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func copyQuantityMap(
	in map[corev1.ResourceName]resource.Quantity,
) map[corev1.ResourceName]resource.Quantity {
	out := make(map[corev1.ResourceName]resource.Quantity, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
