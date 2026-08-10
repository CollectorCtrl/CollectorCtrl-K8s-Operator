// operator/api/v1alpha1/collectormonitor_types.go
// CRD types for the CollectorCtrl K8s Operator.
// Uses kubebuilder markers for code generation and validation.

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// CollectorMonitorSpec defines the desired state of CollectorMonitor.
type CollectorMonitorSpec struct {
	// WorkloadSelector identifies the collector workload (DaemonSet, Deployment, StatefulSet)
	// that this monitor should watch and manage.
	// +kubebuilder:validation:Required
	WorkloadSelector WorkloadSelector `json:"workloadSelector"`

	// ConfigMapSelector identifies the ConfigMap containing the collector configuration.
	// If omitted, the operator will attempt to auto-discover the ConfigMap
	// by examining the workload's pod template volumes.
	// +optional
	ConfigMapSelector *ConfigMapSelector `json:"configMapSelector,omitempty"`

	// OpAMPServer is the WebSocket endpoint of the CollectorCtrl management server.
	// Example: wss://collectorctrl.corp.internal:4320/v1/opamp
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^wss?://.*$`
	OpAMPServer string `json:"opampServer"`

	// Auth configures how the Operator authenticates to the CollectorCtrl Server.
	// +optional
	Auth OpAMPAuth `json:"auth,omitempty"`

	// HealthCheck configures periodic health monitoring of collector pods.
	// +optional
	// +kubebuilder:default={enabled:true,interval:"30s",metricsPort:8888}
	HealthCheck HealthCheckConfig `json:"healthCheck,omitempty"`

	// DriftDetection enables comparison of effective runtime config vs. ConfigMap content.
	// +optional
	// +kubebuilder:default={enabled:true,interval:"60s"}
	DriftDetection DriftDetectionConfig `json:"driftDetection,omitempty"`

	// EmergencyMode allows the Operator to accept direct config patches from the server
	// for incident response. When disabled, the Operator is read-only.
	// +optional
	// +kubebuilder:default={enabled:true}
	EmergencyMode EmergencyModeConfig `json:"emergencyMode,omitempty"`

	// EnrichWithNodeMetadata adds node labels/annotations as OpAMP agent labels.
	// +optional
	// +kubebuilder:default=true
	EnrichWithNodeMetadata *bool `json:"enrichWithNodeMetadata,omitempty"`
}

// WorkloadSelector defines how to discover the collector workload.
type WorkloadSelector struct {
	// MatchLabels is a standard Kubernetes label selector.
	// Example: app.kubernetes.io/name: splunk-otel-collector
	// +kubebuilder:validation:Required
	MatchLabels map[string]string `json:"matchLabels"`

	// Kind restricts the search to a specific workload type.
	// +optional
	// +kubebuilder:validation:Enum=DaemonSet;Deployment;StatefulSet
	Kind string `json:"kind,omitempty"`

	// Name, if set, matches a specific workload by name.
	// If omitted, all workloads matching MatchLabels and Kind are monitored.
	// +optional
	Name string `json:"name,omitempty"`
}

// ConfigMapSelector identifies the ConfigMap holding collector config.
type ConfigMapSelector struct {
	// MatchLabels selects the ConfigMap by label.
	// +optional
	MatchLabels map[string]string `json:"matchLabels,omitempty"`

	// Name, if set, is the exact ConfigMap name.
	// +optional
	Name string `json:"name,omitempty"`

	// Key is the data key within the ConfigMap (e.g., "relay.yaml", "config.yaml").
	// +optional
	// +kubebuilder:default="config.yaml"
	Key string `json:"key,omitempty"`
}

// OpAMPAuth configures authentication to the CollectorCtrl Server.
type OpAMPAuth struct {
	// SecretRef points to a Kubernetes Secret containing the credentials.
	// The secret must have a key named "secret-key".
	// +optional
	SecretRef *SecretRef `json:"secretRef,omitempty"`

	// TODO: mTLS and OIDC support can be added here in future versions.
}

// SecretRef references a Kubernetes Secret.
type SecretRef struct {
	// Name of the Secret.
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// Namespace of the Secret. Defaults to the same namespace as the CollectorMonitor.
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// Key within the Secret that holds the credential. Defaults to "secret-key".
	// +optional
	// +kubebuilder:default="secret-key"
	Key string `json:"key,omitempty"`
}

