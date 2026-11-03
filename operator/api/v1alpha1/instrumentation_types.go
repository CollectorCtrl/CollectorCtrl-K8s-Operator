// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 CollectorCtrl

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// Labels and annotations of the auto-instrumentation feature.
const (
	// LabelInstrumentJava on a pod template opts a workload in. Values:
	// "true" instruments every new pod, "canary" instruments one pod per
	// ReplicaSet/StatefulSet/DaemonSet revision, anything else does nothing.
	LabelInstrumentJava = "collectorctrl.io/instrument-java"
	// LabelInstrumented is set on pods the webhook changed (value: language).
	LabelInstrumented = "collectorctrl.io/instrumented"
	// AnnotationInstrumentation names the Instrumentation profile to use
	// ("name" in the pod's namespace or "namespace/name").
	AnnotationInstrumentation = "collectorctrl.io/instrumentation"
	// AnnotationInstrumentContainer names the container to instrument when the
	// pod has more than one application container.
	AnnotationInstrumentContainer = "collectorctrl.io/instrument-container"
	// AnnotationInstrumentationStatus is written on every pod the webhook
	// looked at: "injected" or "skipped".
	AnnotationInstrumentationStatus = "collectorctrl.io/instrumentation-status"
	// AnnotationInstrumentationReason explains a skipped pod or notes limits of
	// an injected one (e.g. image ENV not checked).
	AnnotationInstrumentationReason = "collectorctrl.io/instrumentation-reason"
	// AnnotationInstrumentationMode is "canary" or "all" on injected pods.
	AnnotationInstrumentationMode = "collectorctrl.io/instrumentation-mode"
	// AnnotationInstrumentationRevision is the workload revision (pod template
	// hash or controller revision) an injected pod belongs to.
	AnnotationInstrumentationRevision = "collectorctrl.io/instrumentation-revision"
)

// Instrumentation modes requested through LabelInstrumentJava.
const (
	InstrumentModeAll    = "all"
	InstrumentModeCanary = "canary"
)

// Workload states in Instrumentation status.
const (
	WorkloadStateCanary   = "canary"
	WorkloadStateRolled   = "rolled-out"
	WorkloadStateReverted = "reverted"
)

// InstrumentationSpec is a central OpenTelemetry auto-instrumentation
// profile. Workloads opt in with the collectorctrl.io/instrument-java label;
// the profile says where telemetry goes and which agent image is used.
type InstrumentationSpec struct {
	// Exporter is the OTLP destination every instrumented process sends to.
	// +kubebuilder:validation:Required
	Exporter ExporterSpec `json:"exporter"`

	// Propagators sets OTEL_PROPAGATORS (default tracecontext,baggage).
	// +optional
	Propagators []string `json:"propagators,omitempty"`

	// Sampler sets OTEL_TRACES_SAMPLER and OTEL_TRACES_SAMPLER_ARG.
	// +optional
	Sampler *SamplerSpec `json:"sampler,omitempty"`

	// ResourceAttributes are added to OTEL_RESOURCE_ATTRIBUTES after the
	// Kubernetes attributes the webhook sets (namespace, pod, container).
	// +optional
	ResourceAttributes map[string]string `json:"resourceAttributes,omitempty"`

	// Env are extra variables for the instrumented container (OTEL_* settings).
	// A variable the container already defines is never overwritten.
	// +optional
	Env []corev1.EnvVar `json:"env,omitempty"`

	// Java configures the Java agent.
	// +optional
	Java JavaSpec `json:"java,omitempty"`

	// Registry controls whether the operator reads image configuration from
	// the container registry so JVM flags baked into the image (Dockerfile ENV
	// JAVA_TOOL_OPTIONS) are preserved when the agent is added.
	// +optional
	Registry RegistrySpec `json:"registry,omitempty"`

	// Canary controls the one-pod-first rollout and its automatic revert.
	// +optional
	Canary CanarySpec `json:"canary,omitempty"`
}

