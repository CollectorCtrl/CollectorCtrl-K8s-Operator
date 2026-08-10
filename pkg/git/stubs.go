package git

import (
	"fmt"
)

func newGitHubClient(cfg Config) (Client, error) {
	return nil, fmt.Errorf("github provider not implemented")
}

func newGitLabClient(cfg Config) (Client, error) {
	return nil, fmt.Errorf("gitlab provider not implemented")
}

func newAzureDevOpsClient(cfg Config) (Client, error) {
	return nil, fmt.Errorf("azure devops provider not implemented")
}

func newGenericClient(cfg Config) (Client, error) {
	return nil, fmt.Errorf("generic provider not implemented")
}
