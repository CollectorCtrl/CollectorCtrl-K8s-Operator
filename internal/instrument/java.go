// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 CollectorCtrl

// Package instrument decides whether a new pod gets the OpenTelemetry Java
// agent and builds the mutation. It is pure: the webhook gathers the inputs
// (profile, namespace labels, injectors, ConfigMap and image lookups, whether a
// canary already runs) and applies the result.
//
// Rules, in order: the pod must ask (collectorctrl.io/instrument-java), a
// profile must exist, the workload revision must not have been reverted, no
// APM agent of any vendor may be loaded or about to be injected, exactly one
// container must be the Java application, and its JAVA_TOOL_OPTIONS must be
// something the operator can read and extend. Existing JVM flags are kept:
// from the pod spec, from a ConfigMap, or from the image configuration when
// the profile allows the registry read.
package instrument

import (
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/collectorctrl/collectorctrl/internal/readiness"
	v1 "github.com/collectorctrl/collectorctrl/operator/api/v1alpha1"
)

// DefaultJavaImage carries the upstream OpenTelemetry Java agent at /javaagent.jar.
const DefaultJavaImage = "ghcr.io/open-telemetry/opentelemetry-operator/autoinstrumentation-java:2.33.1"

// Names used in the mutated pod. The jar name contains "opentelemetry-javaagent"
// so the readiness report recognises it as an OpenTelemetry agent.
const (
	VolumeName        = "collectorctrl-java-agent"
	InitContainerName = "collectorctrl-java-agent"
	MountPath         = "/collectorctrl/java"
	AgentJar          = MountPath + "/opentelemetry-javaagent.jar"
	podNameEnv        = "COLLECTORCTRL_POD_NAME"
	// AnnotationForce lets a pod be instrumented although JAVA_TOOL_OPTIONS
	// could come from a Secret through envFrom (the explicit value then wins).
	AnnotationForce = "collectorctrl.io/instrument-force"
)

// Decision codes. Stable: the server and UIs key on them.
const (
	CodeInjected           = "injected"
	CodeNotRequested       = "not_requested"
	CodeNoProfile          = "no_profile"
	CodeReverted           = "reverted"
	CodeCanaryPresent      = "canary_present"
	CodeAgentPresent       = "agent_present"
	CodeInjectorPresent    = "injector_present"
	CodeContainerNotFound  = "container_not_found"
	CodeContainerAmbiguous = "container_ambiguous"
	CodeNotJava            = "not_java"
	CodeLauncherUnreadable = "launcher_unreadable"
)

// Notes on injected pods.
const (
	NoteImageEnvMerged     = "image_env_merged"
	NoteImageEnvNotChecked = "image_env_not_checked"
	NoteImageEnvUnreadable = "image_env_unreadable"
	NoteConfigMapInlined   = "launcher_configmap_inlined"
	NoteLanguageAssumed    = "language_assumed"
	NoteEnvFromSecretForce = "envfrom_secret_forced"
)

// Workload is the pod's controller, as far as owner references tell.
type Workload struct {
	Kind     string // Deployment, StatefulSet, DaemonSet, ReplicaSet, Job, Pod, ...
	Name     string
	OwnerUID string // UID of the direct controller (ReplicaSet for Deployments)
	Revision string // pod-template-hash or controller-revision-hash
}

// OwnerOf derives the workload from the pod's controller reference. Pods of a
// Deployment are owned by a ReplicaSet named <deployment>-<pod-template-hash>.
func OwnerOf(pod *corev1.Pod) Workload {
	owner := metav1.GetControllerOf(pod)
	if owner == nil {
		name := pod.Name
		if name == "" {
			name = strings.TrimSuffix(pod.GenerateName, "-")
		}
		return Workload{Kind: "Pod", Name: name}
	}
	w := Workload{Kind: owner.Kind, Name: owner.Name, OwnerUID: string(owner.UID)}
	switch owner.Kind {
	case "ReplicaSet":
		hash := pod.Labels["pod-template-hash"]
		w.Revision = hash
		if hash != "" && strings.HasSuffix(owner.Name, "-"+hash) {
			w.Kind, w.Name = "Deployment", strings.TrimSuffix(owner.Name, "-"+hash)
		}
	case "StatefulSet", "DaemonSet":
		w.Revision = pod.Labels["controller-revision-hash"]
	}
	return w
}

