package sizing

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestQuantize(t *testing.T) {
	cases := []struct {
		name   string
		res    corev1.ResourceName
		milli  int64
		format resource.Format
		want   string
	}{
		{
			name:   "cpu keeps milli precision",
			res:    corev1.ResourceRequestsCPU,
			milli:  11250,
			format: resource.DecimalSI,
			want:   "11250m",
		},
		{
			name:   "memory rounds up to whole Mi",
			res:    corev1.ResourceRequestsMemory,
			milli:  100*1024*1024*1000 + 1,
			format: resource.BinarySI,
			want:   "101Mi",
		},
		{
			name:   "pods round up to a whole number",
			res:    corev1.ResourcePods,
			milli:  11250,
			format: resource.DecimalSI,
			want:   "12",
		},
		{
			name:   "count/ keys round up to a whole number",
			res:    corev1.ResourceName("count/deployments.apps"),
			milli:  3200,
			format: resource.DecimalSI,
			want:   "4",
		},
		{
			name:   "storage rounds up to whole Mi",
			res:    corev1.ResourceRequestsStorage,
			milli:  5 * 1024 * 1024 * 1000,
			format: resource.BinarySI,
			want:   "5Mi",
		},
		{
			// A storage-class scoped key counts claims. The scope contains
			// the word "storage", which must not route it into bytes.
			name:   "storage-class scoped claim count stays an integer",
			res:    corev1.ResourceName("gold.storageclass.storage.k8s.io/persistentvolumeclaims"),
			milli:  11250,
			format: resource.DecimalSI,
			want:   "12",
		},
		{
			name:   "storage-class scoped storage request stays bytes",
			res:    corev1.ResourceName("gold.storageclass.storage.k8s.io/requests.storage"),
			milli:  5 * 1024 * 1024 * 1000,
			format: resource.BinarySI,
			want:   "5Mi",
		},
		{
			name:   "count/ key whose group contains storage stays an integer",
			res:    corev1.ResourceName("count/csistoragecapacities.storage.k8s.io"),
			milli:  3200,
			format: resource.DecimalSI,
			want:   "4",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Quantize(tc.res, tc.milli, tc.format)
			if got.String() != tc.want {
				t.Fatalf("Quantize(%s, %d) = %s, want %s",
					tc.res, tc.milli, got.String(), tc.want)
			}
		})
	}
}

func TestIsCountable(t *testing.T) {
	countable := []corev1.ResourceName{
		corev1.ResourcePods,
		corev1.ResourceSecrets,
		corev1.ResourcePersistentVolumeClaims,
		corev1.ResourceName("count/jobs.batch"),
		corev1.ResourceName("count/csistoragecapacities.storage.k8s.io"),
		corev1.ResourceName("gold.storageclass.storage.k8s.io/persistentvolumeclaims"),
	}
	for _, res := range countable {
		if !IsCountable(res) {
			t.Errorf("IsCountable(%s) = false, want true", res)
		}
	}

	divisible := []corev1.ResourceName{
		corev1.ResourceRequestsCPU,
		corev1.ResourceLimitsMemory,
		corev1.ResourceRequestsStorage,
		corev1.ResourceName("gold.storageclass.storage.k8s.io/requests.storage"),
	}
	for _, res := range divisible {
		if IsCountable(res) {
			t.Errorf("IsCountable(%s) = true, want false", res)
		}
	}
}
