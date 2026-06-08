package controller

import (
	"context"

	"github.com/payback159/namespace-resizer/internal/git"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

type FakeGitProvider struct {
	PRStatus   *git.PRStatus
	MergedPRID int

	// CreatePRID is the PR number returned by CreatePR (defaults to 1).
	CreatePRID int
	// ExistingPR, when non-zero, is returned by FindOpenPR to simulate an
	// orphaned PR that was created but never locked.
	ExistingPR int

	// Call counters for assertions.
	CreatePRCalls   int
	UpdatePRCalls   int
	FindOpenPRCalls int
}

func (f *FakeGitProvider) GetPRStatus(ctx context.Context, prID int) (*git.PRStatus, error) {
	return f.PRStatus, nil
}

func (f *FakeGitProvider) CreatePR(ctx context.Context, quotaName, namespace string, annotations map[string]string, newLimits map[corev1.ResourceName]resource.Quantity) (int, error) {
	f.CreatePRCalls++
	if f.CreatePRID != 0 {
		return f.CreatePRID, nil
	}
	return 1, nil
}

func (f *FakeGitProvider) UpdatePR(ctx context.Context, prID int, quotaName, namespace string, annotations map[string]string, newLimits map[corev1.ResourceName]resource.Quantity) error {
	f.UpdatePRCalls++
	return nil
}

func (f *FakeGitProvider) MergePR(ctx context.Context, prID int, method string) error {
	f.MergedPRID = prID
	return nil
}

func (f *FakeGitProvider) FindOpenPR(ctx context.Context, namespace, quotaName string) (int, error) {
	f.FindOpenPRCalls++
	return f.ExistingPR, nil
}
