// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 CollectorCtrl

package instrument

import (
	"errors"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/collectorctrl/collectorctrl/internal/readiness"
	v1 "github.com/collectorctrl/collectorctrl/operator/api/v1alpha1"
)

func profile() *v1.Instrumentation {
	return &v1.Instrumentation{ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "collectorctrl-operator"},
		Spec: v1.InstrumentationSpec{Exporter: v1.ExporterSpec{Endpoint: "http://otel-agent.observability:4318"},
			Propagators: []string{"tracecontext", "baggage"}, Sampler: &v1.SamplerSpec{Type: "parentbased_traceidratio", Argument: "0.5"},
			ResourceAttributes: map[string]string{"deployment.environment": "prod"}, Env: []corev1.EnvVar{{Name: "OTEL_LOGS_EXPORTER", Value: "none"}}}}
}

func javaPod(env ...corev1.EnvVar) *corev1.Pod {
	ctrl := true
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "orders-7d9f8-", Labels: map[string]string{v1.LabelInstrumentJava: "true", "pod-template-hash": "7d9f8", "app": "orders"},
			OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "orders-7d9f8", UID: "rs-1", Controller: &ctrl}}},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "acme/orders:2", Command: []string{"java", "-jar", "/app.jar"}, Env: env,
			SecurityContext: &corev1.SecurityContext{RunAsNonRoot: ptr(true)}}}},
	}
}

func ptr[T any](v T) *T { return &v }

func input(pod *corev1.Pod) Input {
	return Input{Pod: pod, Namespace: "shop", Workload: OwnerOf(pod), Profile: profile(), InjectorsChecked: true}
}

func envValue(c corev1.Container, name string) string {
	for _, e := range c.Env {
		if e.Name == name {
			return e.Value
		}
	}
	return ""
}

func TestOwnerOf(t *testing.T) {
	w := OwnerOf(javaPod())
	if w.Kind != "Deployment" || w.Name != "orders" || w.Revision != "7d9f8" || w.OwnerUID != "rs-1" {
		t.Fatalf("%+v", w)
	}
	ctrl := true
	sts := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "db-0", Labels: map[string]string{"controller-revision-hash": "db-abc"},
		OwnerReferences: []metav1.OwnerReference{{Kind: "StatefulSet", Name: "db", UID: "s", Controller: &ctrl}}}}
	if w := OwnerOf(sts); w.Kind != "StatefulSet" || w.Name != "db" || w.Revision != "db-abc" {
		t.Fatalf("%+v", w)
	}
	if w := OwnerOf(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{GenerateName: "one-off-"}}); w.Kind != "Pod" || w.Name != "one-off" {
		t.Fatalf("%+v", w)
	}
}

