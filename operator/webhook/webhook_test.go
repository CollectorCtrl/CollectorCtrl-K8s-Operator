// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 CollectorCtrl

package webhook

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	admissionreq "k8s.io/api/admission/v1"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/collectorctrl/collectorctrl/internal/instrument"
	v1 "github.com/collectorctrl/collectorctrl/operator/api/v1alpha1"
)

func scheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	_ = admissionv1.AddToScheme(s)
	_ = v1.AddToScheme(s)
	return s
}

func injector(t *testing.T, objects ...client.Object) *PodInjector {
	t.Helper()
	s := scheme()
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objects...).Build()
	return &PodInjector{Client: c, APIReader: c, OperatorNamespace: "collectorctrl-operator", OwnWebhookName: "collectorctrl-operator-java", Decoder: admission.NewDecoder(s)}
}

func request(t *testing.T, pod *corev1.Pod) admission.Request {
	t.Helper()
	raw, err := json.Marshal(pod)
	if err != nil {
		t.Fatal(err)
	}
	return admission.Request{AdmissionRequest: admissionreq.AdmissionRequest{
		UID: "1", Namespace: "shop", Operation: admissionreq.Create,
		Kind:   metav1.GroupVersionKind{Version: "v1", Kind: "Pod"},
		Object: runtime.RawExtension{Raw: raw},
	}}
}

func optedInPod(label string) *corev1.Pod {
	ctrl := true
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{GenerateName: "orders-abc12-", Namespace: "shop",
		Labels:          map[string]string{v1.LabelInstrumentJava: label, "pod-template-hash": "abc12"},
		OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "orders-abc12", UID: "rs-1", Controller: &ctrl}}},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "acme/orders:1", Command: []string{"java", "-jar", "app.jar"}}}}}
}

func defaultProfile() *v1.Instrumentation {
	return &v1.Instrumentation{ObjectMeta: metav1.ObjectMeta{Name: DefaultProfileName, Namespace: "collectorctrl-operator"},
		Spec: v1.InstrumentationSpec{Exporter: v1.ExporterSpec{Endpoint: "http://gw:4318"}}}
}

func patchedPod(t *testing.T, resp admission.Response, original *corev1.Pod) *corev1.Pod {
	t.Helper()
	if !resp.Allowed {
		t.Fatalf("denied: %+v", resp.Result)
	}
	// Apply the JSON patch by re-running the same mutation path is circular;
	// instead decode the patch operations and check the essentials.
	out := original.DeepCopy()
	for _, op := range resp.Patches {
		switch {
		case op.Path == "/spec/initContainers" && op.Operation == "add":
			raw, _ := json.Marshal(op.Value)
			_ = json.Unmarshal(raw, &out.Spec.InitContainers)
		case op.Path == "/spec/containers/0/env" && op.Operation == "add":
			raw, _ := json.Marshal(op.Value)
			_ = json.Unmarshal(raw, &out.Spec.Containers[0].Env)
		case strings.HasPrefix(op.Path, "/metadata/annotations"):
			if out.Annotations == nil {
				out.Annotations = map[string]string{}
			}
			if op.Path == "/metadata/annotations" {
				raw, _ := json.Marshal(op.Value)
				_ = json.Unmarshal(raw, &out.Annotations)
			} else {
				key := strings.ReplaceAll(strings.TrimPrefix(op.Path, "/metadata/annotations/"), "~1", "/")
				out.Annotations[key], _ = op.Value.(string)
			}
		case strings.HasPrefix(op.Path, "/metadata/labels/"):
			key := strings.ReplaceAll(strings.TrimPrefix(op.Path, "/metadata/labels/"), "~1", "/")
			out.Labels[key], _ = op.Value.(string)
		}
	}
	return out
}

func TestWebhookInjectsWithDefaultProfile(t *testing.T) {
	h := injector(t, defaultProfile(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "shop"}})
	pod := optedInPod("true")
	resp := h.Handle(context.Background(), request(t, pod))
	out := patchedPod(t, resp, pod)
	if len(out.Spec.InitContainers) != 1 || out.Spec.InitContainers[0].Name != instrument.InitContainerName {
		t.Fatalf("init containers: %+v", out.Spec.InitContainers)
	}
	if out.Annotations[v1.AnnotationInstrumentationStatus] != "injected" || out.Annotations[v1.AnnotationInstrumentation] != "collectorctrl-operator/default" {
		t.Fatalf("annotations: %v", out.Annotations)
	}
	if out.Labels[v1.LabelInstrumented] != "java" {
		t.Fatalf("labels: %v", out.Labels)
	}
	found := false
	for _, e := range out.Spec.Containers[0].Env {
		if e.Name == "JAVA_TOOL_OPTIONS" && strings.Contains(e.Value, instrument.AgentJar) {
			found = true
		}
	}
	if !found {
		t.Fatalf("env: %+v", out.Spec.Containers[0].Env)
	}
}