// ModeOf maps the opt-in label to a mode; "" means not requested.
func ModeOf(labels map[string]string) string {
	switch strings.ToLower(strings.TrimSpace(labels[v1.LabelInstrumentJava])) {
	case "true", "all", "yes":
		return v1.InstrumentModeAll
	case "canary":
		return v1.InstrumentModeCanary
	}
	return ""
}

// ProfileRef splits the collectorctrl.io/instrumentation annotation into
// namespace and name; the namespace is empty when only a name is given.
func ProfileRef(annotations map[string]string) (namespace, name string) {
	raw := strings.TrimSpace(annotations[v1.AnnotationInstrumentation])
	if raw == "" {
		return "", ""
	}
	if ns, n, ok := strings.Cut(raw, "/"); ok {
		return ns, n
	}
	return "", raw
}

// ImageEnvLookup returns the value of a variable in the image configuration.
// found is false when the image does not set it; err means the registry could
// not be read.
type ImageEnvLookup func(image, name string) (value string, found bool, err error)

// Input is everything Decide needs.
type Input struct {
	Pod       *corev1.Pod
	Namespace string // from the admission request; pod.Namespace may be empty
	Workload  Workload
	Profile   *v1.Instrumentation // nil when none applies
	// CanaryTaken is true when another instrumented pod of this workload
	// revision already exists.
	CanaryTaken bool

	NamespaceLabels  map[string]string
	Injectors        []readiness.Injector // other injectors; the operator's own webhook removed
	InjectorsChecked bool
	Lookup           readiness.Lookup
	LookupAll        readiness.LookupAll
	// ImageEnv is nil when the profile does not allow registry reads.
	ImageEnv ImageEnvLookup
}

// Result is the decision and what to record on the pod.
type Result struct {
	Injected  bool
	Code      string
	Reason    string
	Mode      string
	Container string
	Notes     []string
	// javaToolOptions is the merged value to set when Injected.
	javaToolOptions string
	// replaceConfigMapEnv is true when JAVA_TOOL_OPTIONS came from a ConfigMap
	// key and is inlined as a literal.
	replaceConfigMapEnv bool
}

func skip(code, reason string) Result { return Result{Code: code, Reason: reason} }

// Decide runs the rules.
func Decide(in Input) Result {
	pod := in.Pod
	mode := ModeOf(pod.Labels)
	if mode == "" {
		return skip(CodeNotRequested, "pod does not carry "+v1.LabelInstrumentJava+"=true or canary")
	}
	if in.Profile == nil {
		return skip(CodeNoProfile, "no Instrumentation profile applies: create one in the namespace, name it with "+v1.AnnotationInstrumentation+", or create a default profile in the operator namespace")
	}
	if w, ok := in.Profile.Reverted(in.Namespace, in.Workload.Kind, in.Workload.Name, in.Workload.Revision); ok {
		return skip(CodeReverted, fmt.Sprintf("revision %s was reverted: %s; roll out a new revision to try again", w.Revision, w.Reason))
	}
	if mode == v1.InstrumentModeCanary && in.CanaryTaken {
		return skip(CodeCanaryPresent, "a canary pod of this revision is already instrumented; set "+v1.LabelInstrumentJava+"=true to roll out")
	}

	tmpl := corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: pod.Labels, Annotations: pod.Annotations}, Spec: pod.Spec}
	containers := readiness.Analyze(readiness.Input{
		Template: tmpl, WorkloadAnnotations: pod.Annotations, NamespaceLabels: in.NamespaceLabels,
		Injectors: in.Injectors, InjectorsChecked: in.InjectorsChecked, Lookup: in.Lookup, LookupAll: in.LookupAll,
	})
	for _, c := range containers {
		if len(c.Agents) > 0 {
			a := c.Agents[0]
			return skip(CodeAgentPresent, fmt.Sprintf("container %s already loads a %s %s agent (%s)", c.Name, a.Vendor, a.Language, a.Evidence))
		}
		if len(c.SelectedBy) > 0 {
			return skip(CodeInjectorPresent, fmt.Sprintf("container %s is selected by injector webhook %s", c.Name, strings.Join(c.SelectedBy, ", ")))
		}
	}

	target, note, res := pickContainer(pod, containers)
	if res != nil {
		return *res
	}
	out := Result{Injected: true, Code: CodeInjected, Mode: mode, Container: target.Name}
	if note != "" {
		out.Notes = append(out.Notes, note)
	}
	base, notes, failed := baseJavaToolOptions(pod, target, in)
	if failed != nil {
		return *failed
	}
	out.Notes = append(out.Notes, notes...)
	for _, n := range notes {
		if n == NoteConfigMapInlined {
			out.replaceConfigMapEnv = true
		}
	}
	flags := "-javaagent:" + AgentJar
	if extra := strings.TrimSpace(in.Profile.Spec.Java.ExtraOptions); extra != "" {
		flags += " " + extra
	}
	if base == "" {
		out.javaToolOptions = flags
	} else {
		out.javaToolOptions = strings.TrimSpace(base) + " " + flags
	}
	out.Reason = "OpenTelemetry Java agent added to container " + target.Name
	return out
}

