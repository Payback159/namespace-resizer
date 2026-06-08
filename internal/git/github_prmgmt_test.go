package git

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"text/template"

	"github.com/google/go-github/v75/github"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func newTestProvider(serverURL string) *GitHubProvider {
	client := github.NewClient(nil)
	u, _ := url.Parse(serverURL + "/")
	client.BaseURL = u
	client.UploadURL = u

	tmpl := template.Must(template.New("path").Parse("managed-resources/{{ .Cluster }}/{{ .Namespace }}"))
	return &GitHubProvider{
		client:       client,
		owner:        "o",
		repo:         "r",
		clusterName:  "cluster",
		pathTemplate: tmpl,
	}
}

// TestFindOpenPR verifies that an open PR is matched by its resizer branch prefix.
func TestFindOpenPR(t *testing.T) {
	g := NewWithT(t)

	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	defer server.Close()

	mux.HandleFunc("/repos/o/r/pulls", func(w http.ResponseWriter, r *http.Request) {
		g.Expect(r.Method).To(Equal("GET"))
		g.Expect(r.URL.Query().Get("state")).To(Equal("open"))
		_, _ = fmt.Fprint(w, `[
			{"number": 7, "head": {"ref": "feature/unrelated"}},
			{"number": 42, "head": {"ref": "resize/default-my-quota-1700000000"}}
		]`)
	})

	provider := newTestProvider(server.URL)

	// Matching namespace/quota -> returns the PR number.
	id, err := provider.FindOpenPR(context.TODO(), "default", "my-quota")
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(id).To(Equal(42))

	// Non-matching quota -> returns 0.
	id, err = provider.FindOpenPR(context.TODO(), "default", "other-quota")
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(id).To(Equal(0))
}

// TestFindOpenPR_ListError verifies that a list failure surfaces an error
// (so the controller does not fall through and create a duplicate PR).
func TestFindOpenPR_ListError(t *testing.T) {
	g := NewWithT(t)

	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	defer server.Close()

	mux.HandleFunc("/repos/o/r/pulls", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})

	provider := newTestProvider(server.URL)
	_, err := provider.FindOpenPR(context.TODO(), "default", "my-quota")
	g.Expect(err).To(HaveOccurred())
}

// TestUpdatePR_EditPayload verifies that PR updates only send the editable body
// field and not the full PR object (which would include head/base and be
// rejected by GitHub).
func TestUpdatePR_EditPayload(t *testing.T) {
	g := NewWithT(t)

	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	defer server.Close()

	var patchBody string

	mux.HandleFunc("/repos/o/r/pulls/101", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_, _ = fmt.Fprint(w, `{"head": {"ref": "resize/branch"}}`)
		case http.MethodPatch:
			b, _ := io.ReadAll(r.Body)
			patchBody = string(b)
			_, _ = fmt.Fprint(w, `{"number": 101}`)
		}
	})

	mux.HandleFunc("/repos/o/r/contents/managed-resources/cluster/default", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `[{"name": "quota.yaml", "path": "managed-resources/cluster/default/quota.yaml", "type": "file"}]`)
	})

	mux.HandleFunc("/repos/o/r/contents/managed-resources/cluster/default/quota.yaml", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_, _ = fmt.Fprint(w, `{"content": "a2luZDogUmVzb3VyY2VRdW90YQptZXRhZGF0YToKICBuYW1lOiBteS1xdW90YQpzcGVjOgogIGhhcmQ6CiAgICByZXF1ZXN0cy5jcHU6IDE=", "encoding": "base64", "sha": "file-sha"}`)
		case http.MethodPut:
			_, _ = fmt.Fprint(w, `{"commit": {"sha": "new-sha"}}`)
		}
	})

	provider := newTestProvider(server.URL)
	limits := map[corev1.ResourceName]resource.Quantity{
		corev1.ResourceRequestsCPU: resource.MustParse("2"),
	}

	err := provider.UpdatePR(context.TODO(), 101, "my-quota", "default", nil, limits)
	g.Expect(err).ToNot(HaveOccurred())

	// The PATCH body must contain the body field but NOT head/base, which the
	// Edit endpoint cannot accept as objects.
	g.Expect(patchBody).To(ContainSubstring(`"body"`))
	g.Expect(patchBody).ToNot(ContainSubstring(`"head"`))
	g.Expect(patchBody).ToNot(ContainSubstring(`"base"`))
}

// TestGetPRStatus_CombinedStatusError verifies that a failure fetching the
// combined commit status is surfaced instead of being silently treated as
// "no checks" (which could bypass required CI during auto-merge).
func TestGetPRStatus_CombinedStatusError(t *testing.T) {
	g := NewWithT(t)

	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	defer server.Close()

	mux.HandleFunc("/repos/o/r/pulls/789", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"state": "open", "merged": false, "head": {"sha": "abc123"}}`)
	})

	mux.HandleFunc("/repos/o/r/commits/abc123/status", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "status service unavailable", http.StatusInternalServerError)
	})

	provider := newTestProvider(server.URL)
	_, err := provider.GetPRStatus(context.TODO(), 789)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("combined status"))
}

// TestGetPRStatus_NoHeadSHA verifies that a PR without a head SHA does not call
// the status endpoint and reports empty checks.
func TestGetPRStatus_NoHeadSHA(t *testing.T) {
	g := NewWithT(t)

	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	defer server.Close()

	mux.HandleFunc("/repos/o/r/pulls/321", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"state": "open", "merged": false}`)
	})

	provider := newTestProvider(server.URL)
	status, err := provider.GetPRStatus(context.TODO(), 321)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(status.IsOpen).To(BeTrue())
	g.Expect(status.ChecksState).To(Equal(""))
	g.Expect(status.ChecksTotalCount).To(Equal(0))
}
