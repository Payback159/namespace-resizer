package sizing

import (
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

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
	// ShrinkPreview holds the shrink targets that would be proposed if no gate
	// blocked them. It exists so the dry-run rollout can report what the
	// controller would do while shrinking is still switched off. Never act on
	// it — only Targets is authoritative.
	ShrinkPreview map[corev1.ResourceName]resource.Quantity
	Reason        string
	BlockedBy     []Gate
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
	var growReasons, shrinkReasons []string

	for res, hard := range in.Hard {
		hardMilli := hard.MilliValue()
		if hardMilli == 0 {
			continue
		}

		usedMilli := int64(0)
		if u, ok := in.Used[res]; ok {
			usedMilli = u.MilliValue()
		}

		headroom := in.Policy.HeadroomFor(res)
		targetMilli, driver := targetFor(in, res, usedMilli, headroom)

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

		case driver == "configured minimum" || float64(targetMilli) < float64(hardMilli)*(1-in.Policy.Tolerance):
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
			Direction: DirectionGrow,
			Targets:   growTargets,
			Reason:    strings.Join(growReasons, "\n"),
		}
	}

	if len(shrinkTargets) == 0 {
		return Decision{Direction: DirectionNone}
	}

	if blocked := shrinkGates(in, shrinkTargets); len(blocked) > 0 {
		return Decision{
			Direction:     DirectionNone,
			ShrinkPreview: shrinkTargets,
			BlockedBy:     blocked,
		}
	}

	sort.Strings(shrinkReasons)
	return Decision{
		Direction:     DirectionShrink,
		Targets:       shrinkTargets,
		ShrinkPreview: shrinkTargets,
		Reason:        strings.Join(shrinkReasons, "\n"),
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
	if min, ok := in.Policy.MinFor(res); ok && min.MilliValue() > target {
		target = min.MilliValue()
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