// ExporterSpec is the OTLP exporter configuration.
type ExporterSpec struct {
	// Endpoint is the OTLP endpoint, e.g. http://otel-agent.observability:4318.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^(https?|grpc)://.*$`
	Endpoint string `json:"endpoint"`
	// Protocol is grpc or http/protobuf (default http/protobuf).
	// +optional
	// +kubebuilder:validation:Enum=grpc;http/protobuf
	Protocol string `json:"protocol,omitempty"`
}

// SamplerSpec configures the SDK sampler.
type SamplerSpec struct {
	// Type is an OTEL_TRACES_SAMPLER value (parentbased_always_on, parentbased_traceidratio, ...).
	// +kubebuilder:validation:Required
	Type string `json:"type"`
	// Argument is OTEL_TRACES_SAMPLER_ARG, e.g. "0.25".
	// +optional
	Argument string `json:"argument,omitempty"`
}

// JavaSpec configures Java agent injection.
type JavaSpec struct {
	// Image carries the OpenTelemetry Java agent at /javaagent.jar. Only
	// OpenTelemetry agents (upstream and OTel-based distributions) are
	// supported. Default: the upstream autoinstrumentation-java image.
	// +optional
	Image string `json:"image,omitempty"`
	// Resources for the init container that copies the agent.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
	// ExtraOptions are JVM flags appended after the -javaagent flag, for agent
	// settings that have no environment variable form.
	// +optional
	ExtraOptions string `json:"extraOptions,omitempty"`
}

// RegistrySpec controls image configuration reads.
type RegistrySpec struct {
	// ReadImageConfig allows the operator to fetch the image manifest and
	// config from the registry (one small GET per image, cached) so a
	// JAVA_TOOL_OPTIONS baked into the image is merged, not shadowed. Off by
	// default: the operator then contacts no registry and says so on the pod.
	// +optional
	ReadImageConfig bool `json:"readImageConfig,omitempty"`
	// CredentialsSecret names a kubernetes.io/dockerconfigjson Secret in the
	// Instrumentation's namespace used to read private images. The operator
	// needs get access to exactly this Secret (Helm value
	// instrumentation.registryCredentialSecrets).
	// +optional
	CredentialsSecret string `json:"credentialsSecret,omitempty"`
}

// CanarySpec controls canary rollouts.
type CanarySpec struct {
	// MaxRestarts is how many container restarts an instrumented canary pod
	// may accumulate before the workload revision is reverted (default 3).
	// +optional
	// +kubebuilder:validation:Minimum=1
	MaxRestarts *int32 `json:"maxRestarts,omitempty"`
	// AutoRevert deletes a failing canary pod so its replacement starts
	// without the agent (default true). With false the revert is recorded in
	// status only and the pod is left alone.
	// +optional
	AutoRevert *bool `json:"autoRevert,omitempty"`
}

