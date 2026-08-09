package git

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"text/template"

	"github.com/google/go-github/v75/github"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// newTestProvider points a GitHubProvider at a local test server.
func newTestProvider(t *testing.T, handler http.Handler) (*GitHubProvider, func()) {
	t.Helper()
	server := httptest.NewServer(handler)

	client := github.NewClient(server.Client())
	baseURL, err := url.Parse(server.URL + "/")
	if err != nil {
		t.Fatalf("parse base URL: %v", err)
	}
	client.BaseURL = baseURL
	client.UploadURL = baseURL

	tmpl := template.Must(template.New("path").Parse("managed-resources/{{ .Cluster }}/{{ .Namespace }}"))
	provider := &GitHubProvider{
		client:       client,
		owner:        "o",
		repo:         "r",
		clusterName:  "cluster",
		pathTemplate: tmpl,
	}
	return provider, server.Close
}

// TestFindOpenPR verifies that an open PR is matched by its resizer branch prefix.
func TestFindOpenPR(t *testing.T) {
	g := NewWithT(t)

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/pulls", func(w http.ResponseWriter, r *http.Request) {
		g.Expect(r.Method).To(Equal("GET"))
		g.Expect(r.URL.Query().Get("state")).To(Equal("open"))
		_, _ = fmt.Fprint(w, `[
			{"number": 7, "head": {"ref": "feature/unrelated"}},
			{"number": 42, "head": {"ref": "resize/default-my-quota-1700000000"}}
		]`)
	})
	provider, teardown := newTestProvider(t, mux)
	defer teardown()

	// Matching namespace/quota -> returns the PR number.
	id, _, err := provider.FindOpenPR(context.TODO(), "default", "my-quota")
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(id).To(Equal(42))

	// Non-matching quota -> returns 0.
	id, _, err = provider.FindOpenPR(context.TODO(), "default", "other-quota")
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(id).To(Equal(0))
}

// TestFindOpenPR_ListError verifies that a list failure surfaces an error
// (so the controller does not fall through and create a duplicate PR).
func TestFindOpenPR_ListError(t *testing.T) {
	g := NewWithT(t)

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/pulls", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	provider, teardown := newTestProvider(t, mux)
	defer teardown()

	_, _, err := provider.FindOpenPR(context.TODO(), "default", "my-quota")
	g.Expect(err).To(HaveOccurred())
}

// TestUpdatePR_EditPayload verifies that PR updates only send the editable body
// field and not the full PR object (which would include head/base and be
// rejected by GitHub).
func TestUpdatePR_EditPayload(t *testing.T) {
	g := NewWithT(t)

	mux := http.NewServeMux()

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

	provider, teardown := newTestProvider(t, mux)
	defer teardown()
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
	mux.HandleFunc("/repos/o/r/pulls/789", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"state": "open", "merged": false, "head": {"sha": "abc123"}}`)
	})

	mux.HandleFunc("/repos/o/r/commits/abc123/status", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "status service unavailable", http.StatusInternalServerError)
	})

	provider, teardown := newTestProvider(t, mux)
	defer teardown()
	_, err := provider.GetPRStatus(context.TODO(), 789)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("combined status"))
}

// TestGetPRStatus_NoHeadSHA verifies that a PR without a head SHA does not call
// the status endpoint and reports empty checks.
func TestGetPRStatus_NoHeadSHA(t *testing.T) {
	g := NewWithT(t)

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/pulls/321", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"state": "open", "merged": false}`)
	})

	provider, teardown := newTestProvider(t, mux)
	defer teardown()
	status, err := provider.GetPRStatus(context.TODO(), 321)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(status.IsOpen).To(BeTrue())
	g.Expect(status.ChecksState).To(Equal(""))
	g.Expect(status.ChecksTotalCount).To(Equal(0))
}