func TestInjectsAgentAndKeepsFlags(t *testing.T) {
	pod := javaPod(corev1.EnvVar{Name: "JAVA_TOOL_OPTIONS", Value: "-Xmx512m"}, corev1.EnvVar{Name: "OTEL_SERVICE_NAME", Value: "orders-api"})
	in := input(pod)
	res := Decide(in)
	if !res.Injected || res.Code != CodeInjected || res.Container != "app" || res.Mode != v1.InstrumentModeAll {
		t.Fatalf("%+v", res)
	}
	Apply(pod, in, res)
	c := pod.Spec.Containers[0]
	if got := envValue(c, "JAVA_TOOL_OPTIONS"); got != "-Xmx512m -javaagent:"+AgentJar {
		t.Errorf("JAVA_TOOL_OPTIONS = %q", got)
	}
	if envValue(c, "OTEL_SERVICE_NAME") != "orders-api" {
		t.Error("user's OTEL_SERVICE_NAME was overwritten")
	}
	if envValue(c, "OTEL_EXPORTER_OTLP_ENDPOINT") != "http://otel-agent.observability:4318" || envValue(c, "OTEL_EXPORTER_OTLP_PROTOCOL") != "http/protobuf" {
		t.Errorf("exporter env: %+v", c.Env)
	}
	attrs := envValue(c, "OTEL_RESOURCE_ATTRIBUTES")
	for _, want := range []string{"k8s.namespace.name=shop", "k8s.pod.name=$(COLLECTORCTRL_POD_NAME)", "k8s.container.name=app", "k8s.deployment.name=orders", "deployment.environment=prod"} {
		if !strings.Contains(attrs, want) {
			t.Errorf("resource attributes %q lack %s", attrs, want)
		}
	}
	if envValue(c, "OTEL_PROPAGATORS") != "tracecontext,baggage" || envValue(c, "OTEL_TRACES_SAMPLER") != "parentbased_traceidratio" || envValue(c, "OTEL_TRACES_SAMPLER_ARG") != "0.5" || envValue(c, "OTEL_LOGS_EXPORTER") != "none" {
		t.Errorf("sdk env: %+v", c.Env)
	}
	if len(pod.Spec.InitContainers) != 1 || pod.Spec.InitContainers[0].Image != DefaultJavaImage || pod.Spec.InitContainers[0].SecurityContext == nil {
		t.Errorf("init containers: %+v", pod.Spec.InitContainers)
	}
	if len(pod.Spec.Volumes) != 1 || len(c.VolumeMounts) != 1 || c.VolumeMounts[0].MountPath != MountPath {
		t.Errorf("volume wiring: %+v %+v", pod.Spec.Volumes, c.VolumeMounts)
	}
	if pod.Labels[v1.LabelInstrumented] != "java" || pod.Annotations[v1.AnnotationInstrumentationStatus] != "injected" ||
		pod.Annotations[v1.AnnotationInstrumentation] != "collectorctrl-operator/default" || pod.Annotations[v1.AnnotationInstrumentationRevision] != "7d9f8" {
		t.Errorf("markers: %v %v", pod.Labels, pod.Annotations)
	}
	// Idempotent: applying twice adds nothing.
	Apply(pod, in, res)
	if len(pod.Spec.InitContainers) != 1 || len(pod.Spec.Volumes) != 1 || len(pod.Spec.Containers[0].VolumeMounts) != 1 {
		t.Error("second Apply duplicated wiring")
	}
	// The readiness analysis now sees exactly one OpenTelemetry agent.
	got := readiness.Analyze(readiness.Input{Template: corev1.PodTemplateSpec{ObjectMeta: pod.ObjectMeta, Spec: pod.Spec}})
	if len(got) != 1 || got[0].Verdict != readiness.VerdictInstrumented || got[0].Agents[0].Vendor != "opentelemetry" {
		t.Errorf("readiness after injection: %+v", got)
	}
}

func TestImageEnvIsMergedWhenAllowed(t *testing.T) {
	pod := javaPod()
	in := input(pod)
	res := Decide(in)
	if !res.Injected || !contains(res.Notes, NoteImageEnvNotChecked) {
		t.Fatalf("without registry: %+v", res)
	}
	in.ImageEnv = func(image, name string) (string, bool, error) {
		if image != "acme/orders:2" || name != "JAVA_TOOL_OPTIONS" {
			t.Errorf("lookup %s %s", image, name)
		}
		return "-XX:+UseG1GC -Xss1m", true, nil
	}
	res = Decide(in)
	if !res.Injected || !contains(res.Notes, NoteImageEnvMerged) {
		t.Fatalf("with registry: %+v", res)
	}
	Apply(pod, in, res)
	if got := envValue(pod.Spec.Containers[0], "JAVA_TOOL_OPTIONS"); got != "-XX:+UseG1GC -Xss1m -javaagent:"+AgentJar {
		t.Errorf("JAVA_TOOL_OPTIONS = %q", got)
	}
	if !strings.Contains(pod.Annotations[v1.AnnotationInstrumentationReason], NoteImageEnvMerged) {
		t.Errorf("reason = %q", pod.Annotations[v1.AnnotationInstrumentationReason])
	}
	in = input(javaPod())
	in.ImageEnv = func(string, string) (string, bool, error) { return "", false, errors.New("registry down") }
	if res = Decide(in); !res.Injected || !contains(res.Notes, NoteImageEnvUnreadable) {
		t.Fatalf("registry error: %+v", res)
	}
}

