package controller

import (
	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/payback159/namespace-resizer/internal/sizing"
)

const (
	labelNamespace = "namespace"
	labelQuota     = "quota"
	labelResource  = "resource"
	labelGate      = "gate"
	labelDirection = "direction"
)

var quotaLabels = []string{labelNamespace, labelQuota, labelResource}

var (
	quotaTarget = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "resizer_quota_target",
		Help: "Computed target for a quota resource, in milli-units.",
	}, quotaLabels)

	quotaWasteRatio = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "resizer_quota_waste_ratio",
		Help: "Current hard limit divided by the computed target.",
	}, quotaLabels)

	shrinkBlockedBy = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "resizer_shrink_blocked_by",
		Help: "1 while the named gate blocks a pending shrink, 0 otherwise.",
	}, []string{labelNamespace, labelQuota, labelGate})

	decisionTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "resizer_decision_total",
		Help: "Decisions taken, by direction.",
	}, []string{labelNamespace, labelQuota, labelDirection})
)

// allGates is iterated on every record so a gate that stopped blocking is
// actively reset to 0 instead of keeping its last value forever.
var allGates = []sizing.Gate{
	sizing.GateEnabled,
	sizing.GateWindow,
	sizing.GateRecentGrow,
	sizing.GateCooldown,
}

func init() {
	metrics.Registry.MustRegister(
		quotaTarget, quotaWasteRatio, shrinkBlockedBy, decisionTotal)
}

// recordDecision publishes one evaluation. It reports the shrink preview as
// well as the acted-on targets, so the waste is visible while the shrink path
// is still switched off.
func recordDecision(
	namespace, quota string,
	hard corev1.ResourceList,
	decision sizing.Decision,
) {
	decisionTotal.WithLabelValues(namespace, quota, decision.Direction.String()).Inc()

	targets := decision.Targets
	if len(targets) == 0 {
		targets = decision.ShrinkPreview
	}

	for res, target := range targets {
		labels := []string{namespace, quota, string(res)}
		targetMilli := target.MilliValue()
		quotaTarget.WithLabelValues(labels...).Set(float64(targetMilli))

		current, ok := hard[res]
		if !ok || targetMilli == 0 {
			continue
		}
		quotaWasteRatio.WithLabelValues(labels...).
			Set(float64(current.MilliValue()) / float64(targetMilli))
	}

	blocked := make(map[sizing.Gate]bool, len(decision.BlockedBy))
	for _, gate := range decision.BlockedBy {
		blocked[gate] = true
	}
	for _, gate := range allGates {
		value := 0.0
		if blocked[gate] {
			value = 1.0
		}
		shrinkBlockedBy.WithLabelValues(namespace, quota, string(gate)).Set(value)
	}
}
