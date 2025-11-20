package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestConvertToReadableFormat(t *testing.T) {
	tests := []struct {
		name       string
		resName    corev1.ResourceName
		milliValue int64
		format     resource.Format
		expected   string
	}{
		{
			name:       "CPU - small value",
			resName:    corev1.ResourceCPU,
			milliValue: 500, // 500m
			format:     resource.DecimalSI,
			expected:   "500m",
		},
		{
			name:       "CPU - large value",
			resName:    corev1.ResourceCPU,
			milliValue: 2000, // 2000m = 2
			format:     resource.DecimalSI,
			expected:   "2",
		},
		{
			name:       "Memory - exact Mi",
			resName:    corev1.ResourceMemory,
			milliValue: 1024 * 1024 * 1000, // 1 Mi in millis
			format:     resource.BinarySI,
			expected:   "1Mi",
		},
		{
			name:       "Memory - slightly more than 1 Mi",
			resName:    corev1.ResourceMemory,
			milliValue: (1024*1024 + 1) * 1000, // 1 Mi + 1 byte
			format:     resource.BinarySI,
			expected:   "2Mi", // Should round up
		},
		{
			name:       "Memory - 1.2 Gi",
			resName:    corev1.ResourceMemory,
			milliValue: 1288490188800, // ~1.2 Gi in millis
			format:     resource.BinarySI,
			expected:   "1229Mi", // 1.2 Gi = 1228.8 Mi -> 1229 Mi
		},
		{
			name:       "Memory - requests.memory",
			resName:    "requests.memory",
			milliValue: 500 * 1000, // 500 bytes
			format:     resource.BinarySI,
			expected:   "1Mi", // Round up to 1 Mi
		},
		{
			name:       "Storage - 1 Gi",
			resName:    "requests.storage",
			milliValue: 1024 * 1024 * 1024 * 1000, // 1 Gi in millis
			format:     resource.BinarySI,
			expected:   "1Gi",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := convertToReadableFormat(tt.resName, tt.milliValue, tt.format)
			if got.String() != tt.expected {
				t.Errorf("convertToReadableFormat() = %v, want %v", got.String(), tt.expected)
			}
		})
	}
}