func TestConfigMapLauncherIsInlinedAndSecretRefused(t *testing.T) {
	pod := javaPod(corev1.EnvVar{Name: "JAVA_TOOL_OPTIONS", ValueFrom: &corev1.EnvVarSource{ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: "jvm"}, Key: "opts"}}})
	in := input(pod)
	if res := Decide(in); res.Injected || res.Code != CodeLauncherUnreadable {
		t.Fatalf("unreadable ConfigMap: %+v", res)
	}
	in.Lookup = func(cm, key string) (string, bool) { return "-Xmx2g", cm == "jvm" && key == "opts" }
	res := Decide(in)
	if !res.Injected || !contains(res.Notes, NoteConfigMapInlined) {
		t.Fatalf("%+v", res)
	}
	Apply(pod, in, res)
	e := pod.Spec.Containers[0].Env[0]
	if e.ValueFrom != nil || e.Value != "-Xmx2g -javaagent:"+AgentJar {
		t.Errorf("env = %+v", e)
	}

	secret := javaPod(corev1.EnvVar{Name: "JAVA_TOOL_OPTIONS", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{Key: "x"}}})
	if res := Decide(input(secret)); res.Injected || res.Code != CodeLauncherUnreadable || !strings.Contains(res.Reason, "Secret") {
		t.Fatalf("secret: %+v", res)
	}
}

func TestEnvFromSecretNeedsForce(t *testing.T) {
	pod := javaPod()
	pod.Spec.Containers[0].EnvFrom = []corev1.EnvFromSource{{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "app-secrets"}}}}
	if res := Decide(input(pod)); res.Injected || res.Code != CodeLauncherUnreadable || !strings.Contains(res.Reason, "app-secrets") {
		t.Fatalf("%+v", res)
	}
	pod.Annotations = map[string]string{AnnotationForce: "true"}
	if res := Decide(input(pod)); !res.Injected || !contains(res.Notes, NoteEnvFromSecretForce) {
		t.Fatalf("forced: %+v", res)
	}
	// envFrom ConfigMap values are read and kept.
	pod = javaPod()
	pod.Spec.Containers[0].EnvFrom = []corev1.EnvFromSource{{ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "jvm"}}}}
	in := input(pod)
	in.LookupAll = func(string) (map[string]string, bool) {
		return map[string]string{"JAVA_TOOL_OPTIONS": "-Xms256m"}, true
	}
	res := Decide(in)
	Apply(pod, in, res)
	if got := envValue(pod.Spec.Containers[0], "JAVA_TOOL_OPTIONS"); got != "-Xms256m -javaagent:"+AgentJar {
		t.Errorf("JAVA_TOOL_OPTIONS = %q", got)
	}
}

func TestNeverInjectsNextToAnotherAgentOrInjector(t *testing.T) {
	dd := javaPod(corev1.EnvVar{Name: "JAVA_TOOL_OPTIONS", Value: "-javaagent:/dd/dd-java-agent.jar"})
	if res := Decide(input(dd)); res.Injected || res.Code != CodeAgentPresent || !strings.Contains(res.Reason, "datadog") {
		t.Fatalf("datadog agent: %+v", res)
	}
	otel := javaPod(corev1.EnvVar{Name: "JAVA_TOOL_OPTIONS", Value: "-javaagent:/otel/opentelemetry-javaagent.jar"})
	if res := Decide(input(otel)); res.Injected || res.Code != CodeAgentPresent {
		t.Fatalf("existing otel agent: %+v", res)
	}
	sidecar := javaPod()
	sidecar.Spec.InitContainers = []corev1.Container{{Name: "datadog-lib-java-init", Image: "gcr.io/datadoghq/dd-lib-java-init:v1"}}
	if res := Decide(input(sidecar)); res.Injected || res.Code != CodeAgentPresent {
		t.Fatalf("init container agent: %+v", res)
	}

	pod := javaPod()
	in := input(pod)
	in.Injectors = []readiness.Injector{dynatraceInjector()}
	if res := Decide(in); res.Injected || res.Code != CodeInjectorPresent || !strings.Contains(res.Reason, "dynatrace-webhook") {
		t.Fatalf("selector injector: %+v", res)
	}
}