func TestFindOpenPR_ReturnsDirectionFromLabel(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/pulls", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `[{
			"number": 42,
			"head": {"ref": "resize/team-a-compute-1700000000"},
			"labels": [
				{"name": "resizer/managed"},
				{"name": "resizer/direction:shrink"}
			]
		}]`)
	})
	provider, teardown := newTestProvider(t, mux)
	defer teardown()

	id, direction, err := provider.FindOpenPR(context.Background(), "team-a", "compute")

	if err != nil {
		t.Fatalf("FindOpenPR: %v", err)
	}
	if id != 42 {
		t.Errorf("id = %d, want 42", id)
	}
	if direction != DirectionShrink {
		t.Errorf("direction = %q, want %q", direction, DirectionShrink)
	}
}

func TestFindOpenPR_DefaultsToGrowWithoutLabel(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/pulls", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `[{
			"number": 7,
			"head": {"ref": "resize/team-a-compute-1700000000"},
			"labels": [{"name": "resizer/managed"}]
		}]`)
	})
	provider, teardown := newTestProvider(t, mux)
	defer teardown()

	_, direction, err := provider.FindOpenPR(context.Background(), "team-a", "compute")

	if err != nil {
		t.Fatalf("FindOpenPR: %v", err)
	}
	// PRs created before this feature carry no direction label. Treating them
	// as grow keeps the old behaviour for them and never enables an
	// unreviewed shrink merge.
	if direction != DirectionGrow {
		t.Errorf("direction = %q, want %q", direction, DirectionGrow)
	}
}

func TestClosePR_CommentsThenCloses(t *testing.T) {
	var commented, closed bool
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/issues/42/comments",
		func(w http.ResponseWriter, r *http.Request) {
			commented = true
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"id": 1}`)
		})
	mux.HandleFunc("/repos/o/r/pulls/42",
		func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPatch {
				closed = true
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"number": 42, "state": "closed"}`)
		})
	provider, teardown := newTestProvider(t, mux)
	defer teardown()

	err := provider.ClosePR(context.Background(), 42, "superseded by a shortage")

	if err != nil {
		t.Fatalf("ClosePR: %v", err)
	}
	if !commented {
		t.Error("no comment was posted; the reason must survive in the PR")
	}
	if !closed {
		t.Error("PR was not closed")
	}
}

func TestFindOpenPR_UnknownDirectionReadsAsShrink(t *testing.T) {
	cases := []struct {
		name   string
		labels []string
	}{
		{"wrong case", []string{"resizer/direction:Shrink"}},
		{"unrecognised value", []string{"resizer/direction:banana"}},
		// Nothing stops a second direction label being pinned onto a pull
		// request this controller opened. A grow label sitting in front of
		// the real one must not decide the outcome by list order.
		{"grow label added in front of a shrink", []string{
			"resizer/direction:grow", "resizer/direction:shrink",
		}},
		{"grow label added after a shrink", []string{
			"resizer/direction:shrink", "resizer/direction:grow",
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			names := make([]string, 0, len(tc.labels))
			for _, label := range tc.labels {
				names = append(names, fmt.Sprintf(`{"name": %q}`, label))
			}
			mux := http.NewServeMux()
			mux.HandleFunc("/repos/o/r/pulls", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprintf(w, `[{
					"number": 42,
					"head": {"ref": "resize/team-a-compute-1700000000"},
					"labels": [%s]
				}]`, strings.Join(names, ", "))
			})
			provider, teardown := newTestProvider(t, mux)
			defer teardown()

			_, direction, err := provider.FindOpenPR(context.Background(), "team-a", "compute")

			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(direction).To(Equal(DirectionShrink))
		})
	}
}

// setupCreatePRRoutes registers every handler CreatePR needs before it
// reaches the labelling step: repo lookup, base ref, branch creation, the
// quota file listing/get/put, and PR creation itself, all returning the
// given PR number.
func setupCreatePRRoutes(g *WithT, mux *http.ServeMux, prNumber int) {
	mux.HandleFunc("/repos/o/r", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"default_branch": "main"}`)
	})
	mux.HandleFunc("/repos/o/r/git/ref/heads/main", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"object": {"sha": "base-sha"}}`)
	})
	mux.HandleFunc("/repos/o/r/git/refs", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"ref": "refs/heads/new-branch"}`)
	})
	mux.HandleFunc("/repos/o/r/contents/managed-resources/cluster/default", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `[
			{"name": "quota.yaml", "path": "managed-resources/cluster/default/quota.yaml", "type": "file"}
		]`)
	})
	mux.HandleFunc("/repos/o/r/contents/managed-resources/cluster/default/quota.yaml", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_, _ = fmt.Fprint(w, `{"content": "a2luZDogUmVzb3VyY2VRdW90YQptZXRhZGF0YToKICBuYW1lOiBteS1xdW90YQpzcGVjOgogIGhhcmQ6CiAgICByZXF1ZXN0cy5jcHU6IDE=", "encoding": "base64", "sha": "file-sha"}`)
		case http.MethodPut:
			_, _ = fmt.Fprint(w, `{"commit": {"sha": "new-sha"}}`)
		}
	})
	mux.HandleFunc("/repos/o/r/pulls", func(w http.ResponseWriter, r *http.Request) {
		g.Expect(r.Method).To(Equal(http.MethodPost))
		_, _ = fmt.Fprintf(w, `{"number": %d, "state": "open"}`, prNumber)
	})
}