// pickContainer chooses the application container: the annotated one, else
// the single Java container, else the single application container.
func pickContainer(pod *corev1.Pod, analysed []readiness.Container) (corev1.Container, string, *Result) {
	byName := map[string]corev1.Container{}
	for _, c := range pod.Spec.Containers {
		byName[c.Name] = c
	}
	if want := strings.TrimSpace(pod.Annotations[v1.AnnotationInstrumentContainer]); want != "" {
		c, ok := byName[want]
		if !ok {
			r := skip(CodeContainerNotFound, fmt.Sprintf("container %q named in %s does not exist", want, v1.AnnotationInstrumentContainer))
			return corev1.Container{}, "", &r
		}
		return c, "", nil
	}
	var java, other []readiness.Container
	for _, c := range analysed {
		switch c.Language {
		case readiness.LangJava:
			java = append(java, c)
		case readiness.LangUnknown:
			other = append(other, c)
		}
	}
	switch {
	case len(java) == 1:
		return byName[java[0].Name], "", nil
	case len(java) > 1:
		r := skip(CodeContainerAmbiguous, fmt.Sprintf("%d Java containers (%s); name one with %s", len(java), names(java), v1.AnnotationInstrumentContainer))
		return corev1.Container{}, "", &r
	case len(other) == 1 && len(analysed) == 1:
		return byName[other[0].Name], NoteLanguageAssumed, nil
	case len(other) > 1:
		r := skip(CodeContainerAmbiguous, fmt.Sprintf("%d containers of unknown language (%s); name one with %s", len(other), names(other), v1.AnnotationInstrumentContainer))
		return corev1.Container{}, "", &r
	}
	langs := make([]string, 0, len(analysed))
	for _, c := range analysed {
		langs = append(langs, c.Name+"="+c.Language)
	}
	if len(analysed) == 0 {
		r := skip(CodeNotJava, "no application container (only infrastructure images)")
		return corev1.Container{}, "", &r
	}
	r := skip(CodeNotJava, "no Java container found ("+strings.Join(langs, ", ")+"); set "+v1.AnnotationInstrumentContainer+" or "+readiness.AnnotationLanguage)
	return corev1.Container{}, "", &r
}

func names(cs []readiness.Container) string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.Name)
	}
	return strings.Join(out, ", ")
}

