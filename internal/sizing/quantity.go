package sizing

import (
	"math"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

const (
	bytesPerMi  = 1024 * 1024
	countPrefix = "count/"
)

// measureOf strips the scope from a quota key so classification looks only at
// what is being measured. ResourceQuota keys can be scoped by storage class,
// and the scope contains the word "storage" whatever it measures:
// "gold.storageclass.storage.k8s.io/persistentvolumeclaims" counts claims
// while ".../requests.storage" measures bytes. Keys carrying the "count/"
// prefix are returned whole — there the prefix is the classification.
func measureOf(res corev1.ResourceName) string {
	name := string(res)
	if strings.HasPrefix(name, countPrefix) {
		return name
	}
	if i := strings.LastIndex(name, "/"); i >= 0 {
		return name[i+1:]
	}
	return name
}

// IsCountable reports whether a quota key counts objects rather than
// measuring a divisible amount. Countable keys only accept whole numbers,
// so a fractional target must be rounded up before it is written back.
func IsCountable(res corev1.ResourceName) bool {
	if strings.HasPrefix(string(res), countPrefix) {
		return true
	}
	switch corev1.ResourceName(measureOf(res)) {
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
//
// Countable keys are tested first. A substring test for "storage" would
// otherwise claim scoped keys such as
// "gold.storageclass.storage.k8s.io/persistentvolumeclaims", which counts
// claims and has to stay an integer.
func Quantize(res corev1.ResourceName, milli int64, format resource.Format) resource.Quantity {
	if IsCountable(res) {
		whole := int64(math.Ceil(float64(milli) / 1000.0))
		return *resource.NewQuantity(whole, resource.DecimalSI)
	}

	measure := measureOf(res)
	if strings.Contains(measure, "memory") || strings.Contains(measure, "storage") {
		// Milli-bytes back to bytes, then up to the next whole Mi so the
		// rendered value stays readable ("101Mi" instead of raw bytes).
		bytes := float64(milli) / 1000.0
		mi := math.Ceil(bytes / float64(bytesPerMi))
		return *resource.NewQuantity(int64(mi)*bytesPerMi, resource.BinarySI)
	}

	return *resource.NewMilliQuantity(milli, format)
}