// HealthCheckConfig defines how the Operator monitors collector pod health.
type HealthCheckConfig struct {
	// Enabled turns health checking on/off.
	// +optional
	// +kubebuilder:default=true
	Enabled *bool `json:"enabled,omitempty"`

	// Interval between health checks.
	// +optional
	// +kubebuilder:default="30s"
	Interval metav1.Duration `json:"interval,omitempty"`

	// MetricsPort is the port where the collector exposes /metrics.
	// +optional
	// +kubebuilder:default=8888
	MetricsPort int32 `json:"metricsPort,omitempty"`
}

// DriftDetectionConfig defines how the Operator detects config drift.
type DriftDetectionConfig struct {
	// Enabled turns drift detection on/off.
	// +optional
	// +kubebuilder:default=true
	Enabled *bool `json:"enabled,omitempty"`

	// Interval between drift checks.
	// +optional
	// +kubebuilder:default="60s"
	Interval metav1.Duration `json:"interval,omitempty"`
}

// EmergencyModeConfig controls whether the Operator can apply emergency overrides.
type EmergencyModeConfig struct {
	// Enabled allows the Operator to patch ConfigMaps directly when requested by the server.
	// +optional
	// +kubebuilder:default=true
	Enabled *bool `json:"enabled,omitempty"`
}

// CollectorMonitorStatus defines the observed state of CollectorMonitor.
type CollectorMonitorStatus struct {
	// Phase is the high-level state of the monitor.
	// +kubebuilder:validation:Enum=Pending;Active;Error;Disconnected
	Phase string `json:"phase,omitempty"`

	// LastHeartbeat is when the Operator last reported to the server.
	// +optional
	LastHeartbeat *metav1.Time `json:"lastHeartbeat,omitempty"`

	// AgentCount is the total number of collector pods discovered.
	// +optional
	AgentCount int32 `json:"agentCount,omitempty"`

	// HealthyAgents is the number of pods reporting healthy status.
	// +optional
	HealthyAgents int32 `json:"healthyAgents,omitempty"`

	// ConfigMapRef is the resolved ConfigMap being monitored.
	// +optional
	ConfigMapRef *ConfigMapReference `json:"configMapRef,omitempty"`

	// Conditions represent the latest available observations of the monitor's state.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ConfigMapReference points to the discovered ConfigMap.
type ConfigMapReference struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Key       string `json:"key"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=cmo;cmos
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Agents",type=integer,JSONPath=`.status.agentCount`
// +kubebuilder:printcolumn:name="Healthy",type=integer,JSONPath=`.status.healthyAgents`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// CollectorMonitor is the Schema for the collectormonitors API.
type CollectorMonitor struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   CollectorMonitorSpec   `json:"spec,omitempty"`
	Status CollectorMonitorStatus `json:"status,omitempty"`
}

// DeepCopyInto copies this CollectorMonitor into another.
func (in *CollectorMonitor) DeepCopyInto(out *CollectorMonitor) {
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Spec = in.Spec
	out.Status = in.Status
}

// DeepCopy returns a copy of this CollectorMonitor.
func (in *CollectorMonitor) DeepCopy() *CollectorMonitor {
	if in == nil {
		return nil
	}
	out := new(CollectorMonitor)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyObject implements runtime.Object.
func (in *CollectorMonitor) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

// CollectorMonitorList contains a list of CollectorMonitor.
type CollectorMonitorList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CollectorMonitor `json:"items"`
}

// DeepCopyInto copies this CollectorMonitorList into another.
func (in *CollectorMonitorList) DeepCopyInto(out *CollectorMonitorList) {
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]CollectorMonitor, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

// DeepCopy returns a copy of this CollectorMonitorList.
func (in *CollectorMonitorList) DeepCopy() *CollectorMonitorList {
	if in == nil {
		return nil
	}
	out := new(CollectorMonitorList)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyObject implements runtime.Object.
func (in *CollectorMonitorList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

// GroupVersion is group version used to register these objects.
var GroupVersion = schema.GroupVersion{Group: "collectorctrl.io", Version: "v1alpha1"}

// SchemeBuilder is used to add go types to the GroupVersionKind scheme.
var SchemeBuilder = &runtime.SchemeBuilder{}

// AddToScheme adds the types in this group-version to the given scheme.
func AddToScheme(s *runtime.Scheme) error {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypeWithName(GroupVersion.WithKind("CollectorMonitor"), &CollectorMonitor{})
		s.AddKnownTypeWithName(GroupVersion.WithKind("CollectorMonitorList"), &CollectorMonitorList{})
		metav1.AddToGroupVersion(s, GroupVersion)
		return nil
	})
	return SchemeBuilder.AddToScheme(s)
}