// baseJavaToolOptions finds the JAVA_TOOL_OPTIONS the process would have had
// without the agent, so the agent is appended rather than replacing it.
func baseJavaToolOptions(pod *corev1.Pod, c corev1.Container, in Input) (string, []string, *Result) {
	var notes []string
	force := strings.EqualFold(strings.TrimSpace(pod.Annotations[AnnotationForce]), "true")
	for _, e := range c.Env {
		if e.Name != "JAVA_TOOL_OPTIONS" {
			continue
		}
		if e.ValueFrom == nil {
			return e.Value, notes, nil
		}
		switch {
		case e.ValueFrom.ConfigMapKeyRef != nil:
			ref := e.ValueFrom.ConfigMapKeyRef
			if in.Lookup != nil {
				if v, ok := in.Lookup(ref.Name, ref.Key); ok {
					return v, append(notes, NoteConfigMapInlined), nil
				}
			}
			r := skip(CodeLauncherUnreadable, fmt.Sprintf("JAVA_TOOL_OPTIONS comes from ConfigMap %s key %s, which could not be read", ref.Name, ref.Key))
			return "", nil, &r
		case e.ValueFrom.SecretKeyRef != nil:
			r := skip(CodeLauncherUnreadable, fmt.Sprintf("JAVA_TOOL_OPTIONS comes from Secret %s; the operator does not read Secrets, so it cannot merge the agent flag", e.ValueFrom.SecretKeyRef.Name))
			return "", nil, &r
		default:
			r := skip(CodeLauncherUnreadable, "JAVA_TOOL_OPTIONS comes from a field or resource reference, which cannot be merged")
			return "", nil, &r
		}
	}
	// envFrom: a ConfigMap value is readable; a Secret might set it and cannot be read.
	var secrets []string
	for _, src := range c.EnvFrom {
		if src.SecretRef != nil {
			secrets = append(secrets, src.SecretRef.Name)
		}
	}
	if len(secrets) > 0 && !force {
		r := skip(CodeLauncherUnreadable, fmt.Sprintf("envFrom Secret %s may set JAVA_TOOL_OPTIONS; the operator does not read Secrets. Set %s=true after confirming it does not", strings.Join(secrets, ", "), AnnotationForce))
		return "", nil, &r
	}
	if len(secrets) > 0 {
		notes = append(notes, NoteEnvFromSecretForce)
	}
	for _, src := range c.EnvFrom {
		if src.ConfigMapRef == nil || in.LookupAll == nil {
			continue
		}
		if data, ok := in.LookupAll(src.ConfigMapRef.Name); ok {
			if v, ok := data[src.Prefix+"JAVA_TOOL_OPTIONS"]; ok && src.Prefix == "" {
				return v, notes, nil
			}
		}
	}
	// Nothing in the pod spec: the image may still set it.
	if in.ImageEnv == nil {
		return "", append(notes, NoteImageEnvNotChecked), nil
	}
	v, found, err := in.ImageEnv(c.Image, "JAVA_TOOL_OPTIONS")
	switch {
	case err != nil:
		return "", append(notes, NoteImageEnvUnreadable), nil
	case found:
		return v, append(notes, NoteImageEnvMerged), nil
	}
	return "", notes, nil
}

// Apply records the decision on the pod and, when injected, adds the agent.
func Apply(pod *corev1.Pod, in Input, res Result) {
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	if pod.Labels == nil {
		pod.Labels = map[string]string{}
	}
	if !res.Injected {
		pod.Annotations[v1.AnnotationInstrumentationStatus] = "skipped"
		pod.Annotations[v1.AnnotationInstrumentationReason] = res.Code + ": " + res.Reason
		return
	}
	profile := in.Profile
	pod.Labels[v1.LabelInstrumented] = readiness.LangJava
	pod.Annotations[v1.AnnotationInstrumentationStatus] = "injected"
	pod.Annotations[v1.AnnotationInstrumentation] = profile.Namespace + "/" + profile.Name
	pod.Annotations[v1.AnnotationInstrumentationMode] = res.Mode
	if in.Workload.Revision != "" {
		pod.Annotations[v1.AnnotationInstrumentationRevision] = in.Workload.Revision
	}
	reason := res.Reason
	if len(res.Notes) > 0 {
		notes := append([]string(nil), res.Notes...)
		sort.Strings(notes)
		reason += " (" + strings.Join(notes, ", ") + ")"
	}
	pod.Annotations[v1.AnnotationInstrumentationReason] = reason

	// Volume and init container that copies the agent.
	hasVolume := false
	for _, v := range pod.Spec.Volumes {
		if v.Name == VolumeName {
			hasVolume = true
		}
	}
	if !hasVolume {
		limit := resource.MustParse("256Mi")
		pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{Name: VolumeName, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: &limit}}})
	}
	var target *corev1.Container
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == res.Container {
			target = &pod.Spec.Containers[i]
		}
	}
	if target == nil {
		return
	}
	image := profile.Spec.Java.Image
	if image == "" {
		image = DefaultJavaImage
	}
	init := corev1.Container{
		Name: InitContainerName, Image: image,
		Command:      []string{"cp", "/javaagent.jar", AgentJar},
		VolumeMounts: []corev1.VolumeMount{{Name: VolumeName, MountPath: MountPath}},
		Resources:    initResources(profile.Spec.Java.Resources),
	}
	if target.SecurityContext != nil {
		init.SecurityContext = target.SecurityContext.DeepCopy()
	}
	hasInit := false
	for _, ic := range pod.Spec.InitContainers {
		if ic.Name == InitContainerName {
			hasInit = true
		}
	}
	if !hasInit {
		pod.Spec.InitContainers = append(pod.Spec.InitContainers, init)
	}

	// The application container: mount, launcher flag, OTel settings.
	hasMount := false
	for _, m := range target.VolumeMounts {
		if m.Name == VolumeName {
			hasMount = true
		}
	}
	if !hasMount {
		target.VolumeMounts = append(target.VolumeMounts, corev1.VolumeMount{Name: VolumeName, MountPath: MountPath, ReadOnly: true})
	}
	set := false
	for i := range target.Env {
		if target.Env[i].Name == "JAVA_TOOL_OPTIONS" {
			target.Env[i] = corev1.EnvVar{Name: "JAVA_TOOL_OPTIONS", Value: res.javaToolOptions}
			set = true
		}
	}
	if !set {
		target.Env = append(target.Env, corev1.EnvVar{Name: "JAVA_TOOL_OPTIONS", Value: res.javaToolOptions})
	}

	defaults := otelEnv(in, res.Container)
	have := map[string]bool{}
	for _, e := range target.Env {
		have[e.Name] = true
	}
	for _, e := range defaults {
		if !have[e.Name] {
			target.Env = append(target.Env, e)
			have[e.Name] = true
		}
	}
	for _, e := range profile.Spec.Env {
		if !have[e.Name] {
			target.Env = append(target.Env, *e.DeepCopy())
			have[e.Name] = true
		}
	}
}