func dynatraceInjector() readiness.Injector {
	cfg := admissionv1.MutatingWebhookConfiguration{ObjectMeta: metav1.ObjectMeta{Name: "dynatrace-webhook"}, Webhooks: []admissionv1.MutatingWebhook{{
		Name:  "webhook.pod.dynatrace.com",
		Rules: []admissionv1.RuleWithOperations{{Operations: []admissionv1.OperationType{admissionv1.Create}, Rule: admissionv1.Rule{Resources: []string{"pods"}}}},
	}}}
	return readiness.ClassifyWebhooks([]admissionv1.MutatingWebhookConfiguration{cfg})[0]
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func TestCanaryAndRevert(t *testing.T) {
	pod := javaPod()
	pod.Labels[v1.LabelInstrumentJava] = "canary"
	in := input(pod)
	res := Decide(in)
	if !res.Injected || res.Mode != v1.InstrumentModeCanary {
		t.Fatalf("first canary: %+v", res)
	}
	Apply(pod, in, res)
	if pod.Annotations[v1.AnnotationInstrumentationMode] != "canary" {
		t.Errorf("mode annotation %v", pod.Annotations)
	}
	in.CanaryTaken = true
	if res := Decide(in); res.Injected || res.Code != CodeCanaryPresent {
		t.Fatalf("second canary: %+v", res)
	}
	// A reverted revision is skipped until a new revision appears.
	in.CanaryTaken = false
	in.Profile.Status.Workloads = []v1.InstrumentedWorkload{{Namespace: "shop", Kind: "Deployment", Name: "orders", Revision: "7d9f8",
		Mode: "canary", State: v1.WorkloadStateReverted, Reason: "canary pod restarted 3 times"}}
	res = Decide(in)
	if res.Injected || res.Code != CodeReverted || !strings.Contains(res.Reason, "restarted 3 times") {
		t.Fatalf("reverted: %+v", res)
	}
	skipped := javaPod()
	Apply(skipped, in, res)
	if skipped.Annotations[v1.AnnotationInstrumentationStatus] != "skipped" || !strings.HasPrefix(skipped.Annotations[v1.AnnotationInstrumentationReason], CodeReverted+": ") ||
		skipped.Labels[v1.LabelInstrumented] != "" || len(skipped.Spec.InitContainers) != 0 {
		t.Errorf("skipped pod was changed: %v %v", skipped.Annotations, skipped.Spec.InitContainers)
	}
	fresh := javaPod()
	fresh.Labels[v1.LabelInstrumentJava] = "canary"
	fresh.Labels["pod-template-hash"] = "new01"
	in.Pod, in.Workload = fresh, OwnerOf(fresh)
	in.Workload.Revision = "new01"
	if res := Decide(in); !res.Injected {
		t.Fatalf("new revision after revert: %+v", res)
	}
}

func TestNotRequestedOrNoProfile(t *testing.T) {
	pod := javaPod()
	pod.Labels[v1.LabelInstrumentJava] = "false"
	if res := Decide(input(pod)); res.Injected || res.Code != CodeNotRequested {
		t.Fatalf("%+v", res)
	}
	pod = javaPod()
	in := input(pod)
	in.Profile = nil
	if res := Decide(in); res.Injected || res.Code != CodeNoProfile {
		t.Fatalf("%+v", res)
	}
}

func TestContainerSelection(t *testing.T) {
	// Two containers, one Java: picked by language.
	pod := javaPod()
	pod.Spec.Containers = append(pod.Spec.Containers, corev1.Container{Name: "web", Image: "node:20", Command: []string{"node", "server.js"}})
	if res := Decide(input(pod)); !res.Injected || res.Container != "app" {
		t.Fatalf("by language: %+v", res)
	}
	// Two unknown-language containers: ambiguous until annotated.
	pod = javaPod()
	pod.Spec.Containers = []corev1.Container{{Name: "a", Image: "acme/a:1"}, {Name: "b", Image: "acme/b:1"}}
	if res := Decide(input(pod)); res.Injected || res.Code != CodeContainerAmbiguous {
		t.Fatalf("ambiguous: %+v", res)
	}
	pod.Annotations = map[string]string{v1.AnnotationInstrumentContainer: "b"}
	if res := Decide(input(pod)); !res.Injected || res.Container != "b" {
		t.Fatalf("annotated: %+v", res)
	}
	pod.Annotations[v1.AnnotationInstrumentContainer] = "zzz"
	if res := Decide(input(pod)); res.Injected || res.Code != CodeContainerNotFound {
		t.Fatalf("missing: %+v", res)
	}
	// One container of unknown language: the label says Java, so it is used and noted.
	pod = javaPod()
	pod.Spec.Containers[0].Command = nil
	res := Decide(input(pod))
	if !res.Injected || !contains(res.Notes, NoteLanguageAssumed) {
		t.Fatalf("assumed: %+v", res)
	}
	// A Node.js-only pod is never given a Java agent.
	pod = javaPod()
	pod.Spec.Containers[0].Command = []string{"node", "server.js"}
	if res := Decide(input(pod)); res.Injected || res.Code != CodeNotJava {
		t.Fatalf("node: %+v", res)
	}
	// Collector sidecar is ignored; the Java app is picked.
	pod = javaPod()
	pod.Spec.Containers = append(pod.Spec.Containers, corev1.Container{Name: "otel", Image: "otel/opentelemetry-collector-contrib:0.112.0"})
	if res := Decide(input(pod)); !res.Injected || res.Container != "app" {
		t.Fatalf("sidecar: %+v", res)
	}
}

func TestProfileRefAndMode(t *testing.T) {
	if ns, n := ProfileRef(map[string]string{v1.AnnotationInstrumentation: "obs/prod"}); ns != "obs" || n != "prod" {
		t.Error(ns, n)
	}
	if ns, n := ProfileRef(map[string]string{v1.AnnotationInstrumentation: "prod"}); ns != "" || n != "prod" {
		t.Error(ns, n)
	}
	if ModeOf(map[string]string{v1.LabelInstrumentJava: "True"}) != v1.InstrumentModeAll || ModeOf(map[string]string{v1.LabelInstrumentJava: "Canary"}) != v1.InstrumentModeCanary || ModeOf(nil) != "" {
		t.Error("mode")
	}
}

func TestExtraOptionsAndCustomImage(t *testing.T) {
	pod := javaPod()
	in := input(pod)
	in.Profile.Spec.Java.Image = "registry.corp/otel/javaagent:2.33.1"
	in.Profile.Spec.Java.ExtraOptions = "-Dotel.javaagent.debug=true"
	in.Profile.Spec.Exporter.Protocol = "grpc"
	res := Decide(in)
	Apply(pod, in, res)
	c := pod.Spec.Containers[0]
	if got := envValue(c, "JAVA_TOOL_OPTIONS"); got != "-javaagent:"+AgentJar+" -Dotel.javaagent.debug=true" {
		t.Errorf("JAVA_TOOL_OPTIONS = %q", got)
	}
	if pod.Spec.InitContainers[0].Image != "registry.corp/otel/javaagent:2.33.1" || envValue(c, "OTEL_EXPORTER_OTLP_PROTOCOL") != "grpc" {
		t.Errorf("%+v %+v", pod.Spec.InitContainers[0].Image, c.Env)
	}
}
