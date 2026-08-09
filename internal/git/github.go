package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"text/template"
	"time"

	"github.com/bradleyfalzon/ghinstallation/v2"
	"github.com/google/go-github/v75/github"
	"golang.org/x/oauth2"
	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

var ErrFileNotFound = errors.New("file not found")

// GitHub pull request mergeable_state values that the auto-merge logic checks.
const (
	MergeableStateClean   = "clean"
	MergeableStateBlocked = "blocked"
	// ChecksStateSuccess is the GitHub combined commit status "success" state.
	ChecksStateSuccess = "success"
)

// Pull request directions. They are persisted as a GitHub label so an
// orphaned PR can be classified without any local state.
const (
	DirectionGrow   = "grow"
	DirectionShrink = "shrink"

	labelManaged         = "resizer/managed"
	labelDirectionPrefix = "resizer/direction:"
)

type Provider interface {
	GetPRStatus(ctx context.Context, prID int) (*PRStatus, error)
	MergePR(ctx context.Context, prID int, method string) error
	CreatePR(ctx context.Context, quotaName, namespace, direction string,
		annotations map[string]string,
		newLimits map[corev1.ResourceName]resource.Quantity) (int, error)
	UpdatePR(ctx context.Context, prID int, quotaName, namespace string,
		annotations map[string]string,
		newLimits map[corev1.ResourceName]resource.Quantity) error
	// FindOpenPR returns the number and the direction of an existing open PR
	// managed by the resizer, or 0 and an empty direction if none exists.
	FindOpenPR(ctx context.Context, namespace, quotaName string) (int, string, error)
	// ClosePR posts comment on the pull request and then closes it without
	// merging.
	ClosePR(ctx context.Context, prID int, comment string) error
}

type PRStatus struct {
	IsOpen           bool
	IsMerged         bool
	Mergeable        bool
	MergeableState   string
	ChecksState      string
	ChecksTotalCount int
	// CreatedAt is when the pull request was opened. The shrink TTL is
	// measured against it.
	CreatedAt time.Time
}

type GitHubProvider struct {
	client       *github.Client
	owner        string
	repo         string
	clusterName  string
	pathTemplate *template.Template
}

func NewGitHubProvider(token, owner, repo, clusterName, pathTmpl string) *GitHubProvider {
	ts := oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: token},
	)
	tc := oauth2.NewClient(context.Background(), ts)

	tmpl := template.Must(template.New("path").Parse(pathTmpl))

	return &GitHubProvider{
		client:       github.NewClient(tc),
		owner:        owner,
		repo:         repo,
		clusterName:  clusterName,
		pathTemplate: tmpl,
	}
}

func NewGitHubAppProvider(appID, installationID int64, privateKey []byte, owner, repo, clusterName, pathTmpl string) (*GitHubProvider, error) {
	itr, err := ghinstallation.New(http.DefaultTransport, appID, installationID, privateKey)
	if err != nil {
		return nil, err
	}

	tmpl, err := template.New("path").Parse(pathTmpl)
	if err != nil {
		return nil, err
	}

	return &GitHubProvider{
		client:       github.NewClient(&http.Client{Transport: itr}),
		owner:        owner,
		repo:         repo,
		clusterName:  clusterName,
		pathTemplate: tmpl,
	}, nil
}

