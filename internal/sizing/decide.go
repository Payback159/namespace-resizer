package sizing

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// maxMilliValue is the largest Value() a Quantity can carry without its
// MilliValue() wrapping instead of clamping. Verified empirically against
// the pinned k8s.io/apimachinery v0.36.1: AsScaledInt64 discards the ok flag
// from a scale conversion that overflows int64, so ScaledValue silently
// returns a wrapped (and possibly negative) product above this threshold
// (16Pi.Value()=18014398509481984, MilliValue()=-432345564227567616).
const maxMilliValue = math.MaxInt64 / 1000

// overflowsMilliValue reports whether q is too large for MilliValue() to
// convert safely. "Too large" has two distinct shapes: a Value() above
// maxMilliValue, where MilliValue() wraps (see above); and, at a much
// higher magnitude, a Value() that has itself saturated to exactly 0
// instead of wrapping (verified empirically:
// resource.MustParse("1E30").Value()==0, .IsZero()==false). The second case
// would pass a plain "Value() > maxMilliValue" check undetected and then
// read as an honestly zero quantity — for a hard limit that reads as
// "resource not in this quota" and is harmless, but for a used value it
// reads as "fully idle" and can drive a shrink off garbage.
func overflowsMilliValue(q resource.Quantity) bool {
	if q.Value() > maxMilliValue {
		return true
	}
	return q.Value() == 0 && !q.IsZero()
}

// Direction is the kind of change a Decision asks for.
type Direction int

const (
	// DirectionNone means the quota is inside the tolerance band, or a shrink
	// was blocked by a gate.
	DirectionNone Direction = iota
	// DirectionGrow raises limits. It always wins over a pending shrink.
	DirectionGrow
	// DirectionShrink lowers limits and is never auto-merged.
	DirectionShrink
)

func (d Direction) String() string {
	switch d {
	case DirectionGrow:
		return "grow"
	case DirectionShrink:
		return "shrink"
	default:
		return "none"
	}
}

// Gate names a precondition that must hold before a shrink is proposed.
type Gate string

const (
	// GateEnabled covers the global flag and the namespace opt-out.
	GateEnabled Gate = "enabled"
	// GateWindow requires a fully covered observation window.
	GateWindow Gate = "window"
	// GateRecentGrow blocks a shrink that follows a grow inside the window.
	GateRecentGrow Gate = "recent-grow"
	// GateCooldown enforces the long shrink cooldown.
	GateCooldown Gate = "cooldown"
)

// Input is everything Decide needs. It carries no client and no ambient clock.
type Input struct {
	Now  time.Time
	Hard corev1.ResourceList
	Used corev1.ResourceList
	// Deficits holds per-resource milli-values derived from FailedCreate
	// events, or nil when no event is pending.
	Deficits   map[corev1.ResourceName]int64
	Window     Window
	Policy     Policy
	LastGrow   time.Time
	LastShrink time.Time
}

// Decision is the result of one evaluation.
type Decision struct {
	Direction Direction
	// Targets is what the caller may act on. It is empty unless Direction is
	// Grow or Shrink.
	Targets map[corev1.ResourceName]resource.Quantity
	// ShrinkPreview holds the step-capped shrink target for every resource
	// evaluated as a shrink candidate — the same value Targets would carry
	// if the shrink went ahead — populated even when a gate in BlockedBy
	// stops it from being proposed. It answers "what would this PR look
	// like" for a namespace stuck behind a gate, which RawTargets cannot:
	// RawTargets is the uncapped target and does not reflect the step cap a
	// real PR would be bound by. Nothing in this codebase reads it outside
	// tests today — recordDecision (controller/metrics.go) moved to
	// RawTargets for the metrics — but the value it carries is not
	// derivable from RawTargets, so it stays. Never act on it directly —
	// only Targets is authoritative for a pull request.
	ShrinkPreview map[corev1.ResourceName]resource.Quantity
	// RawTargets is the uncapped target for every resource Decide evaluated,
	// independent of the per-PR step cap and the tolerance band. It exists to
	// drive resizer_quota_target/resizer_quota_waste_ratio (see
	// controller/metrics.go): a step-capped shrink candidate saturates at
	// hard * (1 - max-shrink-step) and cannot tell a 4x oversized namespace
	// from a 40x one, but the uncapped target can. Never act on it for a pull
	// request — Targets/ShrinkPreview stay authoritative there.
	RawTargets map[corev1.ResourceName]resource.Quantity
	Reason     string
	BlockedBy  []Gate
}

