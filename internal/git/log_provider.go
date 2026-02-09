package git

import (
	"context"
	"math/rand"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// LogOnlyProvider simulates Git operations by logging them.
// Useful for local testing without a real GitHub connection.
type LogOnlyProvider struct{}

func NewLogOnlyProvider() *LogOnlyProvider {
	return &LogOnlyProvider{}
}

func (p *LogOnlyProvider) GetPRStatus(ctx context.Context, prID int) (*PRStatus, error) {
	// Simulate a PR that is open and mergeable
	// In a real demo, we might want to simulate merging after some time?
	// For now, let's say it's always open and clean.
	return &PRStatus{
		IsOpen:         true,
		IsMerged:       false,
		Mergeable:      true,
		MergeableState: "clean",
		ChecksState:    "success",
	}, nil
}

func (p *LogOnlyProvider) CreatePR(ctx context.Context, quotaName, namespace string, annotations map[string]string, newLimits map[corev1.ResourceName]resource.Quantity) (int, error) {
	logger := log.FromContext(ctx)
	logger.Info("GitOps Simulation: Creating PR", "namespace", namespace, "quota", quotaName, "newLimits", newLimits)

	// Return a random PR ID
	return rand.Intn(1000) + 1000, nil
}

func (p *LogOnlyProvider) UpdatePR(ctx context.Context, prID int, quotaName, namespace string, annotations map[string]string, newLimits map[corev1.ResourceName]resource.Quantity) error {
	logger := log.FromContext(ctx)
	logger.Info("GitOps Simulation: Updating PR", "prID", prID, "newLimits", newLimits)
	return nil
}

func (p *LogOnlyProvider) MergePR(ctx context.Context, prID int, method string) error {
	logger := log.FromContext(ctx)
	logger.Info("GitOps Simulation: Merging PR", "prID", prID, "method", method)

	// Simulate successful merge
	// Note: In the real controller loop, we check GetPRStatus again.
	// Since this provider is stateless, GetPRStatus will still return "Open".
	// This might cause an infinite loop in the demo if we rely on status changes.
	// However, for "Auto-Merge", the controller calls MergePR and then Requeues.
	// If we want to simulate the full lifecycle, we might need a bit of state here.
	return nil
}

// StatefulLogProvider allows simulating state changes for the demo
type PRDetails struct {
	Namespace string
	QuotaName string
	NewLimits map[corev1.ResourceName]resource.Quantity
	Status    *PRStatus
}

type StatefulLogProvider struct {
	prs map[int]*PRDetails
}

func NewStatefulLogProvider() *StatefulLogProvider {
	return &StatefulLogProvider{
		prs: make(map[int]*PRDetails),
	}
}

func (p *StatefulLogProvider) GetPRStatus(ctx context.Context, prID int) (*PRStatus, error) {
	logger := log.FromContext(ctx)
	if details, ok := p.prs[prID]; ok {
		logger.Info("StatefulLogProvider: Found PR", "prID", prID, "status", details.Status)
		return details.Status, nil
	}
	logger.Info("StatefulLogProvider: PR not found, returning default", "prID", prID)
	// Default to open/clean
	return &PRStatus{
		IsOpen:         true,
		IsMerged:       false,
		Mergeable:      true,
		MergeableState: "clean",
	}, nil
}

func (p *StatefulLogProvider) CreatePR(ctx context.Context, quotaName, namespace string, annotations map[string]string, newLimits map[corev1.ResourceName]resource.Quantity) (int, error) {
	logger := log.FromContext(ctx)
	id := rand.Intn(1000) + 1000
	logger.Info("GitOps Simulation: Creating PR", "namespace", namespace, "quota", quotaName, "prID", id)

	p.prs[id] = &PRDetails{
		Namespace: namespace,
		QuotaName: quotaName,
		NewLimits: newLimits,
		Status: &PRStatus{
			IsOpen:         true,
			IsMerged:       false,
			Mergeable:      true,
			MergeableState: "clean",
		},
	}
	logger.Info("StatefulLogProvider: Stored PR", "prID", id)
	return id, nil
}

func (p *StatefulLogProvider) UpdatePR(ctx context.Context, prID int, quotaName, namespace string, annotations map[string]string, newLimits map[corev1.ResourceName]resource.Quantity) error {
	logger := log.FromContext(ctx)
	logger.Info("GitOps Simulation: Updating PR", "prID", prID)
	return nil
}

func (p *StatefulLogProvider) MergePR(ctx context.Context, prID int, method string) error {
	logger := log.FromContext(ctx)
	logger.Info("GitOps Simulation: Merging PR", "prID", prID)

	if details, ok := p.prs[prID]; ok {
		details.Status.IsOpen = false
		details.Status.IsMerged = true
		logger.Info("StatefulLogProvider: Merged PR", "prID", prID, "newStatus", details.Status)

		// Log what would be synced — actual sync is done by ArgoCD (or manually in E2E tests).
		// DRY_RUN does not modify cluster resources.
		logger.Info("StatefulLogProvider: DRY_RUN - would sync ResourceQuota",
			"namespace", details.Namespace,
			"name", details.QuotaName,
			"newLimits", details.NewLimits,
		)
	} else {
		logger.Info("StatefulLogProvider: PR not found for merge", "prID", prID)
	}
	return nil
}
