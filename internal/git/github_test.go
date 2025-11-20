package git

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"text/template"

	"github.com/google/go-github/v60/github"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestApplyChangesToYaml(t *testing.T) {
	g := NewWithT(t)

	tests := []struct {
		name     string
		input    string
		limits   map[corev1.ResourceName]resource.Quantity
		expected []string // Substrings to expect
	}{
		{
			name: "Simple replacement",
			input: `apiVersion: v1
kind: ResourceQuota
metadata:
  name: test
spec:
  hard:
    cpu: "1000m"
    memory: 1Gi
`,
			limits: map[corev1.ResourceName]resource.Quantity{
				corev1.ResourceCPU:    resource.MustParse("2"),
				corev1.ResourceMemory: resource.MustParse("2Gi"),
			},
			expected: []string{`cpu: "2"`, `memory: "2Gi"`},
		},
		{
			name: "Preserve comments",
			input: `apiVersion: v1
kind: ResourceQuota
metadata:
  name: test # My Quota
spec:
  hard:
    # CPU Limit
    cpu: "1000m"
    pods: "10"
`,
			limits: map[corev1.ResourceName]resource.Quantity{
				corev1.ResourceCPU: resource.MustParse("4"),
			},
			expected: []string{`# CPU Limit`, `cpu: "4"`, `pods: "10"`},
		},
		{
			name: "Handle requests.cpu format",
			input: `spec:
  hard:
    requests.cpu: "500m"
`,
			limits: map[corev1.ResourceName]resource.Quantity{
				corev1.ResourceRequestsCPU: resource.MustParse("1"),
			},
			expected: []string{`requests.cpu: "1"`},
		},
		{
			name: "Handle storage short name",
			input: `spec:
  hard:
    storage: "10Gi"
`,
			limits: map[corev1.ResourceName]resource.Quantity{
				corev1.ResourceRequestsStorage: resource.MustParse("20Gi"),
			},
			expected: []string{`storage: "20Gi"`},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := applyChangesToYaml(tt.input, tt.limits)
			for _, exp := range tt.expected {
				g.Expect(got).To(ContainSubstring(exp))
			}
		})
	}
}

func TestGeneratePRBody(t *testing.T) {
	g := NewWithT(t)

	limits := map[corev1.ResourceName]resource.Quantity{
		corev1.ResourceCPU: resource.MustParse("10"),
	}

	body := generatePRBody("default", "my-quota", limits)

	g.Expect(body).To(ContainSubstring("Quota Resize Recommendation"))
	g.Expect(body).To(ContainSubstring("default"))
	g.Expect(body).To(ContainSubstring("my-quota"))
	g.Expect(body).To(ContainSubstring("| cpu | 10 |"))
}

func TestGetPRStatus(t *testing.T) {
	g := NewWithT(t)

	// Mock GitHub API
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	defer server.Close()

	mux.HandleFunc("/repos/o/r/pulls/123", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"state": "open", "merged": false}`)
	})

	mux.HandleFunc("/repos/o/r/pulls/456", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"state": "closed", "merged": true}`)
	})

	// Setup Client
	client := github.NewClient(nil)
	serverURL, _ := url.Parse(server.URL + "/")
	client.BaseURL = serverURL
	client.UploadURL = serverURL

	provider := &GitHubProvider{
		client:       client,
		owner:        "o",
		repo:         "r",
		pathTemplate: nil, // Not used here
	}

	// Test Open PR
	status, err := provider.GetPRStatus(context.TODO(), 123)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(status.IsOpen).To(BeTrue())
	g.Expect(status.IsMerged).To(BeFalse())

	// Test Merged PR
	status, err = provider.GetPRStatus(context.TODO(), 456)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(status.IsOpen).To(BeFalse())
	g.Expect(status.IsMerged).To(BeTrue())
}

