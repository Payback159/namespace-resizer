package git

import (
	"context"
	"math/rand"
	"sync"

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
		MergeableState: MergeableStateClean,
		ChecksState:    ChecksStateSuccess,
	}, nil
}

func (p *LogOnlyProvider) CreatePR(
	ctx context.Context,
	quotaName, namespace, direction string,
	annotations map[string]string,
	newLimits map[corev1.ResourceName]resource.Quantity,
) (int, error) {
	log.FromContext(ctx).Info("Would create pull request",
		"namespace", namespace, "quota", quotaName,
		"direction", direction, "limits", newLimits)

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

// FindOpenPR always reports no existing PR for the stateless log provider.
func (p *LogOnlyProvider) FindOpenPR(
	ctx context.Context,
	namespace, quotaName string,
) (int, string, error) {
	return 0, "", nil
}

func (p *LogOnlyProvider) ClosePR(ctx context.Context, prID int, comment string) error {
	log.FromContext(ctx).Info("Would close pull request",
		"prID", prID, "comment", comment)
	return nil
}

// StatefulLogProvider allows simulating state changes for the demo
type PRDetails struct {
	Namespace string
	QuotaName string
	Direction string
	NewLimits map[corev1.ResourceName]resource.Quantity
	Status    *PRStatus
}

type StatefulLogProvider struct {
	mu  sync.RWMutex
	prs map[int]*PRDetails
}

func NewStatefulLogProvider() *StatefulLogProvider {
	return &StatefulLogProvider{
		prs: make(map[int]*PRDetails),
	}
}

func (p *StatefulLogProvider) GetPRStatus(ctx context.Context, prID int) (*PRStatus, error) {
	logger := log.FromContext(ctx)
	p.mu.RLock()
	details, ok := p.prs[prID]
	var statusCopy *PRStatus
	if ok && details.Status != nil {
		s := *details.Status
		statusCopy = &s
	}
	p.mu.RUnlock()

	if ok {
		logger.Info("StatefulLogProvider: Found PR", "prID", prID, "status", statusCopy)
		return statusCopy, nil
	}
	logger.Info("StatefulLogProvider: PR not found, returning default", "prID", prID)
	// Default to open/clean
	return &PRStatus{
		IsOpen:         true,
		IsMerged:       false,
		Mergeable:      true,
		MergeableState: MergeableStateClean,
	}, nil
}

func (p *StatefulLogProvider) CreatePR(
	ctx context.Context,
	quotaName, namespace, direction string,
	annotations map[string]string,
	newLimits map[corev1.ResourceName]resource.Quantity,
) (int, error) {
	logger := log.FromContext(ctx)
	id := rand.Intn(1000) + 1000
	logger.Info("GitOps Simulation: Creating PR", "namespace", namespace, "quota", quotaName, "direction", direction, "prID", id)

	p.mu.Lock()
	p.prs[id] = &PRDetails{
		Namespace: namespace,
		QuotaName: quotaName,
		Direction: direction,
		NewLimits: newLimits,
		Status: &PRStatus{
			IsOpen:         true,
			IsMerged:       false,
			Mergeable:      true,
			MergeableState: MergeableStateClean,
		},
	}
	p.mu.Unlock()
	logger.Info("StatefulLogProvider: Stored PR", "prID", id)
	return id, nil
}

func (p *StatefulLogProvider) UpdatePR(ctx context.Context, prID int, quotaName, namespace string, annotations map[string]string, newLimits map[corev1.ResourceName]resource.Quantity) error {
	logger := log.FromContext(ctx)
	logger.Info("GitOps Simulation: Updating PR", "prID", prID)
	return nil
}

// FindOpenPR returns the number and direction of a stored, still-open PR
// matching the given namespace/quota, or 0 and an empty direction if none
// exists. This mirrors the GitHub provider so the orphaned-PR recovery path
// can be exercised in dry-run/simulation mode.
func (p *StatefulLogProvider) FindOpenPR(
	ctx context.Context,
	namespace, quotaName string,
) (int, string, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for id, details := range p.prs {
		if details.Namespace == namespace && details.QuotaName == quotaName &&
			details.Status != nil && details.Status.IsOpen {
			return id, details.Direction, nil
		}
	}
	return 0, "", nil
}

func (p *StatefulLogProvider) MergePR(ctx context.Context, prID int, method string) error {
	logger := log.FromContext(ctx)
	logger.Info("GitOps Simulation: Merging PR", "prID", prID)

	p.mu.Lock()
	details, ok := p.prs[prID]
	if ok {
		details.Status.IsOpen = false
		details.Status.IsMerged = true
	}
	p.mu.Unlock()

	if ok {
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

// ClosePR marks the stored PR as closed (without merging) and logs the
// comment, mirroring MergePR's bookkeeping.
func (p *StatefulLogProvider) ClosePR(ctx context.Context, prID int, comment string) error {
	logger := log.FromContext(ctx)
	logger.Info("GitOps Simulation: Closing PR", "prID", prID, "comment", comment)

	p.mu.Lock()
	details, ok := p.prs[prID]
	if ok {
		details.Status.IsOpen = false
	}
	p.mu.Unlock()

	if ok {
		logger.Info("StatefulLogProvider: Closed PR", "prID", prID, "newStatus", details.Status)
	} else {
		logger.Info("StatefulLogProvider: PR not found for close", "prID", prID)
	}
	return nil
}