func TestWebhookPrefersNamespaceProfileAndAnnotation(t *testing.T) {
	nsProfile := &v1.Instrumentation{ObjectMeta: metav1.ObjectMeta{Name: "shop-tracing", Namespace: "shop"}, Spec: v1.InstrumentationSpec{Exporter: v1.ExporterSpec{Endpoint: "http://shop-gw:4318"}}}
	named := &v1.Instrumentation{ObjectMeta: metav1.ObjectMeta{Name: "debug", Namespace: "observability"}, Spec: v1.InstrumentationSpec{Exporter: v1.ExporterSpec{Endpoint: "http://debug:4318"}}}
	h := injector(t, defaultProfile(), nsProfile, named)
	pod := optedInPod("true")
	out := patchedPod(t, h.Handle(context.Background(), request(t, pod)), pod)
	if out.Annotations[v1.AnnotationInstrumentation] != "shop/shop-tracing" {
		t.Fatalf("profile: %v", out.Annotations[v1.AnnotationInstrumentation])
	}
	pod = optedInPod("true")
	pod.Annotations = map[string]string{v1.AnnotationInstrumentation: "observability/debug"}
	out = patchedPod(t, h.Handle(context.Background(), request(t, pod)), pod)
	if out.Annotations[v1.AnnotationInstrumentation] != "observability/debug" {
		t.Fatalf("profile: %v", out.Annotations[v1.AnnotationInstrumentation])
	}
	for _, e := range out.Spec.Containers[0].Env {
		if e.Name == "OTEL_EXPORTER_OTLP_ENDPOINT" && e.Value != "http://debug:4318" {
			t.Fatalf("endpoint %s", e.Value)
		}
	}
}

func TestWebhookRecordsSkipWithoutProfileAndNeverDenies(t *testing.T) {
	h := injector(t)
	pod := optedInPod("true")
	resp := h.Handle(context.Background(), request(t, pod))
	out := patchedPod(t, resp, pod)
	if out.Annotations[v1.AnnotationInstrumentationStatus] != "skipped" || !strings.HasPrefix(out.Annotations[v1.AnnotationInstrumentationReason], instrument.CodeNoProfile) {
		t.Fatalf("annotations: %v", out.Annotations)
	}
	if len(out.Spec.InitContainers) != 0 {
		t.Fatal("skipped pod was changed")
	}
	// Not opted in: untouched.
	plain := optedInPod("")
	resp = h.Handle(context.Background(), request(t, plain))
	if !resp.Allowed || len(resp.Patches) != 0 {
		t.Fatalf("not opted in: %+v", resp)
	}
	// Garbage: an error response, which failurePolicy Ignore turns into "allowed".
	resp = h.Handle(context.Background(), admission.Request{AdmissionRequest: admissionreq.AdmissionRequest{Object: runtime.RawExtension{Raw: []byte("{")}}})
	if resp.Allowed {
		t.Fatal("garbage should be an error response")
	}
}

func TestWebhookCanaryIsOnePodPerRevision(t *testing.T) {
	ctrl := true
	existing := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "orders-abc12-x", Namespace: "shop",
		Labels:          map[string]string{v1.LabelInstrumented: "java", v1.LabelInstrumentJava: "canary"},
		Annotations:     map[string]string{v1.AnnotationInstrumentationRevision: "abc12"},
		OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "orders-abc12", UID: "rs-1", Controller: &ctrl}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	h := injector(t, defaultProfile(), existing)
	pod := optedInPod("canary")
	out := patchedPod(t, h.Handle(context.Background(), request(t, pod)), pod)
	if out.Annotations[v1.AnnotationInstrumentationStatus] != "skipped" || !strings.HasPrefix(out.Annotations[v1.AnnotationInstrumentationReason], instrument.CodeCanaryPresent) {
		t.Fatalf("second canary: %v", out.Annotations)
	}
	// A new revision gets its own canary.
	pod = optedInPod("canary")
	pod.Labels["pod-template-hash"] = "def34"
	pod.OwnerReferences[0].Name, pod.OwnerReferences[0].UID = "orders-def34", "rs-2"
	out = patchedPod(t, h.Handle(context.Background(), request(t, pod)), pod)
	if out.Annotations[v1.AnnotationInstrumentationStatus] != "injected" || out.Annotations[v1.AnnotationInstrumentationMode] != "canary" {
		t.Fatalf("new revision canary: %v", out.Annotations)
	}
}