func initResources(r corev1.ResourceRequirements) corev1.ResourceRequirements {
	if len(r.Limits) > 0 || len(r.Requests) > 0 {
		return *r.DeepCopy()
	}
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("32Mi")},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("200m"), corev1.ResourceMemory: resource.MustParse("128Mi")},
	}
}

// otelEnv builds the SDK settings: exporter, service name, resource
// attributes (Kubernetes first, then the profile's), propagators, sampler.
func otelEnv(in Input, container string) []corev1.EnvVar {
	spec := in.Profile.Spec
	protocol := spec.Exporter.Protocol
	if protocol == "" {
		protocol = "http/protobuf"
	}
	out := []corev1.EnvVar{
		{Name: podNameEnv, ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}}},
		{Name: "OTEL_EXPORTER_OTLP_ENDPOINT", Value: spec.Exporter.Endpoint},
		{Name: "OTEL_EXPORTER_OTLP_PROTOCOL", Value: protocol},
		{Name: "OTEL_SERVICE_NAME", Value: in.Workload.Name},
	}
	attrs := []string{
		"k8s.namespace.name=" + in.Namespace,
		"k8s.pod.name=$(" + podNameEnv + ")",
		"k8s.container.name=" + container,
	}
	switch in.Workload.Kind {
	case "Deployment":
		attrs = append(attrs, "k8s.deployment.name="+in.Workload.Name)
	case "StatefulSet":
		attrs = append(attrs, "k8s.statefulset.name="+in.Workload.Name)
	case "DaemonSet":
		attrs = append(attrs, "k8s.daemonset.name="+in.Workload.Name)
	}
	keys := make([]string, 0, len(spec.ResourceAttributes))
	for k := range spec.ResourceAttributes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		attrs = append(attrs, k+"="+spec.ResourceAttributes[k])
	}
	out = append(out, corev1.EnvVar{Name: "OTEL_RESOURCE_ATTRIBUTES", Value: strings.Join(attrs, ",")})
	if len(spec.Propagators) > 0 {
		out = append(out, corev1.EnvVar{Name: "OTEL_PROPAGATORS", Value: strings.Join(spec.Propagators, ",")})
	}
	if spec.Sampler != nil && spec.Sampler.Type != "" {
		out = append(out, corev1.EnvVar{Name: "OTEL_TRACES_SAMPLER", Value: spec.Sampler.Type})
		if spec.Sampler.Argument != "" {
			out = append(out, corev1.EnvVar{Name: "OTEL_TRACES_SAMPLER_ARG", Value: spec.Sampler.Argument})
		}
	}
	return out
}
