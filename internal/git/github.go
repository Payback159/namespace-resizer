package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"text/template"
	"time"

	"github.com/bradleyfalzon/ghinstallation/v2"
	"github.com/google/go-github/v60/github"
	"golang.org/x/oauth2"
	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

var ErrFileNotFound = errors.New("file not found")

type Provider interface {
	GetPRStatus(ctx context.Context, prID int) (*PRStatus, error)
	MergePR(ctx context.Context, prID int, method string) error
	CreatePR(ctx context.Context, quotaName, namespace string, annotations map[string]string, newLimits map[corev1.ResourceName]resource.Quantity) (int, error)
	UpdatePR(ctx context.Context, prID int, quotaName, namespace string, annotations map[string]string, newLimits map[corev1.ResourceName]resource.Quantity) error
}

type PRStatus struct {
	IsOpen           bool
	IsMerged         bool
	Mergeable        bool
	MergeableState   string
	ChecksState      string
	ChecksTotalCount int
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
		if err == nil {
			checksState = status.GetState()
			if status.TotalCount != nil {
				checksTotalCount = *status.TotalCount
			}
		}
	}

	return &PRStatus{
		IsOpen:           pr.GetState() == "open",
		IsMerged:         pr.GetMerged(),
		Mergeable:        pr.GetMergeable(),
		MergeableState:   pr.GetMergeableState(),
		ChecksState:      checksState,
		ChecksTotalCount: checksTotalCount,
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

func (g *GitHubProvider) CreatePR(ctx context.Context, quotaName, namespace string, annotations map[string]string, newLimits map[corev1.ResourceName]resource.Quantity) (int, error) {
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
	branchName := fmt.Sprintf("resize/%s-%s-%d", namespace, quotaName, time.Now().Unix())
	newRef := &github.Reference{
		Ref: github.String("refs/heads/" + branchName),
		Object: &github.GitObject{
			SHA: baseRef.Object.SHA,
		},
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
		Message:   github.String(fmt.Sprintf("chore(%s): resize quota %s", namespace, quotaName)),
		Content:   []byte(newContent),
		SHA:       fileContent.SHA,
		Branch:    github.String(branchName),
		Committer: &github.CommitAuthor{Name: github.String("Namespace Resizer"), Email: github.String("bot@resizer.io")},
	}
	_, _, err = g.client.Repositories.UpdateFile(ctx, g.owner, g.repo, targetFile, opts)
	if err != nil {
		return 0, fmt.Errorf("failed to commit file: %w", err)
	}

	// 6. Create PR
	newPR := &github.NewPullRequest{
		Title:               github.String(fmt.Sprintf("Resize Quota %s in %s", quotaName, namespace)),
		Head:                github.String(branchName),
		Base:                github.String(repo.GetDefaultBranch()),
		Body:                github.String(generatePRBody(namespace, quotaName, newLimits)),
		MaintainerCanModify: github.Bool(true),
	}

	pr, _, err := g.client.PullRequests.Create(ctx, g.owner, g.repo, newPR)
	if err != nil {
		return 0, fmt.Errorf("failed to create PR: %w", err)
	}

	// 7. Add Labels
	_, _, err = g.client.Issues.AddLabelsToIssue(ctx, g.owner, g.repo, pr.GetNumber(), []string{"resizer/managed", fmt.Sprintf("resizer/ns:%s", namespace)})
	if err != nil {
		// Log error but don't fail the whole flow
		fmt.Printf("Failed to add labels: %v\n", err)
	}

	return pr.GetNumber(), nil
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
		Message:   github.String(fmt.Sprintf("chore(%s): update quota resize %s", namespace, quotaName)),
		Content:   []byte(newContent),
		SHA:       fileContent.SHA,
		Branch:    github.String(branchName),
		Committer: &github.CommitAuthor{Name: github.String("Namespace Resizer"), Email: github.String("bot@resizer.io")},
	}
	_, _, err = g.client.Repositories.UpdateFile(ctx, g.owner, g.repo, targetFile, opts)
	if err != nil {
		return fmt.Errorf("failed to update file: %w", err)
	}

	// 5. Update PR Body
	newBody := generatePRBody(namespace, quotaName, newLimits)
	pr.Body = github.String(newBody)
	_, _, err = g.client.PullRequests.Edit(ctx, g.owner, g.repo, prID, pr)
	if err != nil {
		return fmt.Errorf("failed to update PR body: %w", err)
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
	sb.WriteString(fmt.Sprintf("### Quota Resize Recommendation for `%s` in `%s`\n\n", quota, ns))
	sb.WriteString("The Namespace Resizer Controller detected a need to increase the following limits:\n\n")
	sb.WriteString("| Resource | New Limit |\n")
	sb.WriteString("| :--- | :--- |\n")
	for res, qty := range limits {
		sb.WriteString(fmt.Sprintf("| %s | %s |\n", res, qty.String()))
	}
	sb.WriteString("\n\n*Generated automatically by Namespace Resizer*")
	return sb.String()
}

func applyChangesToYaml(content string, limits map[corev1.ResourceName]resource.Quantity) string {
	var node yaml.Node
	if err := yaml.Unmarshal([]byte(content), &node); err != nil {
		// Fallback to naive implementation if parsing fails
		return applyChangesToYamlNaive(content, limits)
	}

	// Walk the AST to find spec.hard fields
	// We look for the path: spec -> hard -> [resourceName]
	updateYamlNode(&node, limits)

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&node); err != nil {
		return applyChangesToYamlNaive(content, limits)
	}

	return buf.String()
}