func createPRLimits() map[corev1.ResourceName]resource.Quantity {
	return map[corev1.ResourceName]resource.Quantity{
		corev1.ResourceRequestsCPU: resource.MustParse("2"),
	}
}

// TestCreatePR_AttachesDirectionLabel is the assertion the label handler in
// TestCreatePR never made: that the direction actually reaches GitHub as a
// label, for both directions.
func TestCreatePR_AttachesDirectionLabel(t *testing.T) {
	cases := []struct {
		name      string
		direction string
		wantLabel string
	}{
		{"shrink", DirectionShrink, "resizer/direction:shrink"},
		{"grow", DirectionGrow, "resizer/direction:grow"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			var gotBody []byte
			mux := http.NewServeMux()
			setupCreatePRRoutes(g, mux, 101)
			mux.HandleFunc("/repos/o/r/issues/101/labels", func(w http.ResponseWriter, r *http.Request) {
				var err error
				gotBody, err = io.ReadAll(r.Body)
				g.Expect(err).ToNot(HaveOccurred())
				_, _ = fmt.Fprint(w, `[]`)
			})
			provider, teardown := newTestProvider(t, mux)
			defer teardown()

			prID, err := provider.CreatePR(context.Background(), "my-quota", "default", tc.direction, nil, createPRLimits())

			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(prID).To(Equal(101))
			g.Expect(string(gotBody)).To(ContainSubstring(tc.wantLabel))
		})
	}
}

// TestCreatePR_ShrinkClosesItselfWhenLabellingFails covers the write half of
// the safety boundary: a shrink PR that cannot be labelled must not survive
// as an unlabelled (and therefore grow-classified, auto-mergeable) PR.
func TestCreatePR_ShrinkClosesItselfWhenLabellingFails(t *testing.T) {
	g := NewWithT(t)
	original := labelRetryBackoff
	labelRetryBackoff = 0
	t.Cleanup(func() { labelRetryBackoff = original })

	var labelAttemptsMade int
	var commented, closed bool
	mux := http.NewServeMux()
	setupCreatePRRoutes(g, mux, 101)
	mux.HandleFunc("/repos/o/r/issues/101/labels", func(w http.ResponseWriter, r *http.Request) {
		labelAttemptsMade++
		w.WriteHeader(http.StatusInternalServerError)
	})
	mux.HandleFunc("/repos/o/r/issues/101/comments", func(w http.ResponseWriter, r *http.Request) {
		commented = true
		_, _ = fmt.Fprint(w, `{"id": 1}`)
	})
	mux.HandleFunc("/repos/o/r/pulls/101", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			closed = true
		}
		_, _ = fmt.Fprint(w, `{"number": 101, "state": "closed"}`)
	})
	provider, teardown := newTestProvider(t, mux)
	defer teardown()

	prID, err := provider.CreatePR(context.Background(), "my-quota", "default", DirectionShrink, nil, createPRLimits())

	g.Expect(err).To(HaveOccurred())
	g.Expect(prID).To(Equal(0))
	g.Expect(labelAttemptsMade).To(Equal(labelAttempts))
	g.Expect(commented).To(BeTrue(), "the reason must survive as a comment")
	g.Expect(closed).To(BeTrue(), "an unlabelled shrink must not be left open")
}

// TestCreatePR_GrowSurvivesLabellingFailure asserts the lenient side of the
// same boundary: a grow PR with a failed label is still usable, since an
// unlabelled PR already defaults to grow.
func TestCreatePR_GrowSurvivesLabellingFailure(t *testing.T) {
	g := NewWithT(t)
	original := labelRetryBackoff
	labelRetryBackoff = 0
	t.Cleanup(func() { labelRetryBackoff = original })

	var closeCalled bool
	mux := http.NewServeMux()
	setupCreatePRRoutes(g, mux, 101)
	mux.HandleFunc("/repos/o/r/issues/101/labels", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	mux.HandleFunc("/repos/o/r/pulls/101", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			closeCalled = true
		}
		_, _ = fmt.Fprint(w, `{"number": 101, "state": "open"}`)
	})
	provider, teardown := newTestProvider(t, mux)
	defer teardown()

	prID, err := provider.CreatePR(context.Background(), "my-quota", "default", DirectionGrow, nil, createPRLimits())

	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(prID).To(Equal(101))
	g.Expect(closeCalled).To(BeFalse())
}

