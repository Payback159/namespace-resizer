package sizing

import (
	"math"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

const bytesPerMi = 1024 * 1024

// IsCountable reports whether a quota key counts objects rather than
// measuring a divisible amount. Countable keys only accept whole numbers,
// so a fractional target must be rounded up before it is written back.
func IsCountable(res corev1.ResourceName) bool {
	if strings.HasPrefix(string(res), "count/") {
		return true
	}
	switch res {
	case corev1.ResourcePods,
		corev1.ResourceServices,
		corev1.ResourceReplicationControllers,
		corev1.ResourceQuotas,
		corev1.ResourceSecrets,
		corev1.ResourceConfigMaps,
		corev1.ResourcePersistentVolumeClaims,
		corev1.ResourceServicesNodePorts,
		corev1.ResourceServicesLoadBalancers:
		return true
	}
	return false
}

// Quantize converts a computed milli-value back into a Quantity that
// Kubernetes accepts for the given quota key. Rounding is always upwards so
// the result never falls below the computed target.
func Quantize(res corev1.ResourceName, milli int64, format resource.Format) resource.Quantity {
	name := string(res)
	switch {
	case strings.Contains(name, "memory"), strings.Contains(name, "storage"):
		// Milli-bytes back to bytes, then up to the next whole Mi so the
		// rendered value stays readable ("101Mi" instead of raw bytes).
		bytes := float64(milli) / 1000.0
		mi := math.Ceil(bytes / float64(bytesPerMi))
		return *resource.NewQuantity(int64(mi)*bytesPerMi, resource.BinarySI)
	case IsCountable(res):
		whole := int64(math.Ceil(float64(milli) / 1000.0))
		return *resource.NewQuantity(whole, resource.DecimalSI)
	default:
		return *resource.NewMilliQuantity(milli, format)
	}
}
