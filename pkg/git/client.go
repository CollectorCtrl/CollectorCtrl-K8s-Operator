// pkg/git/client.go
// Git integration module for CollectorCtrl Server.
// Supports GitHub, GitLab, Azure DevOps, and generic Git (SSH/HTTP).

package git

import (
	"context"
	"fmt"
	"time"
)

// Provider identifies the Git hosting platform.
type Provider string

const (
	ProviderGitHub      Provider = "github"
	ProviderGitLab      Provider = "gitlab"
	ProviderAzureDevOps Provider = "azuredevops"
	ProviderGeneric     Provider = "generic"
)

// CommitRequest describes a file change to commit.
type CommitRequest struct {
	// Repo is the repository identifier.
	//   GitHub: "owner/repo"
	//   GitLab: "group/project"
	//   Azure:  "org/project/_git/repo"
	Repo string

	// Branch to commit to.
	Branch string

	// FilePath within the repo (e.g., "clusters/eks-prod/daemonset.yaml").
	FilePath string

	// Content is the new file contents.
	Content string

	// Message is the Git commit message.
	Message string

	// AuthorName and AuthorEmail for the commit.
	AuthorName  string
	AuthorEmail string
}

// PullRequestRequest describes a PR to create.
type PullRequestRequest struct {
	Repo        string
	Title       string
	Body        string
	HeadBranch  string // branch with changes
	BaseBranch  string // target branch (e.g., "main")
}

// PullRequestResult contains the created PR info.
type PullRequestResult struct {
	PRNumber int
	PRURL    string
	State    string // open, closed, merged
}

// FileResult is the content and metadata of a file.
type FileResult struct {
	Content   string
	SHA       string // blob SHA for GitHub
	CommitSHA string // commit SHA of the file at this path
}

// Client is the interface for all Git operations.
// Implementations are in sub-packages: github, gitlab, azure, generic.
type Client interface {
	// GetFile reads a file from a specific branch.
	GetFile(ctx context.Context, repo, branch, filePath string) (*FileResult, error)

	// CreateCommit writes a file directly to a branch (bypassing PR for speed).
	// Returns the new commit SHA.
	CreateCommit(ctx context.Context, req CommitRequest) (string, error)

	// CreatePullRequest opens a PR from head branch to base branch.
	CreatePullRequest(ctx context.Context, req PullRequestRequest) (*PullRequestResult, error)

	// FastForward commits directly to the base branch without a PR.
	// This is the "emergency" path: update file on main immediately.
	FastForward(ctx context.Context, req CommitRequest) (string, error)

	// GetLatestCommit returns the HEAD commit SHA of a branch.
	GetLatestCommit(ctx context.Context, repo, branch string) (string, error)
}

// Config holds the settings for creating a Git client.
type Config struct {
	Provider Provider

	// BaseURL is the API base URL.
	//   GitHub: "https://api.github.com" (or enterprise URL)
	//   GitLab: "https://gitlab.com/api/v4" (or self-hosted)
	BaseURL string

	// AuthToken is a personal access token or GitHub App installation token.
	AuthToken string

	// For SSH-based generic Git:
	SSHPrivateKey string
	SSHPassphrase string
	SSHUser       string

	// Commit author defaults (used if not overridden per-request)
	DefaultAuthorName  string
	DefaultAuthorEmail string
}

// NewClient creates a Git client based on the provider.
func NewClient(cfg Config) (Client, error) {
	switch cfg.Provider {
	case ProviderGitHub:
		return newGitHubClient(cfg)
	case ProviderGitLab:
		return newGitLabClient(cfg)
	case ProviderAzureDevOps:
		return newAzureDevOpsClient(cfg)
	case ProviderGeneric:
		return newGenericClient(cfg)
	default:
		return nil, fmt.Errorf("unsupported git provider: %s", cfg.Provider)
	}
}

// --- Emergency Sync Strategy ---

// EmergencySync commits an emergency config change directly to main,
// then returns the commit SHA so the server can track it.
type EmergencySync struct {
	client Client
}

// NewEmergencySync creates an emergency sync helper.
func NewEmergencySync(client Client) *EmergencySync {
	return &EmergencySync{client: client}
}

// Apply patches the ConfigMap content on the main branch immediately.
func (es *EmergencySync) Apply(ctx context.Context, repo, branch, filePath, content, reason string) (string, error) {
	req := CommitRequest{
		Repo:     repo,
		Branch:   branch,
		FilePath: filePath,
		Content:  content,
		Message:  fmt.Sprintf("[EMERGENCY] %s", reason),
	}
	return es.client.FastForward(ctx, req)
}

// ConfigSync is the standard GitOps flow: create a commit on a feature branch, open a PR.
type ConfigSync struct {
	client Client
}

// NewConfigSync creates a standard config sync helper.
func NewConfigSync(client Client) *ConfigSync {
	return &ConfigSync{client: client}
}

// ProposeChange creates a PR with a config update.
// The consumer (server) can then poll the PR status and merge via UI or auto-merge.
func (cs *ConfigSync) ProposeChange(ctx context.Context, repo, baseBranch, filePath, content, title, description string) (*PullRequestResult, error) {
	// Generate a unique branch name
	ts := time.Now().UTC().Format("20060102-150405")
	featureBranch := fmt.Sprintf("collectorctrl/config-update-%s", ts)

	// Step 1: Create commit on feature branch
	commitReq := CommitRequest{
		Repo:        repo,
		Branch:      featureBranch,
		FilePath:    filePath,
		Content:     content,
		Message:     fmt.Sprintf("collectorctrl: %s", title),
	}
	if _, err := cs.client.CreateCommit(ctx, commitReq); err != nil {
		return nil, fmt.Errorf("create commit: %w", err)
	}

	// Step 2: Open PR
	prReq := PullRequestRequest{
		Repo:       repo,
		Title:      title,
		Body:       description,
		HeadBranch: featureBranch,
		BaseBranch: baseBranch,
	}
	return cs.client.CreatePullRequest(ctx, prReq)
}