// TestFindOpenPR_ShrinkSurvivesLabelAndCloseFailure covers the split-brain
// finding: when both the label write and the take-back close fail, the pull
// request is left open on GitHub with no direction label. The next
// FindOpenPR must not classify it as a grow just because directionFromLabels
// would default an unlabelled PR that way — it has to recover "shrink" from
// the branch name the PR was created with, since the branch is created
// atomically with the PR and cannot fail independently of it.
func TestFindOpenPR_ShrinkSurvivesLabelAndCloseFailure(t *testing.T) {
	g := NewWithT(t)
	original := labelRetryBackoff
	labelRetryBackoff = 0
	t.Cleanup(func() { labelRetryBackoff = original })

	var capturedRef string

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"default_branch": "main"}`)
	})
	mux.HandleFunc("/repos/o/r/git/ref/heads/main", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"object": {"sha": "base-sha"}}`)
	})
	mux.HandleFunc("/repos/o/r/git/refs", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"ref": "refs/heads/new-branch"}`)
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
	mux.HandleFunc("/repos/o/r/pulls", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			var payload struct {
				Head string `json:"head"`
			}
			body, err := io.ReadAll(r.Body)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(json.Unmarshal(body, &payload)).To(Succeed())
			capturedRef = payload.Head
			_, _ = fmt.Fprint(w, `{"number": 101, "state": "open"}`)
		case http.MethodGet:
			// The PR that CreatePR could neither label nor close, still open
			// with the branch it was created on and no direction label.
			g.Expect(capturedRef).ToNot(BeEmpty(), "PR creation must have happened first")
			_, _ = fmt.Fprintf(w, `[{"number": 101, "head": {"ref": %q}}]`, capturedRef)
		}
	})
	// Both the label write and the take-back close fail: same client, same
	// token, same permission scope, so a single outage takes both down
	// together, exactly as the finding describes.
	mux.HandleFunc("/repos/o/r/issues/101/labels", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	mux.HandleFunc("/repos/o/r/issues/101/comments", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})

	provider, teardown := newTestProvider(t, mux)
	defer teardown()

	prID, err := provider.CreatePR(context.Background(), "my-quota", "default", DirectionShrink, nil, createPRLimits())
	g.Expect(err).To(HaveOccurred())
	g.Expect(prID).To(Equal(0), "nothing must be persisted on the lease for an unrecovered PR")

	foundID, direction, err := provider.FindOpenPR(context.Background(), "default", "my-quota")
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(foundID).To(Equal(101))
	g.Expect(direction).To(Equal(DirectionShrink),
		"the branch name must carry the direction; falling back to the "+
			"no-label default would re-adopt this shrink as a grow and make "+
			"it auto-merge eligible")
}

// TestFindOpenPR_LegacyBranchFallsBackToLabel verifies that a PR opened
// before branches encoded their direction is still matched (never orphaned)
// and resolves its direction from the label, exactly as before.
func TestFindOpenPR_LegacyBranchFallsBackToLabel(t *testing.T) {
	g := NewWithT(t)

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/pulls", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `[{
			"number": 9,
			"head": {"ref": "resize/team-a-compute-1700000000"},
			"labels": [
				{"name": "resizer/managed"},
				{"name": "resizer/direction:shrink"}
			]
		}]`)
	})
	provider, teardown := newTestProvider(t, mux)
	defer teardown()

	id, direction, err := provider.FindOpenPR(context.Background(), "team-a", "compute")

	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(id).To(Equal(9))
	g.Expect(direction).To(Equal(DirectionShrink))
}

// TestFindOpenPR_ShrinkPrefixedNamespaceDoesNotCollideWithSiblingsShrinkBranch
// covers the B1 finding: a legacy-shape branch belonging to namespace
// "shrink-team" is byte-identical to what the new shrink-direction shape
// produces for namespace "team". A single pull request must not answer for
// both queries with two different, and for one of them wrong, directions.
func TestFindOpenPR_ShrinkPrefixedNamespaceDoesNotCollideWithSiblingsShrinkBranch(t *testing.T) {
	g := NewWithT(t)

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/pulls", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// This pull request belongs to namespace "shrink-team", quota
		// "compute", and predates direction-in-branch (no labels).
		_, _ = fmt.Fprint(w, `[{
			"number": 77,
			"head": {"ref": "resize/shrink-team-compute-1700000000"}
		}]`)
	})
	provider, teardown := newTestProvider(t, mux)
	defer teardown()

	// A query for namespace "team" must not find a pull request that
	// belongs to namespace "shrink-team".
	id, _, err := provider.FindOpenPR(context.Background(), "team", "compute")
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(id).To(Equal(0),
		"a pull request belonging to namespace shrink-team must not answer for namespace team")

	// The actual owner, "shrink-team", must still find it and resolve its
	// direction through the label fallback (none present -> grow).
	id, direction, err := provider.FindOpenPR(context.Background(), "shrink-team", "compute")
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(id).To(Equal(77))
	g.Expect(direction).To(Equal(DirectionGrow))
}

// TestFindOpenPR_NewShapeAndLegacySiblingBothResolveCorrectly is the
// two-pull-request version of the same case: namespace "team" has its own
// new-shape shrink branch open at the same time namespace "shrink-team" has
// a legacy-shape branch open, and each query must find only its own.
func TestFindOpenPR_NewShapeAndLegacySiblingBothResolveCorrectly(t *testing.T) {
	g := NewWithT(t)

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/pulls", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `[
			{"number": 201, "head": {"ref": "resize/shrink/team/compute/1700000000"}},
			{"number": 202, "head": {"ref": "resize/shrink-team-compute-1700000001"}}
		]`)
	})
	provider, teardown := newTestProvider(t, mux)
	defer teardown()

	id, direction, err := provider.FindOpenPR(context.Background(), "team", "compute")
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(id).To(Equal(201))
	g.Expect(direction).To(Equal(DirectionShrink))

	id, direction, err = provider.FindOpenPR(context.Background(), "shrink-team", "compute")
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(id).To(Equal(202))
	g.Expect(direction).To(Equal(DirectionGrow))
}

// TestFindOpenPR_OwnNewShapePRBeatsAnAmbiguousLegacyOne pins that a
// legacy match never wins on list position alone.
//
// The legacy shape cannot say where the namespace ends and the quota begins:
// "resize/team-a-compute-…" reads equally as namespace "team-a" quota
// "compute" and as namespace "team" quota "a-compute". So a query for
// ("team", "a-compute") matches a branch that in fact belongs to a
// neighbouring namespace. If the scan returned the first match in list order,
// GitHub's ordering would decide whether "team" adopts its own pull request
// or its neighbour's — and adopting the neighbour's means recording it in
// "team"'s lease, where it becomes a merge and update candidate. Listing the
// ambiguous legacy entry first is the case that must not decide anything.
func TestFindOpenPR_OwnNewShapePRBeatsAnAmbiguousLegacyOne(t *testing.T) {
	g := NewWithT(t)

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/pulls", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `[
			{"number": 501, "head": {"ref": "resize/team-a-compute-1700000000"}},
			{"number": 500, "head": {"ref": "resize/shrink/team/a-compute/1700000001"}}
		]`)
	})
	provider, teardown := newTestProvider(t, mux)
	defer teardown()

	id, direction, err := provider.FindOpenPR(context.Background(), "team", "a-compute")
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(id).To(Equal(500),
		"the namespace's own unambiguous pull request must win over an ambiguous legacy match listed before it")
	g.Expect(direction).To(Equal(DirectionShrink))

	// The legacy pull request still answers for the namespace that really
	// owns it, which has no new-shape pull request of its own.
	id, direction, err = provider.FindOpenPR(context.Background(), "team-a", "compute")
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(id).To(Equal(501))
	g.Expect(direction).To(Equal(DirectionGrow))
}

// TestFindOpenPR_QuotaNameIsNotMatchedAsAPrefix pins the trailing "/" on the
// new-shape prefixes. Without it, quota "compute" would match a branch
// belonging to quota "compute2", and the controller would adopt and then
// rewrite a different quota's pull request.
func TestFindOpenPR_QuotaNameIsNotMatchedAsAPrefix(t *testing.T) {
	g := NewWithT(t)

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/pulls", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `[
			{"number": 300, "head": {"ref": "resize/grow/team/compute2/1700000000"}}
		]`)
	})
	provider, teardown := newTestProvider(t, mux)
	defer teardown()

	id, _, err := provider.FindOpenPR(context.Background(), "team", "compute")
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(id).To(Equal(0),
		"quota compute must not match a branch belonging to quota compute2")

	// The namespace is guarded the same way.
	id, _, err = provider.FindOpenPR(context.Background(), "tea", "compute2")
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(id).To(Equal(0), "namespace tea must not match a branch belonging to namespace team")

	id, _, err = provider.FindOpenPR(context.Background(), "team", "compute2")
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(id).To(Equal(300))
}
