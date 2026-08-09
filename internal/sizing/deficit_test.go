package sizing

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestParseEventMessage(t *testing.T) {
	msg := `create Pod burst-sts-0 in StatefulSet burst-sts failed error: pods "burst-sts-0" is forbidden: exceeded quota: sts-burst-quota, requested: requests.cpu=200m, used: requests.cpu=0, limited: requests.cpu=100m`

	resName, qty, err := ParseEventMessage(msg)
	if err != nil {
		t.Fatalf("ParseEventMessage() error = %v, want nil", err)
	}
	if resName != corev1.ResourceName("requests.cpu") {
		t.Errorf("resName = %s, want requests.cpu", resName)
	}
	if got := qty.MilliValue(); got != 200 {
		t.Errorf("qty.MilliValue() = %d, want 200", got)
	}
}

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

	reqs := PodRequests(spec)

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

func TestPodRequests_InitContainerDominates(t *testing.T) {
	spec := corev1.PodSpec{
		InitContainers: []corev1.Container{{
			Name: "migrate",
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU: resource.MustParse("2"),
				},
			},
		}},
		Containers: []corev1.Container{{
			Name: "app",
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU: resource.MustParse("500m"),
				},
			},
		}},
	}

	reqs := PodRequests(spec)

	if got := reqs[corev1.ResourceRequestsCPU]; got != 2000 {
		t.Fatalf("requests.cpu = %d milli, want 2000 (init container wins)", got)
	}
}

func TestWorkloadKey(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "replicaset pod suffix is stripped",
			in:   "app-a-6b474476c4-xfg2z",
			want: "app-a-6b474476c4",
		},
		{
			name: "statefulset ordinal is stripped",
			in:   "web-0",
			want: "web",
		},
		{
			name: "name without a hyphen is returned unchanged",
			in:   "web",
			want: "web",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := WorkloadKey(tc.in); got != tc.want {
				t.Errorf("WorkloadKey(%s) = %s, want %s", tc.in, got, tc.want)
			}
		})
	}
}

func TestPVCRequests(t *testing.T) {
	cases := []struct {
		name      string
		templates []corev1.PersistentVolumeClaim
		want      int64
	}{
		{
			name: "single template",
			templates: []corev1.PersistentVolumeClaim{
				{
					Spec: corev1.PersistentVolumeClaimSpec{
						Resources: corev1.VolumeResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceStorage: resource.MustParse("1Gi"),
							},
						},
					},
				},
			},
			want: 1 * 1024 * 1024 * 1024 * 1000,
		},
		{
			name: "two templates are summed",
			templates: []corev1.PersistentVolumeClaim{
				{
					Spec: corev1.PersistentVolumeClaimSpec{
						Resources: corev1.VolumeResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceStorage: resource.MustParse("1Gi"),
							},
						},
					},
				},
				{
					Spec: corev1.PersistentVolumeClaimSpec{
						Resources: corev1.VolumeResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceStorage: resource.MustParse("2Gi"),
							},
						},
					},
				},
			},
			want: 3 * 1024 * 1024 * 1024 * 1000,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reqs := PVCRequests(tc.templates)
			if got := reqs[corev1.ResourceRequestsStorage]; got != tc.want {
				t.Errorf("requests.storage = %d, want %d", got, tc.want)
			}
		})
	}
}