func updateYamlNode(node *yaml.Node, limits map[corev1.ResourceName]resource.Quantity) {
	// Recursive walk
	if node.Kind == yaml.DocumentNode {
		for _, child := range node.Content {
			updateYamlNode(child, limits)
		}
		return
	}

	if node.Kind == yaml.MappingNode {
		// Check if we are in "spec" -> "hard"
		// This is a simplified traversal. A robust one would track path context.
		// For now, we just look for keys that match our resources ANYWHERE in the file
		// which is safer than the string replace but still heuristic.
		// Ideally, we should verify we are under spec.hard.

		for i := 0; i < len(node.Content); i += 2 {
			keyNode := node.Content[i]
			valNode := node.Content[i+1]

			if keyNode.Kind == yaml.ScalarNode {
				// Check if this key matches any of our resources
				for res, qty := range limits {
					if matchesResourceKey(keyNode.Value, res) {
						// Update the value node
						valNode.Value = qty.String()
						valNode.Style = yaml.DoubleQuotedStyle // Force quotes for safety (e.g. "100m")
					}
				}
			}
			
			// Recurse into value (e.g. to find nested keys)
			updateYamlNode(valNode, limits)
		}
	}
}

func matchesResourceKey(key string, res corev1.ResourceName) bool {
	if key == string(res) {
		return true
	}
	// Handle short names
	if res == corev1.ResourceRequestsCPU && key == "cpu" {
		return true
	}
	if res == corev1.ResourceRequestsMemory && key == "memory" {
		return true
	}
	return false
}

func applyChangesToYamlNaive(content string, limits map[corev1.ResourceName]resource.Quantity) string {
	// Very naive implementation for MVP.
	// In production, use a YAML AST parser (like go-yaml/v3) to preserve comments.
	// Here we just look for "cpu: <value>" and replace it.

	lines := strings.Split(content, "\n")
	for i, line := range lines {
		for res, qty := range limits {
			// Determine keys to look for
			// "requests.cpu" -> look for "requests.cpu:" AND "cpu:"
			// "requests.memory" -> look for "requests.memory:" AND "memory:"
			keysToCheck := []string{fmt.Sprintf("%s:", res)}
			if res == corev1.ResourceRequestsCPU {
				keysToCheck = append(keysToCheck, "cpu:")
			} else if res == corev1.ResourceRequestsMemory {
				keysToCheck = append(keysToCheck, "memory:")
			}

			for _, key := range keysToCheck {
				// Check if line contains resource key (e.g. "cpu:")
				// We use TrimSpace to ensure we are matching the key, not just a substring
				trimmed := strings.TrimSpace(line)
				if strings.HasPrefix(trimmed, key) {
					// Replace the value
					// Assume format "  cpu: 1000m"
					parts := strings.Split(line, ":")
					if len(parts) >= 2 {
						// Keep indentation
						indent := parts[0]
						lines[i] = fmt.Sprintf("%s: %s", indent, qty.String())
						// Break inner loop (keys) once matched
						break
					}
				}
			}
		}
	}
	return strings.Join(lines, "\n")
}
