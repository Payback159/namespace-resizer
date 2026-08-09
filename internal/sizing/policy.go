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

const (
	trueValue  = "true"
	falseValue = "false"
)

// WarningKind classifies a ParsePolicy warning by how urgently an operator
// needs to see it.
type WarningKind int

const (
	// WarningDeprecated reports an old annotation spelling that was honoured
	// exactly as before. Expected, low-noise: any namespace that has not
	// migrated to the headroom annotations produces one on every reconcile.
	WarningDeprecated WarningKind = iota
	// WarningRejected reports an annotation value that failed validation and
	// was ignored — the field keeps whatever it already held (or, for
	// shrink-enabled, is switched off). This is very likely an operator's
	// mistake having no effect and needs more than debug-level visibility.
	WarningRejected
)

// PolicyWarning is one thing ParsePolicy wants the operator to know about.
type PolicyWarning struct {
	Kind    WarningKind
	Message string
}

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

// MinFor returns the configured absolute lower bound for a quota key: exact
// match first, then the resource family (cpu/memory/storage), same as
// HeadroomFor. An absolute minimum configured as e.g. resizer.io/cpu-min is
// safe to share across requests.cpu and limits.cpu — it is the annotation
// name the docs give as the example, and it never matches a quota key
// exactly.
func (p Policy) MinFor(res corev1.ResourceName) (resource.Quantity, bool) {
	if q, ok := p.Min[res]; ok {
		return q, true
	}
	if family, ok := resourceFamily(res); ok {
		if q, ok := p.Min[family]; ok {
			return q, true
		}
	}
	return resource.Quantity{}, false
}