func TestCreatePR(t *testing.T) {
	g := NewWithT(t)

	// Mock GitHub API
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	defer server.Close()

	// 1. Get Repo
	mux.HandleFunc("/repos/o/r", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"default_branch": "main"}`)
	})

	// 2. Get Base Ref
	mux.HandleFunc("/repos/o/r/git/ref/heads/main", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"object": {"sha": "base-sha"}}`)
	})

	// 3. Create Ref
	mux.HandleFunc("/repos/o/r/git/refs", func(w http.ResponseWriter, r *http.Request) {
		g.Expect(r.Method).To(Equal("POST"))
		_, _ = fmt.Fprint(w, `{"ref": "refs/heads/new-branch"}`)
	})

	// 4. List Files (Find Quota)
	mux.HandleFunc("/repos/o/r/contents/managed-resources/cluster/default", func(w http.ResponseWriter, r *http.Request) {
		// Return a list of files
		_, _ = fmt.Fprint(w, `[
			{"name": "quota.yaml", "path": "managed-resources/cluster/default/quota.yaml", "type": "file"}
		]`)
	})

	// 5. Get File Content & 6. Update File
	mux.HandleFunc("/repos/o/r/contents/managed-resources/cluster/default/quota.yaml", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "GET":
			// Return content
			_, _ = fmt.Fprint(w, `{"content": "a2luZDogUmVzb3VyY2VRdW90YQptZXRhZGF0YToKICBuYW1lOiBteS1xdW90YQpzcGVjOgogIGhhcmQ6CiAgICByZXF1ZXN0cy5jcHU6IDE=", "encoding": "base64", "sha": "file-sha"}`)
		case "PUT":
			_, _ = fmt.Fprint(w, `{"commit": {"sha": "new-sha"}}`)
		}
	}) // 7. Create PR
	mux.HandleFunc("/repos/o/r/pulls", func(w http.ResponseWriter, r *http.Request) {
		g.Expect(r.Method).To(Equal("POST"))
		_, _ = fmt.Fprint(w, `{"number": 101, "state": "open"}`)
	})

	// 8. Add Labels
	mux.HandleFunc("/repos/o/r/issues/101/labels", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `[]`)
	})

	// Setup Client
	client := github.NewClient(nil)
	serverURL, _ := url.Parse(server.URL + "/")
	client.BaseURL = serverURL
	client.UploadURL = serverURL

	tmpl := template.Must(template.New("path").Parse("managed-resources/{{ .Cluster }}/{{ .Namespace }}"))

	provider := &GitHubProvider{
		client:       client,
		owner:        "o",
		repo:         "r",
		clusterName:  "cluster",
		pathTemplate: tmpl,
	}

	limits := map[corev1.ResourceName]resource.Quantity{
		corev1.ResourceRequestsCPU: resource.MustParse("2"),
	}

	prID, err := provider.CreatePR(context.TODO(), "my-quota", "default", nil, limits)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(prID).To(Equal(101))
}

func TestUpdatePR(t *testing.T) {
	g := NewWithT(t)

	// Mock GitHub API
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	defer server.Close()

	// 1. Get PR
	mux.HandleFunc("/repos/o/r/pulls/101", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"head": {"ref": "resize/branch"}}`)
	})

	// 2. List Files (Find Quota)
	mux.HandleFunc("/repos/o/r/contents/managed-resources/cluster/default", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `[
			{"name": "quota.yaml", "path": "managed-resources/cluster/default/quota.yaml", "type": "file"}
		]`)
	})

	// 3. Get File Content & 4. Update File
	mux.HandleFunc("/repos/o/r/contents/managed-resources/cluster/default/quota.yaml", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "GET":
			// Return content
			_, _ = fmt.Fprint(w, `{"content": "a2luZDogUmVzb3VyY2VRdW90YQptZXRhZGF0YToKICBuYW1lOiBteS1xdW90YQpzcGVjOgogIGhhcmQ6CiAgICByZXF1ZXN0cy5jcHU6IDE=", "encoding": "base64", "sha": "file-sha"}`)
		case "PUT":
			_, _ = fmt.Fprint(w, `{"commit": {"sha": "new-sha"}}`)
		}
	})

	// Setup Client
	client := github.NewClient(nil)
	serverURL, _ := url.Parse(server.URL + "/")
	client.BaseURL = serverURL
	client.UploadURL = serverURL

	tmpl := template.Must(template.New("path").Parse("managed-resources/{{ .Cluster }}/{{ .Namespace }}"))

	provider := &GitHubProvider{
		client:       client,
		owner:        "o",
		repo:         "r",
		clusterName:  "cluster",
		pathTemplate: tmpl,
	}

	limits := map[corev1.ResourceName]resource.Quantity{
		corev1.ResourceRequestsCPU: resource.MustParse("2"),
	}

	err := provider.UpdatePR(context.TODO(), 101, "my-quota", "default", nil, limits)
	g.Expect(err).ToNot(HaveOccurred())
}
