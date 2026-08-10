// pkg/api/types.go
// Shared types used by both CollectorCtrl Server and K8s Operator.
// This package MUST NOT import server-specific or k8s-specific packages.

package api

import (
	"time"
)

// AgentType identifies the deployment platform of an agent.
type AgentType string

const (
	AgentTypeVM       AgentType = "vm"
	AgentTypeWindows  AgentType = "windows"
	AgentTypeLinux    AgentType = "linux"
	AgentTypeKubernetes AgentType = "kubernetes"
)

// Agent represents a single collector instance or a cluster-level aggregate.
type Agent struct {
	// ID is a globally unique identifier for this agent.
	// Format depends on platform:
	//   VM:       "vm://<hostname>"
	//   Windows:  "windows://<hostname>"
	//   K8s Pod:  "k8s://<cluster>/<namespace>/<pod-name>"
	//   K8s Fleet:"k8s://<cluster>/<namespace>/<workload-kind>/<workload-name>"
	ID string `json:"id"`

	// Type identifies the deployment platform.
	Type AgentType `json:"type"`

	// DisplayName is the human-friendly name shown in the UI.
	DisplayName string `json:"displayName"`

	// Hostname of the machine or node running the collector.
	Hostname string `json:"hostname"`

	// Version of the collector binary (e.g., "0.123.0").
	Version string `json:"version"`

	// EffectiveConfig is the runtime YAML config currently in use.
	EffectiveConfig string `json:"effectiveConfig"`

	// EffectiveConfigHash is a SHA256 hash of EffectiveConfig for quick comparison.
	EffectiveConfigHash string `json:"effectiveConfigHash"`

	// DesiredConfigHash is the hash of the config the server wants applied.
	DesiredConfigHash string `json:"desiredConfigHash"`

	// Health of the agent.
	Health HealthStatus `json:"health"`

	// LastSeen is the timestamp of the last OpAMP heartbeat.
	LastSeen time.Time `json:"lastSeen"`

	// Labels are user-defined or auto-populated key-value pairs for targeting.
	// Auto-populated labels for K8s include:
	//   k8s.cluster.name, k8s.namespace, k8s.node.name, k8s.workload.type, k8s.workload.name
	Labels map[string]string `json:"labels"`

	// Kubernetes is populated only when Type == AgentTypeKubernetes.
	// For a single pod agent, this describes that pod.
	// For a fleet-level aggregate, this describes the workload.
	Kubernetes *K8sContext `json:"kubernetes,omitempty"`

	// MetricsSnapshot contains the latest collector self-metrics.
	MetricsSnapshot map[string]float64 `json:"metricsSnapshot,omitempty"`
}

// K8sContext describes the Kubernetes location and identity of a collector.
type K8sContext struct {
	// ClusterName is the human-readable cluster identifier (e.g., "eks-prod-us-east-1").
	ClusterName string `json:"clusterName"`

	// ClusterID is a unique cluster identifier (EKS ARN, GKE project+name, etc.).
	ClusterID string `json:"clusterID"`

	// Namespace of the collector workload.
	Namespace string `json:"namespace"`

	// NodeName is the Kubernetes node this pod is scheduled on.
	NodeName string `json:"nodeName"`

	// PodName is the name of the individual pod.
	// Empty for fleet-level aggregate agents.
	PodName string `json:"podName,omitempty"`

	// PodUID is the Kubernetes pod UUID.
	PodUID string `json:"podUID,omitempty"`

	// WorkloadType is the Kubernetes controller kind: DaemonSet, Deployment, StatefulSet.
	WorkloadType string `json:"workloadType"`

	// WorkloadName is the name of the DaemonSet/Deployment/StatefulSet.
	WorkloadName string `json:"workloadName"`

	// ContainerName is the collector container within the pod.
	ContainerName string `json:"containerName"`

	// GitCommit is the Git SHA of the config this pod is running.
	// Populated by the Operator by reading a pod annotation or ConfigMap label.
	GitCommit string `json:"gitCommit,omitempty"`

	// GitRepo is the repository URL (e.g., "github.com/acme/otel-configs").
	GitRepo string `json:"gitRepo,omitempty"`

	// ConfigMapName is the name of the ConfigMap holding the collector config.
	ConfigMapName string `json:"configMapName,omitempty"`

	// ConfigMapKey is the key within the ConfigMap (e.g., "relay.yaml").
	ConfigMapKey string `json:"configMapKey,omitempty"`
}

// HealthStatus represents the operational health of an agent.
type HealthStatus string

const (
	HealthHealthy   HealthStatus = "healthy"
	HealthDegraded  HealthStatus = "degraded"
	HealthUnhealthy HealthStatus = "unhealthy"
	HealthUnknown   HealthStatus = "unknown"
	HealthDrift     HealthStatus = "drift" // Config differs from desired state
)

// FleetPolicy defines a targeting rule for config distribution.
type FleetPolicy struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Description string            `json:"description"`
	MatchLabels map[string]string `json:"matchLabels"` // label selectors
	ConfigYAML  string            `json:"configYaml"`  // the OTel config to push
	Rollout     *RolloutStrategy  `json:"rollout,omitempty"`
}

// RolloutStrategy defines how a config change is rolled out across a fleet.
type RolloutStrategy struct {
	// CanaryPercentage is the initial percentage of agents to target (1-100).
	CanaryPercentage int `json:"canaryPercentage"`

	// ObservationWindow is how long to wait before evaluating canary health.
	ObservationWindow time.Duration `json:"observationWindow"`

	// AutoPromote, if true, automatically increases to 100% if canary is healthy.
	AutoPromote bool `json:"autoPromote"`

	// AutoRollback, if true, reverts to previous config if canary is unhealthy.
	AutoRollback bool `json:"autoRollback"`
}

// ConfigChangeRequest is sent from the UI to the server when a user edits a config.
type ConfigChangeRequest struct {
	// TargetPolicyID identifies which policy (and thus which agents) to update.
	TargetPolicyID string `json:"targetPolicyID"`

	// NewConfigYAML is the updated collector configuration.
	NewConfigYAML string `json:"newConfigYAML"`

	// IsEmergency, if true, bypasses Git and pushes directly via Operator emergency mode.
	IsEmergency bool `json:"isEmergency"`

	// GitCommitMessage is the message for the Git commit (ignored if emergency).
	GitCommitMessage string `json:"gitCommitMessage"`
}

// ConfigChangeResult reports the outcome of a config change attempt.
type ConfigChangeResult struct {
	ChangeID    string    `json:"changeID"`
	Status      string    `json:"status"`      // pending, applied, failed, rolled_back
	GitCommit   string    `json:"gitCommit"`   // empty for emergency
	GitPRURL    string    `json:"gitPRURL"`    // empty for emergency
	AffectedAgents []string `json:"affectedAgents"`
	Error       string    `json:"error,omitempty"`
	StartedAt   time.Time `json:"startedAt"`
	CompletedAt *time.Time `json:"completedAt,omitempty"`
}