// resourceFamily classifies a quota key by what it measures, not by its
// full name. measureOf strips any scope prefix first so a storage-class
// scoped claim count such as
// "gold.storageclass.storage.k8s.io/persistentvolumeclaims" — whose scope
// contains the word "storage" while the key itself counts claims — is not
// mistaken for the storage family.
func resourceFamily(res corev1.ResourceName) (corev1.ResourceName, bool) {
	name := measureOf(res)
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

// parseScalar applies one non-headroom, non-increment, non-threshold
// annotation and returns a warning, whose Message is empty when there is
// nothing to say. A value that fails to parse or falls outside the
// annotation's valid range is rejected: the field keeps its current value
// (the default, unless a global flag already set it) and the rejection is
// reported as a WarningRejected warning rather than silently dropped, so a
// plausible-looking typo does not read to the operator as "no annotation
// was written".
func parseScalar(name, value string, out *Policy) PolicyWarning {
	switch {
	case strings.HasSuffix(name, "-min"):
		if q, err := resource.ParseQuantity(value); err == nil {
			out.Min[corev1.ResourceName(strings.TrimSuffix(name, "-min"))] = q
			return PolicyWarning{}
		}
		return rejectionWarning(name, value, "must be a valid resource quantity")
	case name == "tolerance":
		if v, ok := parseFraction(value); ok && v >= 0 && v < 1 {
			out.Tolerance = v
			return PolicyWarning{}
		}
		return rejectionWarning(name, value, "must be a fraction in [0, 1), e.g. \"0.15\" or \"15%\"")
	case name == "max-shrink-step":
		if v, ok := parseFraction(value); ok && v > 0 && v < 1 {
			out.MaxShrinkStep = v
			return PolicyWarning{}
		}
		return rejectionWarning(name, value, "must be a fraction in (0, 1), e.g. \"0.25\" or \"25%\"")
	case name == "window-days":
		if v, err := strconv.Atoi(value); err == nil && v > 0 {
			out.WindowDays = v
			return PolicyWarning{}
		}
		return rejectionWarning(name, value, "must be a positive integer")
	case name == "shrink-cooldown-days":
		if v, err := strconv.Atoi(value); err == nil && v >= 0 {
			out.ShrinkCooldown = time.Duration(v) * 24 * time.Hour
			return PolicyWarning{}
		}
		return rejectionWarning(name, value, "must be a non-negative integer")
	case name == "shrink-pr-ttl-days":
		if v, err := strconv.Atoi(value); err == nil && v > 0 {
			out.ShrinkPRTTL = time.Duration(v) * 24 * time.Hour
			return PolicyWarning{}
		}
		return rejectionWarning(name, value, "must be a positive integer")
	case name == "cooldown-minutes":
		if v, err := strconv.Atoi(value); err == nil && v >= 0 {
			out.GrowCooldown = time.Duration(v) * time.Minute
			return PolicyWarning{}
		}
		return rejectionWarning(name, value, "must be a non-negative integer")
	case name == "enabled":
		out.Enabled = value != falseValue
	case name == "shrink-enabled":
		return parseShrinkOptOut(value, out)
	}
	return PolicyWarning{}
}

// rejectionWarning reports an annotation value that failed validation. The
// field it targeted keeps whatever it already held.
func rejectionWarning(name, value, requirement string) PolicyWarning {
	return PolicyWarning{
		Kind: WarningRejected,
		Message: fmt.Sprintf(
			"annotation %s%s has the invalid value %q, so it is ignored and the previous value is kept (%s)",
			AnnotationPrefix, name, value, requirement),
	}
}

// parseShrinkOptOut applies the shrink-enabled annotation, which is opt-out
// only: a namespace may switch shrinking off, never on. Enabling it is the
// operator's decision, made with the flag.
//
// Only an explicit "true" leaves shrinking as the flag left it. Every other
// value switches it off, because the sole reason to write this annotation is
// to opt out, and a spelling we do not recognise is far likelier to be an
// attempt at that than a request to keep shrinking on. An unrecognised value
// is a WarningRejected warning, same as any other invalid annotation value —
// it silently changed the effective policy (shrinking is now off) and an
// operator relying on shrinking needs to see that, not a debug line.
func parseShrinkOptOut(value string, out *Policy) PolicyWarning {
	switch {
	case strings.EqualFold(value, trueValue):
		return PolicyWarning{}
	case strings.EqualFold(value, falseValue):
		out.ShrinkEnabled = false
		return PolicyWarning{}
	default:
		out.ShrinkEnabled = false
		return PolicyWarning{
			Kind: WarningRejected,
			Message: fmt.Sprintf("annotation %sshrink-enabled has the unrecognised "+
				"value %q, so shrinking is switched off for this namespace; "+
				"use \"true\" or \"false\"", AnnotationPrefix, value),
		}
	}
}

// ParsePolicy merges namespace annotations onto base. It returns the effective
// policy plus a warning for every annotation worth telling the operator about:
// one per deprecated annotation that was honoured, and one per annotation
// value that failed validation and was rejected (including an unrecognised
// shrink-enabled value). Each warning's Kind says which of the two it is —
// see WarningDeprecated and WarningRejected.
func ParsePolicy(annotations map[string]string, base Policy) (Policy, []PolicyWarning) {
	out := base
	out.Headroom = copyFloatMap(base.Headroom)
	out.Min = copyQuantityMap(base.Min)

	// Collected separately so precedence is applied deterministically,
	// independent of Go's random map iteration order.
	fromHeadroom := map[corev1.ResourceName]float64{}
	fromIncrement := map[corev1.ResourceName]float64{}
	fromThreshold := map[corev1.ResourceName]float64{}
	var warnings []PolicyWarning

	for key, value := range annotations {
		if !strings.HasPrefix(key, AnnotationPrefix) {
			continue
		}
		name := strings.TrimPrefix(key, AnnotationPrefix)

		switch {
		case strings.HasSuffix(name, "headroom"):
			if v, ok := parseFraction(value); ok && v >= 0 {
				fromHeadroom[suffixKey(name, "headroom")] = v
			} else {
				warnings = append(warnings, rejectionWarning(name, value,
					"must be a fraction >= 0, e.g. \"0.25\" or \"25%\""))
			}
		case strings.HasSuffix(name, "increment"):
			if v, ok := parseFraction(value); ok && v >= 0 {
				fromIncrement[suffixKey(name, "increment")] = v
			} else {
				warnings = append(warnings, rejectionWarning(name, value,
					"must be a fraction >= 0, e.g. \"0.2\" or \"20%\""))
			}
		case strings.HasSuffix(name, "threshold"):
			if v, err := strconv.ParseFloat(value, 64); err == nil && v > 0 && v <= 100 {
				fromThreshold[suffixKey(name, "threshold")] = 100.0/v - 1.0
			} else {
				warnings = append(warnings, rejectionWarning(name, value,
					"must be a percentage in (0, 100]"))
			}
		default:
			if w := parseScalar(name, value, &out); w.Message != "" {
				warnings = append(warnings, w)
			}
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

func deprecationWarning(res corev1.ResourceName, old string) PolicyWarning {
	prefix := string(res) + "-"
	if res == DefaultKey {
		prefix = ""
	}
	return PolicyWarning{
		Kind: WarningDeprecated,
		Message: fmt.Sprintf(
			"annotation %s%s%s is deprecated, use %s%sheadroom instead",
			AnnotationPrefix, prefix, old, AnnotationPrefix, prefix),
	}
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