func TestWebhookSkipsWhenAnotherInjectorSelectsThePod(t *testing.T) {
	ignore := admissionv1.Ignore
	own := &admissionv1.MutatingWebhookConfiguration{ObjectMeta: metav1.ObjectMeta{Name: "collectorctrl-operator-java"}, Webhooks: []admissionv1.MutatingWebhook{{
		Name: "java.instrumentation.collectorctrl.io", FailurePolicy: &ignore,
		Rules: []admissionv1.RuleWithOperations{{Operations: []admissionv1.OperationType{admissionv1.Create}, Rule: admissionv1.Rule{Resources: []string{"pods"}}}}}}}
	dynatrace := &admissionv1.MutatingWebhookConfiguration{ObjectMeta: metav1.ObjectMeta{Name: "dynatrace-webhook"}, Webhooks: []admissionv1.MutatingWebhook{{
		Name:  "webhook.pod.dynatrace.com",
		Rules: []admissionv1.RuleWithOperations{{Operations: []admissionv1.OperationType{admissionv1.Create}, Rule: admissionv1.Rule{Resources: []string{"pods"}}}}}}}
	h := injector(t, defaultProfile(), own, dynatrace)
	pod := optedInPod("true")
	out := patchedPod(t, h.Handle(context.Background(), request(t, pod)), pod)
	if !strings.HasPrefix(out.Annotations[v1.AnnotationInstrumentationReason], instrument.CodeInjectorPresent) || !strings.Contains(out.Annotations[v1.AnnotationInstrumentationReason], "dynatrace-webhook") {
		t.Fatalf("reason: %v", out.Annotations[v1.AnnotationInstrumentationReason])
	}
	// With only our own webhook present, injection proceeds.
	h = injector(t, defaultProfile(), own)
	pod = optedInPod("true")
	out = patchedPod(t, h.Handle(context.Background(), request(t, pod)), pod)
	if out.Annotations[v1.AnnotationInstrumentationStatus] != "injected" {
		t.Fatalf("own webhook counted as foreign: %v", out.Annotations)
	}
}

func TestGenerateCertificatesAndPublishCABundle(t *testing.T) {
	dir := t.TempDir()
	names := ServiceDNSNames("collectorctrl-operator-webhook", "collectorctrl-operator")
	ca, err := GenerateCertificates(dir, names, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := tls.LoadX509KeyPair(filepath.Join(dir, CertFile), filepath.Join(dir, KeyFile))
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		t.Fatal("CA bundle is not PEM")
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{DNSName: "collectorctrl-operator-webhook.collectorctrl-operator.svc", Roots: pool}); err != nil {
		t.Fatalf("serving certificate does not verify for the service name: %v", err)
	}

	cfg := &admissionv1.MutatingWebhookConfiguration{ObjectMeta: metav1.ObjectMeta{Name: "collectorctrl-operator-java"},
		Webhooks: []admissionv1.MutatingWebhook{{Name: "java.instrumentation.collectorctrl.io"}}}
	c := fake.NewClientBuilder().WithScheme(scheme()).WithObjects(cfg).Build()
	p := &CABundlePublisher{Client: c, Name: cfg.Name, CABundle: ca}
	changed, err := p.Publish(context.Background())
	if err != nil || !changed {
		t.Fatalf("publish: %v %v", changed, err)
	}
	changed, err = p.Publish(context.Background())
	if err != nil || changed {
		t.Fatalf("second publish should be a no-op: %v %v", changed, err)
	}
	got := &admissionv1.MutatingWebhookConfiguration{}
	_ = c.Get(context.Background(), client.ObjectKey{Name: cfg.Name}, got)
	if string(got.Webhooks[0].ClientConfig.CABundle) != string(ca) {
		t.Fatal("CA bundle not written")
	}
	p.Name = "missing"
	if _, err := p.Publish(context.Background()); err == nil {
		t.Fatal("missing configuration should error (and be retried)")
	}
}