func (g *GitHubProvider) resolvePath(namespace string, annotations map[string]string) (string, error) {
	// 1. Check Annotation Override
	if val, ok := annotations["resizer.io/git-path"]; ok {
		return val, nil
	}

	// 2. Use Template
	data := struct {
		Cluster   string
		Namespace string
	}{
		Cluster:   g.clusterName,
		Namespace: namespace,
	}

	var buf bytes.Buffer
	if err := g.pathTemplate.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func (g *GitHubProvider) GetPRStatus(ctx context.Context, prID int) (*PRStatus, error) {
	pr, _, err := g.client.PullRequests.Get(ctx, g.owner, g.repo, prID)
	if err != nil {
		return nil, err
	}

	var checksState string
	var checksTotalCount int
	if pr.Head != nil && pr.Head.SHA != nil {
		status, _, err := g.client.Repositories.GetCombinedStatus(ctx, g.owner, g.repo, *pr.Head.SHA, nil)
		if err != nil {
			// Do not silently swallow this error: a failed status lookup would leave
			// checksTotalCount at 0, which the auto-merge logic interprets as "no CI"
			// and could bypass required checks. Surface it so the caller can retry.
			return nil, fmt.Errorf("failed to get combined status for PR %d: %w", prID, err)
		}
		checksState = status.GetState()
		if status.TotalCount != nil {
			checksTotalCount = *status.TotalCount
		}
	}

	return &PRStatus{
		IsOpen:           pr.GetState() == "open",
		IsMerged:         pr.GetMerged(),
		Mergeable:        pr.GetMergeable(),
		MergeableState:   pr.GetMergeableState(),
		ChecksState:      checksState,
		ChecksTotalCount: checksTotalCount,
		CreatedAt:        pr.GetCreatedAt().Time,
	}, nil
}

func (g *GitHubProvider) MergePR(ctx context.Context, prID int, method string) error {
	if method == "" {
		method = "squash"
	}
	_, _, err := g.client.PullRequests.Merge(ctx, g.owner, g.repo, prID, "Auto-merge by Namespace Resizer", &github.PullRequestOptions{
		MergeMethod: method,
	})
	return err
}

func (g *GitHubProvider) CreatePR(ctx context.Context, quotaName, namespace, direction string, annotations map[string]string, newLimits map[corev1.ResourceName]resource.Quantity) (int, error) {
	// 1. Get default branch ref
	repo, _, err := g.client.Repositories.Get(ctx, g.owner, g.repo)
	if err != nil {
		return 0, fmt.Errorf("failed to get repo: %w", err)
	}
	baseRef, _, err := g.client.Git.GetRef(ctx, g.owner, g.repo, "refs/heads/"+repo.GetDefaultBranch())
	if err != nil {
		return 0, fmt.Errorf("failed to get base ref: %w", err)
	}

	// 2. Create new branch
	//
	// The branch name carries the direction. It is created atomically with the
	// pull request (this same call), so unlike the direction label — a
	// separate API call that can fail on its own — it can never end up
	// out of sync with what the pull request actually does. See
	// FindOpenPR and directionFromLabels for how the label still serves as
	// the fallback for pull requests opened before this encoding existed.
	//
	// The segments are joined with "/", not "-": the legacy shape
	// (resize/<namespace>-<quota>-<timestamp>) already used "-" as its
	// separator, so a namespace or quota name that happens to start with
	// "grow-" or "shrink-" can make a legacy branch byte-identical to a
	// new-shape one for a *different* namespace/quota pair (namespace
	// "shrink-team" legacy vs. namespace "team" new-shape shrink both
	// produce "resize/shrink-team-...-<ts>"). Kubernetes object names cannot
	// contain "/", so that collision is structurally impossible once "/"
	// separates the new shape's segments.
	branchName := fmt.Sprintf("resize/%s/%s/%s/%d", direction, namespace, quotaName, time.Now().Unix())
	newRef := github.CreateRef{
		Ref: "refs/heads/" + branchName,
		SHA: baseRef.Object.GetSHA(),
	}
	_, _, err = g.client.Git.CreateRef(ctx, g.owner, g.repo, newRef)
	if err != nil {
		return 0, fmt.Errorf("failed to create branch: %w", err)
	}

	// 3. Find the file
	basePath, err := g.resolvePath(namespace, annotations)
	if err != nil {
		return 0, fmt.Errorf("failed to resolve path: %w", err)
	}

	targetFile, fileContent, err := g.findQuotaFile(ctx, basePath, branchName, quotaName)
	if err != nil {
		return 0, fmt.Errorf("failed to find quota file in %s: %w", basePath, err)
	}

	content, err := fileContent.GetContent()
	if err != nil {
		return 0, err
	}

	// 4. Apply changes to content
	newContent := applyChangesToYaml(content, newLimits)

	// 5. Commit changes
	opts := &github.RepositoryContentFileOptions{
		Message:   github.Ptr(fmt.Sprintf("chore(%s): resize quota %s", namespace, quotaName)),
		Content:   []byte(newContent),
		SHA:       fileContent.SHA,
		Branch:    github.Ptr(branchName),
		Committer: &github.CommitAuthor{Name: github.Ptr("Namespace Resizer"), Email: github.Ptr("bot@resizer.io")},
	}
	_, _, err = g.client.Repositories.UpdateFile(ctx, g.owner, g.repo, targetFile, opts)
	if err != nil {
		return 0, fmt.Errorf("failed to commit file: %w", err)
	}

	// 6. Create PR
	title := fmt.Sprintf("Resize Quota %s in %s", quotaName, namespace)
	if direction == DirectionShrink {
		title = fmt.Sprintf("Shrink Quota %s in %s", quotaName, namespace)
	}
	newPR := &github.NewPullRequest{
		Title:               github.Ptr(title),
		Head:                github.Ptr(branchName),
		Base:                github.Ptr(repo.GetDefaultBranch()),
		Body:                github.Ptr(generatePRBody(namespace, quotaName, newLimits)),
		MaintainerCanModify: github.Ptr(true),
	}

	pr, _, err := g.client.PullRequests.Create(ctx, g.owner, g.repo, newPR)
	if err != nil {
		return 0, fmt.Errorf("failed to create PR: %w", err)
	}

	// 7. Add Labels
	labels := []string{
		labelManaged,
		fmt.Sprintf("resizer/ns:%s", namespace),
		labelDirectionPrefix + direction,
	}
	if err := g.addLabels(ctx, pr.GetNumber(), labels); err != nil {
		logger := log.FromContext(ctx)
		if direction != DirectionShrink {
			// A grow pull request with no label is recovered as grow anyway,
			// so the only cost is a less precise audit trail.
			logger.Error(err, "failed to label pull request",
				"pr", pr.GetNumber(), "direction", direction)
			return pr.GetNumber(), nil
		}
		// An unlabelled shrink is indistinguishable from a grow once this
		// process forgets it, and grow pull requests are the ones eligible
		// for auto-merge. Take the pull request back rather than leave a
		// mergeable shrink behind.
		logger.Error(err, "failed to label shrink pull request, closing it again",
			"pr", pr.GetNumber())
		if closeErr := g.ClosePR(ctx, pr.GetNumber(), unlabelledShrinkComment); closeErr != nil {
			return 0, fmt.Errorf(
				"failed to label shrink PR %d (%w) and failed to close it again: %w",
				pr.GetNumber(), err, closeErr)
		}
		return 0, fmt.Errorf("failed to label shrink PR %d, closed it again: %w",
			pr.GetNumber(), err)
	}

	return pr.GetNumber(), nil
}

const unlabelledShrinkComment = "Closing this pull request: its direction " +
	"label could not be attached, and an unlabelled shrink proposal would " +
	"later be mistaken for a growth proposal. A replacement will be opened " +
	"on the next reconcile."

// labelAttempts is how often CreatePR tries to attach the labels before it
// treats the failure as final.
const labelAttempts = 3

// labelRetryBackoff is a variable so tests can zero it.
var labelRetryBackoff = 500 * time.Millisecond

func (g *GitHubProvider) addLabels(ctx context.Context, prNumber int, labels []string) error {
	var err error
	for attempt := 1; attempt <= labelAttempts; attempt++ {
		if _, _, err = g.client.Issues.AddLabelsToIssue(
			ctx, g.owner, g.repo, prNumber, labels); err == nil {
			return nil
		}
		if attempt == labelAttempts {
			break
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("labelling PR %d cancelled: %w", prNumber, ctx.Err())
		case <-time.After(labelRetryBackoff):
		}
	}
	return fmt.Errorf("failed to label PR %d after %d attempts: %w",
		prNumber, labelAttempts, err)
}

func (g *GitHubProvider) UpdatePR(ctx context.Context, prID int, quotaName, namespace string, annotations map[string]string, newLimits map[corev1.ResourceName]resource.Quantity) error {
	// 1. Get PR to find the branch
	pr, _, err := g.client.PullRequests.Get(ctx, g.owner, g.repo, prID)
	if err != nil {
		return err
	}

	branchName := pr.Head.GetRef()

	// 2. Find file again
	basePath, err := g.resolvePath(namespace, annotations)
	if err != nil {
		return err
	}

	targetFile, fileContent, err := g.findQuotaFile(ctx, basePath, branchName, quotaName)
	if err != nil {
		return err
	}

	content, err := fileContent.GetContent()
	if err != nil {
		return err
	}

	// 3. Apply new changes
	newContent := applyChangesToYaml(content, newLimits)

	// Check if content actually changed to avoid empty commits
	if newContent == content {
		return nil
	}

	// 4. Commit update
	opts := &github.RepositoryContentFileOptions{
		Message:   github.Ptr(fmt.Sprintf("chore(%s): update quota resize %s", namespace, quotaName)),
		Content:   []byte(newContent),
		SHA:       fileContent.SHA,
		Branch:    github.Ptr(branchName),
		Committer: &github.CommitAuthor{Name: github.Ptr("Namespace Resizer"), Email: github.Ptr("bot@resizer.io")},
	}
	_, _, err = g.client.Repositories.UpdateFile(ctx, g.owner, g.repo, targetFile, opts)
	if err != nil {
		return fmt.Errorf("failed to update file: %w", err)
	}

	// 5. Update PR Body
	// Only send the fields we intend to change. Passing the full PR object
	// returned by Get would also marshal head/base/state, which the Edit endpoint
	// rejects (422) because base must be a branch name, not an object.
	newBody := generatePRBody(namespace, quotaName, newLimits)
	update := &github.PullRequest{Body: github.Ptr(newBody)}
	_, _, err = g.client.PullRequests.Edit(ctx, g.owner, g.repo, prID, update)
	if err != nil {
		return fmt.Errorf("failed to update PR body: %w", err)
	}

	return nil
}

// FindOpenPR lists open pull requests and returns the number and direction of
// the one whose head branch matches the deterministic resizer branch prefix
// for the given namespace/quota. Returns 0 and an empty direction if no
// matching open PR exists.
//
// The direction is read from the branch name whenever the branch was created
// with it encoded (see CreatePR): that is structurally reliable, since the
// branch and the pull request are created in the same call and cannot drift
// apart. A branch predating that encoding falls back to directionFromLabels,
// the same recovery path used before this function knew about branch-encoded
// directions — an in-flight pull request from before the upgrade must still
// be found, not orphaned.
//
// The new-shape prefixes use "/" as their segment separator (see CreatePR):
// a Kubernetes namespace or quota name cannot contain "/", so
// growPrefix/shrinkPrefix can never match a legacyPrefix branch from a
// different namespace/quota pair — unlike an all-"-" scheme, where namespace
// "shrink-team" and namespace "team" could otherwise both produce
// "resize/shrink-team-...".
//
// A legacy match is only ever a fallback, never a first-past-the-post win.
// The legacy shape is ambiguous in a way the new one is not — "resize/team-a-
// compute-…" belongs to namespace "team-a" quota "compute" and to namespace
// "team" quota "a-compute" alike — so a namespace that has its own new-shape
// pull request must adopt that one regardless of where GitHub happens to list
// the two. Deciding per pull request would hand the outcome to list order and
// let a namespace adopt a neighbour's pull request, then update, close or
// merge it.
func (g *GitHubProvider) FindOpenPR(ctx context.Context, namespace, quotaName string) (int, string, error) {
	growPrefix := fmt.Sprintf("resize/%s/%s/%s/", DirectionGrow, namespace, quotaName)
	shrinkPrefix := fmt.Sprintf("resize/%s/%s/%s/", DirectionShrink, namespace, quotaName)
	legacyPrefix := fmt.Sprintf("resize/%s-%s-", namespace, quotaName)
	opts := &github.PullRequestListOptions{
		State:       "open",
		ListOptions: github.ListOptions{PerPage: 100},
	}

	legacyID := 0
	legacyDirection := ""

	for {
		prs, resp, err := g.client.PullRequests.List(ctx, g.owner, g.repo, opts)
		if err != nil {
			return 0, "", fmt.Errorf("failed to list pull requests: %w", err)
		}
		for _, pr := range prs {
			if pr.Head == nil {
				continue
			}
			ref := pr.Head.GetRef()
			switch {
			case strings.HasPrefix(ref, growPrefix):
				return pr.GetNumber(), DirectionGrow, nil
			case strings.HasPrefix(ref, shrinkPrefix):
				return pr.GetNumber(), DirectionShrink, nil
			case strings.HasPrefix(ref, legacyPrefix):
				if legacyID == 0 {
					legacyID = pr.GetNumber()
					legacyDirection = directionFromLabels(pr.Labels)
				}
			}
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	if legacyID != 0 {
		return legacyID, legacyDirection, nil
	}
	return 0, "", nil
}

// directionFromLabels reads the direction label and never invents a value it
// did not recognise.
//
// No direction label at all means the pull request predates the label;
// classifying it as grow preserves the behaviour those pull requests were
// opened under. A label that is present but is not exactly "grow" is a
// different case: labels are writable by anyone with repository access, so an
// unrecognised value is evidence that something other than this controller
// wrote it. Reading it as shrink is the safe direction — shrink proposals are
// never auto-merged, so the cost of being wrong is one human review, whereas
// reading it as grow would cost an unreviewed merge of lowered limits.
//
// For the same reason every direction label is inspected rather than the first
// one found. Nothing stops a second one being added next to the label this
// controller wrote, and stopping at the first match would let a grow label
// pinned onto a genuine shrink decide the outcome by list order alone.
func directionFromLabels(labels []*github.Label) string {
	for _, label := range labels {
		name := label.GetName()
		if !strings.HasPrefix(name, labelDirectionPrefix) {
			continue
		}
		if strings.TrimPrefix(name, labelDirectionPrefix) != DirectionGrow {
			return DirectionShrink
		}
	}
	// Either no direction label at all, or every one of them said grow.
	return DirectionGrow
}

// ClosePR records why the pull request is being abandoned and then closes it.
// The comment is posted first: if closing fails the reason is still visible,
// whereas a closed PR with no explanation is confusing for reviewers.
func (g *GitHubProvider) ClosePR(ctx context.Context, prID int, comment string) error {
	body := &github.IssueComment{Body: github.Ptr(comment)}
	if _, _, err := g.client.Issues.CreateComment(
		ctx, g.owner, g.repo, prID, body); err != nil {
		return fmt.Errorf("failed to comment on PR %d: %w", prID, err)
	}

	update := &github.PullRequest{State: github.Ptr("closed")}
	if _, _, err := g.client.PullRequests.Edit(
		ctx, g.owner, g.repo, prID, update); err != nil {
		return fmt.Errorf("failed to close PR %d: %w", prID, err)
	}
	return nil
}

func (g *GitHubProvider) findQuotaFile(ctx context.Context, basePath, ref, quotaName string) (string, *github.RepositoryContent, error) {
	// List files in directory
	_, dirContent, _, err := g.client.Repositories.GetContents(ctx, g.owner, g.repo, basePath, &github.RepositoryContentGetOptions{Ref: ref})
	if err != nil {
		// Check if it's a 404
		var ghErr *github.ErrorResponse
		if errors.As(err, &ghErr) && ghErr.Response.StatusCode == http.StatusNotFound {
			return "", nil, fmt.Errorf("%w: %v", ErrFileNotFound, err)
		}
		return "", nil, err
	}

	for _, file := range dirContent {
		if file.GetType() != "file" {
			continue
		}
		if !strings.HasSuffix(file.GetName(), ".yaml") && !strings.HasSuffix(file.GetName(), ".yml") {
			continue
		}

		// Read file content to check if it contains the Quota
		fc, _, _, err := g.client.Repositories.GetContents(ctx, g.owner, g.repo, file.GetPath(), &github.RepositoryContentGetOptions{Ref: ref})
		if err != nil {
			continue
		}

		content, err := fc.GetContent()
		if err != nil {
			continue
		}

		// Simple check: Does it contain "kind: ResourceQuota" and "name: <quotaName>"?
		// This is a heuristic. A proper YAML parser would be better.
		if strings.Contains(content, "kind: ResourceQuota") && strings.Contains(content, fmt.Sprintf("name: %s", quotaName)) {
			return file.GetPath(), fc, nil
		}
	}

	return "", nil, fmt.Errorf("%w: quota %s not found in %s", ErrFileNotFound, quotaName, basePath)
}

// Helper functions

func generatePRBody(ns, quota string, limits map[corev1.ResourceName]resource.Quantity) string {
	var sb strings.Builder
	_, _ = fmt.Fprintf(&sb, "### Quota Resize Recommendation for `%s` in `%s`\n\n", quota, ns)
	sb.WriteString("The Namespace Resizer Controller detected a need to increase the following limits:\n\n")
	sb.WriteString("| Resource | New Limit |\n")
	sb.WriteString("| :--- | :--- |\n")
	for res, qty := range limits {
		_, _ = fmt.Fprintf(&sb, "| %s | %s |\n", res, qty.String())
	}
	sb.WriteString("\n\n*Generated automatically by Namespace Resizer*")
	return sb.String()
}

func applyChangesToYaml(content string, limits map[corev1.ResourceName]resource.Quantity) string {
	decoder := yaml.NewDecoder(strings.NewReader(content))
	var nodes []*yaml.Node

	for {
		var node yaml.Node
		if err := decoder.Decode(&node); err != nil {
			if err == io.EOF {
				break
			}
			// If parsing fails, return original content to avoid corrupting the file
			// or modifying the wrong resources via naive replacement.
			return content
		}
		nodes = append(nodes, &node)
	}

	// Helper to find a value node for a given key in a mapping node
	var findValueNode func(n *yaml.Node, key string) *yaml.Node
	findValueNode = func(n *yaml.Node, key string) *yaml.Node {
		if n.Kind == yaml.DocumentNode && len(n.Content) > 0 {
			return findValueNode(n.Content[0], key)
		}
		if n.Kind == yaml.MappingNode {
			for i := 0; i < len(n.Content); i += 2 {
				if n.Content[i].Value == key {
					return n.Content[i+1]
				}
			}
		}
		return nil
	}

	for _, node := range nodes {
		// Check Kind
		kindNode := findValueNode(node, "kind")
		if kindNode == nil || kindNode.Value != "ResourceQuota" {
			continue
		}

		// Navigate to spec -> hard
		specNode := findValueNode(node, "spec")
		if specNode != nil {
			hardNode := findValueNode(specNode, "hard")
			if hardNode != nil && hardNode.Kind == yaml.MappingNode {
				// We are in 'hard' map
				for res, qty := range limits {
					found := false
					// Try to find and update existing key
					for i := 0; i < len(hardNode.Content); i += 2 {
						keyNode := hardNode.Content[i]
						valNode := hardNode.Content[i+1]
						if matchesResourceKey(keyNode.Value, res) {
							valNode.Value = qty.String()
							valNode.Style = yaml.DoubleQuotedStyle
							found = true
							// Don't break, in case multiple aliases exist (e.g. cpu and requests.cpu)
						}
					}

					// If not found, append new key-value pair
					if !found {
						keyNode := &yaml.Node{
							Kind:  yaml.ScalarNode,
							Value: string(res),
						}
						valNode := &yaml.Node{
							Kind:  yaml.ScalarNode,
							Value: qty.String(),
							Style: yaml.DoubleQuotedStyle,
						}
						hardNode.Content = append(hardNode.Content, keyNode, valNode)
					}
				}
			}
		}
	}

	var buf bytes.Buffer
	encoder := yaml.NewEncoder(&buf)
	encoder.SetIndent(2)
	for _, node := range nodes {
		if err := encoder.Encode(node); err != nil {
			return content
		}
	}
	return buf.String()
}

func matchesResourceKey(key string, res corev1.ResourceName) bool {
	if key == string(res) {
		return true
	}
	// Handle short names
	switch res {
	case corev1.ResourceRequestsCPU:
		return key == "cpu" || key == "requests.cpu"
	case corev1.ResourceRequestsMemory:
		return key == "memory" || key == "requests.memory"
	case corev1.ResourceRequestsStorage:
		return key == "storage" || key == "requests.storage"
	}
	return false
}
