package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestGetPodRequests_Limits(t *testing.T) {
	spec := corev1.PodSpec{
		Containers: []corev1.Container{
			{
				Name: "main",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("100m"),
						corev1.ResourceMemory: resource.MustParse("100Mi"),
					},
					Limits: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("200m"),
						corev1.ResourceMemory: resource.MustParse("200Mi"),
					},
				},
			},
		},
	}

	reqs := getPodRequests(spec)

	// Check Requests
	if val, ok := reqs[corev1.ResourceRequestsCPU]; !ok || val != 100 {
		t.Errorf("Expected requests.cpu=100, got %v (map: %v)", val, reqs)
	}

	// Check Limits
	if val, ok := reqs[corev1.ResourceLimitsCPU]; !ok || val != 200 {
		t.Errorf("Expected limits.cpu=200, got %v (map: %v)", val, reqs)
	} else {
		t.Logf("Success: Found limits.cpu=%d", val)
	}
}