// Decide computes the target for every quota key and folds the per-resource
// candidates into a single direction. Growth always wins: a quota that needs
// to grow anywhere discards all shrink candidates, because a PR that raises
// one resource while lowering another cannot be reviewed or merged sensibly.
func Decide(in Input) Decision {
	if !in.Policy.Enabled {
		return Decision{Direction: DirectionNone}
	}

	growTargets := map[corev1.ResourceName]resource.Quantity{}
	shrinkTargets := map[corev1.ResourceName]resource.Quantity{}
	rawTargets := map[corev1.ResourceName]resource.Quantity{}
	var growReasons, shrinkReasons []string

	for res, hard := range in.Hard {
		if overflowsMilliValue(hard) {
			// hard.MilliValue() would wrap or, at a higher magnitude, has
			// already saturated to a false zero; treat the resource as
			// unmeasurable rather than act on garbage.
			continue
		}
		hardMilli := hard.MilliValue()
		if hardMilli == 0 {
			continue
		}

		used, ok := in.Used[res]
		if !ok {
			continue
		}
		if overflowsMilliValue(used) {
			continue
		}
		usedMilli := used.MilliValue()

		headroom := in.Policy.HeadroomFor(res)
		targetMilli, driver := targetFor(in, res, usedMilli, headroom)

		// Recorded before the step cap and the tolerance band below touch
		// targetMilli, so it stays the uncapped target described on
		// RawTargets — the metrics need it; PR proposals do not.
		rawTargets[res] = Quantize(res, targetMilli, hard.Format)

		switch {
		case float64(targetMilli) > float64(hardMilli)*(1+in.Policy.Tolerance):
			qty := Quantize(res, targetMilli, hard.Format)
			if qty.Cmp(hard) <= 0 {
				// Rounding erased the increase; nothing to propose.
				continue
			}
			growTargets[res] = qty
			growReasons = append(growReasons,
				describe(res, hard, qty, driver))

		case float64(targetMilli) < float64(hardMilli)*(1-in.Policy.Tolerance):
			capped := int64(float64(hardMilli) * (1 - in.Policy.MaxShrinkStep))
			if capped > targetMilli {
				targetMilli = capped
				driver = "step cap"
			}
			qty := Quantize(res, targetMilli, hard.Format)
			if qty.Cmp(hard) >= 0 {
				// Rounding erased the decrease; nothing to propose.
				continue
			}
			shrinkTargets[res] = qty
			shrinkReasons = append(shrinkReasons,
				describe(res, hard, qty, driver))
		}
	}

	if len(growTargets) > 0 {
		sort.Strings(growReasons)
		return Decision{
			Direction:  DirectionGrow,
			Targets:    growTargets,
			Reason:     strings.Join(growReasons, "\n"),
			RawTargets: rawTargets,
		}
	}

	if len(shrinkTargets) == 0 {
		return Decision{Direction: DirectionNone, RawTargets: rawTargets}
	}

	if blocked := shrinkGates(in, shrinkTargets); len(blocked) > 0 {
		return Decision{
			Direction:     DirectionNone,
			ShrinkPreview: shrinkTargets,
			BlockedBy:     blocked,
			RawTargets:    rawTargets,
		}
	}

	sort.Strings(shrinkReasons)
	return Decision{
		Direction:     DirectionShrink,
		Targets:       shrinkTargets,
		ShrinkPreview: shrinkTargets,
		Reason:        strings.Join(shrinkReasons, "\n"),
		RawTargets:    rawTargets,
	}
}

// targetFor implements the formula from spec 3 and reports which term decided
// the outcome, for the PR body.
func targetFor(
	in Input,
	res corev1.ResourceName,
	usedMilli int64,
	headroom float64,
) (int64, string) {
	peakMilli := usedMilli
	driver := "current usage"

	if p, ok := in.Window.Peak(res, in.Now, in.Policy.WindowDays); ok && p > peakMilli {
		peakMilli = p
		driver = fmt.Sprintf("%d-day peak", in.Policy.WindowDays)
	}
	if deficit, ok := in.Deficits[res]; ok && deficit > 0 {
		if need := usedMilli + deficit; need > peakMilli {
			peakMilli = need
			driver = "pending shortage"
		}
	}

	target := int64(float64(peakMilli) * (1 + headroom))

	if lowerBound := int64(float64(usedMilli) * (1 + headroom)); lowerBound > target {
		target = lowerBound
		driver = "current usage floor"
	}
	if floor, ok := in.Policy.MinFor(res); ok && floor.MilliValue() > target {
		target = floor.MilliValue()
		driver = "configured minimum"
	}

	return target, driver
}

// shrinkGates returns every gate from spec 3.3 that currently blocks a shrink.
// The lock gate is enforced by the reconciler, which owns the Lease.
func shrinkGates(in Input, targets map[corev1.ResourceName]resource.Quantity) []Gate {
	var blocked []Gate

	if !in.Policy.ShrinkEnabled {
		blocked = append(blocked, GateEnabled)
	}
	for res := range targets {
		if !in.Window.IsComplete(res, in.Now, in.Policy.WindowDays) {
			blocked = append(blocked, GateWindow)
			break
		}
	}
	windowSpan := time.Duration(in.Policy.WindowDays) * 24 * time.Hour
	if !in.LastGrow.IsZero() && in.Now.Sub(in.LastGrow) < windowSpan {
		blocked = append(blocked, GateRecentGrow)
	}
	if !in.LastShrink.IsZero() && in.Now.Sub(in.LastShrink) < in.Policy.ShrinkCooldown {
		blocked = append(blocked, GateCooldown)
	}

	return blocked
}

func describe(
	res corev1.ResourceName,
	from resource.Quantity,
	to resource.Quantity,
	driver string,
) string {
	return fmt.Sprintf("- `%s`: %s -> %s (driven by %s)",
		res, from.String(), to.String(), driver)
}