// InstrumentationStatus is the observed state of an Instrumentation profile.
type InstrumentationStatus struct {
	// PodsInstrumented counts pods in the cluster carrying this profile's agent.
	// +optional
	PodsInstrumented int32 `json:"podsInstrumented,omitempty"`
	// Workloads lists workloads whose pods use this profile, with their
	// rollout state. Reverted entries stop injection for that revision.
	// +optional
	Workloads []InstrumentedWorkload `json:"workloads,omitempty"`
	// Conditions: Ready.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// ObservedGeneration is the spec generation the status reflects.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// InstrumentedWorkload is one workload revision using a profile.
type InstrumentedWorkload struct {
	Namespace string `json:"namespace"`
	// Kind and Name of the owning workload (Deployment, StatefulSet, DaemonSet).
	Kind string `json:"kind"`
	Name string `json:"name"`
	// Revision is the pod template hash (Deployment) or controller revision
	// (StatefulSet, DaemonSet) the pods belong to.
	Revision string `json:"revision,omitempty"`
	// Mode is canary or all.
	Mode string `json:"mode"`
	// State is canary, rolled-out or reverted.
	State string `json:"state"`
	// Pods counts running instrumented pods of this revision.
	Pods int32 `json:"pods"`
	// Reason explains a revert.
	// +optional
	Reason string `json:"reason,omitempty"`
	// LastTransition is when the state last changed.
	// +optional
	LastTransition *metav1.Time `json:"lastTransition,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=inst
// +kubebuilder:printcolumn:name="Endpoint",type=string,JSONPath=`.spec.exporter.endpoint`
// +kubebuilder:printcolumn:name="Pods",type=integer,JSONPath=`.status.podsInstrumented`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Instrumentation is the Schema for the instrumentations API.
type Instrumentation struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   InstrumentationSpec   `json:"spec,omitempty"`
	Status InstrumentationStatus `json:"status,omitempty"`
}

// DeepCopyInto copies this Instrumentation into another.
func (in *Instrumentation) DeepCopyInto(out *Instrumentation) {
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Spec = in.Spec
	if in.Spec.Propagators != nil {
		out.Spec.Propagators = append([]string(nil), in.Spec.Propagators...)
	}
	if in.Spec.Sampler != nil {
		v := *in.Spec.Sampler
		out.Spec.Sampler = &v
	}
	if in.Spec.ResourceAttributes != nil {
		out.Spec.ResourceAttributes = make(map[string]string, len(in.Spec.ResourceAttributes))
		for k, v := range in.Spec.ResourceAttributes {
			out.Spec.ResourceAttributes[k] = v
		}
	}
	if in.Spec.Env != nil {
		out.Spec.Env = make([]corev1.EnvVar, len(in.Spec.Env))
		for i := range in.Spec.Env {
			in.Spec.Env[i].DeepCopyInto(&out.Spec.Env[i])
		}
	}
	in.Spec.Java.Resources.DeepCopyInto(&out.Spec.Java.Resources)
	if in.Spec.Canary.MaxRestarts != nil {
		v := *in.Spec.Canary.MaxRestarts
		out.Spec.Canary.MaxRestarts = &v
	}
	if in.Spec.Canary.AutoRevert != nil {
		v := *in.Spec.Canary.AutoRevert
		out.Spec.Canary.AutoRevert = &v
	}
	out.Status = in.Status
	if in.Status.Workloads != nil {
		out.Status.Workloads = make([]InstrumentedWorkload, len(in.Status.Workloads))
		for i, w := range in.Status.Workloads {
			if w.LastTransition != nil {
				w.LastTransition = w.LastTransition.DeepCopy()
			}
			out.Status.Workloads[i] = w
		}
	}
	if in.Status.Conditions != nil {
		out.Status.Conditions = append([]metav1.Condition(nil), in.Status.Conditions...)
	}
}

// DeepCopy returns a copy of this Instrumentation.
func (in *Instrumentation) DeepCopy() *Instrumentation {
	if in == nil {
		return nil
	}
	out := new(Instrumentation)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyObject implements runtime.Object.
func (in *Instrumentation) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

// InstrumentationList contains a list of Instrumentation.
type InstrumentationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Instrumentation `json:"items"`
}

// DeepCopyInto copies this InstrumentationList into another.
func (in *InstrumentationList) DeepCopyInto(out *InstrumentationList) {
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]Instrumentation, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

// DeepCopy returns a copy of this InstrumentationList.
func (in *InstrumentationList) DeepCopy() *InstrumentationList {
	if in == nil {
		return nil
	}
	out := new(InstrumentationList)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyObject implements runtime.Object.
func (in *InstrumentationList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

// MaxRestartsOrDefault returns the canary restart threshold.
func (in *Instrumentation) MaxRestartsOrDefault() int32 {
	if in.Spec.Canary.MaxRestarts != nil && *in.Spec.Canary.MaxRestarts > 0 {
		return *in.Spec.Canary.MaxRestarts
	}
	return 3
}

// AutoRevertEnabled reports whether failing canary pods are deleted.
func (in *Instrumentation) AutoRevertEnabled() bool {
	return in.Spec.Canary.AutoRevert == nil || *in.Spec.Canary.AutoRevert
}

// Reverted reports whether a workload revision was reverted under this profile.
func (in *Instrumentation) Reverted(namespace, kind, name, revision string) (InstrumentedWorkload, bool) {
	for _, w := range in.Status.Workloads {
		if w.State == WorkloadStateReverted && w.Namespace == namespace && w.Kind == kind && w.Name == name && w.Revision == revision {
			return w, true
		}
	}
	return InstrumentedWorkload{}, false
}
