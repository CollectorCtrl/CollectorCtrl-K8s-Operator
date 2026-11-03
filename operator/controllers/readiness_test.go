// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 CollectorCtrl

package controllers

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/collectorctrl/collectorctrl/internal/readiness"
	v1 "github.com/collectorctrl/collectorctrl/operator/api/v1alpha1"
	"github.com/collectorctrl/collectorctrl/pkg/opamp"
)

func readinessFixture(t *testing.T, withWebhooks bool, objects ...client.Object) *ClusterObserver {
	t.Helper()
	s := runtime.NewScheme()
	_ = appsv1.AddToScheme(s)
	_ = corev1.AddToScheme(s)
	_ = v1.AddToScheme(s)
	if withWebhooks {
		_ = admissionv1.AddToScheme(s)
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objects...).Build()
	r := &CollectorMonitorReconciler{Client: c, Scheme: s, ClusterID: "cluster-1"}
	return &ClusterObserver{Monitors: r, ClusterName: "prod-eu", APIReader: c, ReadinessExclude: DefaultReadinessExclusions}
}

func appDeployment(ns, name string, c corev1.Container) *appsv1.Deployment {
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, UID: types.UID("uid-" + name),
		Labels: map[string]string{"app.kubernetes.io/instance": "shop"}}}
	d.Spec.Template.Labels = map[string]string{"app": name}
	d.Spec.Template.Spec.Containers = []corev1.Container{c}
	return d
}

func TestReadinessReport(t *testing.T) {
	ctrl := true
	orders := appDeployment("shop", "orders", corev1.Container{Name: "app", Image: "acme/orders:2", Command: []string{"java", "-jar", "app.jar"}})
	orders.Spec.Template.Annotations = map[string]string{"instrumentation.opentelemetry.io/inject-java": "true"}
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "orders-7d", Namespace: "shop", UID: "rs-orders",
		OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: "orders", UID: orders.UID, Controller: &ctrl}}}}
	oldPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "orders-7d-old", Namespace: "shop", CreationTimestamp: metav1.NewTime(time.Now().Add(-time.Hour)),
		OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "orders-7d", UID: rs.UID, Controller: &ctrl}}},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	newPod := oldPod.DeepCopy()
	newPod.Name, newPod.CreationTimestamp = "orders-7d-new", metav1.NewTime(time.Now())
	newPod.Spec.InitContainers = []corev1.Container{{Name: "opentelemetry-auto-instrumentation-java"}}
	newPod.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "JAVA_TOOL_OPTIONS", Value: " -javaagent:/otel-auto-instrumentation-java/javaagent.jar"}}

	web := appDeployment("shop", "web", corev1.Container{Name: "app", Image: "node:20", Command: []string{"node", "server.js"},
		EnvFrom: []corev1.EnvFromSource{{ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "web-env"}}}}})
	webEnv := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "web-env", Namespace: "shop"}, Data: map[string]string{"NODE_OPTIONS": "--max-old-space-size=512"}}
	collector := appDeployment("shop", "gateway", corev1.Container{Name: "otel", Image: "otel/opentelemetry-collector-contrib:0.112.0"})
	system := appDeployment("kube-system", "coredns", corev1.Container{Name: "coredns", Image: "registry.k8s.io/coredns/coredns:v1.11.1"})
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "shop", Labels: map[string]string{"kubernetes.io/metadata.name": "shop"}}}
	hook := &admissionv1.MutatingWebhookConfiguration{ObjectMeta: metav1.ObjectMeta{Name: "opentelemetry-operator-mutation"}, Webhooks: []admissionv1.MutatingWebhook{{
		Name: "mpod.kb.io", SideEffects: nil, AdmissionReviewVersions: []string{"v1"},
		Rules: []admissionv1.RuleWithOperations{{Operations: []admissionv1.OperationType{admissionv1.Create}, Rule: admissionv1.Rule{Resources: []string{"pods"}}}},
	}}}

	o := readinessFixture(t, true, orders, rs, oldPod, newPod, web, webEnv, collector, system, ns, hook)
	rep, err := o.Readiness(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !rep.InjectorsChecked || len(rep.Injectors) != 1 || rep.RegistryChecked || rep.ClusterName != "prod-eu" {
		t.Fatalf("header %+v", rep)
	}
	if len(rep.Workloads) != 2 {
		t.Fatalf("want orders and web only (collector and kube-system skipped), got %+v", rep.Workloads)
	}
	o1, w1 := rep.Workloads[0], rep.Workloads[1]
	if o1.Name != "orders" || o1.Verdict != readiness.VerdictInstrumented || o1.Pod != "orders-7d-new" || o1.Stack != "shop" {
		t.Errorf("orders %+v", o1)
	}
	if w1.Name != "web" || w1.Verdict != readiness.VerdictReady || w1.Pod != "" {
		t.Errorf("web %+v", w1)
	}
	if rep.Summary[readiness.VerdictInstrumented] != 1 || rep.Summary[readiness.VerdictReady] != 1 {
		t.Errorf("summary %v", rep.Summary)
	}
	raw, _ := json.Marshal(rep)
	if strings.Contains(string(raw), "max-old-space") {
		t.Errorf("launcher value leaked: %s", raw)
	}
}

func TestReadinessWithoutWebhookAccess(t *testing.T) {
	app := appDeployment("shop", "web", corev1.Container{Name: "app", Image: "node:20", Command: []string{"node", "server.js"}})
	o := readinessFixture(t, false, app)
	rep, err := o.Readiness(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rep.InjectorsChecked || len(rep.Workloads) != 1 {
		t.Fatalf("got %+v", rep)
	}
	found := false
	for _, f := range rep.Workloads[0].Containers[0].Findings {
		found = found || f.Code == readiness.CodeInjectorsNotChecked
	}
	if !found {
		t.Errorf("reasons %v", rep.Workloads[0].Containers[0].Reasons)
	}
}

func TestReadinessReportsInstrumentationState(t *testing.T) {
	ctrl := true
	optIn := func(name, mode string) *appsv1.Deployment {
		d := appDeployment("shop", name, corev1.Container{Name: "app", Image: "acme/" + name + ":1", Command: []string{"java", "-jar", "app.jar"}})
		d.Spec.Template.Labels[v1.LabelInstrumentJava] = mode
		return d
	}
	podOf := func(d *appsv1.Deployment, name string, instrumented bool, ann map[string]string) []client.Object {
		rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: d.Name + "-abc12", Namespace: "shop", UID: types.UID("rs-" + d.Name),
			OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: d.Name, UID: d.UID, Controller: &ctrl}}}}
		p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "shop", Labels: map[string]string{"pod-template-hash": "abc12"}, Annotations: ann,
			OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: rs.Name, UID: rs.UID, Controller: &ctrl}}},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
		if instrumented {
			p.Labels[v1.LabelInstrumented] = "java"
			p.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "JAVA_TOOL_OPTIONS", Value: "-javaagent:/collectorctrl/java/opentelemetry-javaagent.jar"}}
		}
		return []client.Object{rs, p}
	}
	rolled := optIn("orders", "true")
	canary := optIn("payments", "canary")
	skipped := optIn("legacy", "true")
	pending := optIn("fresh", "true")
	reverted := optIn("flaky", "canary")
	plain := appDeployment("shop", "web", corev1.Container{Name: "app", Image: "node:20", Command: []string{"node", "s.js"}})
	profile := &v1.Instrumentation{ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "collectorctrl-operator"},
		Spec:   v1.InstrumentationSpec{Exporter: v1.ExporterSpec{Endpoint: "http://gw:4318"}},
		Status: v1.InstrumentationStatus{Workloads: []v1.InstrumentedWorkload{{Namespace: "shop", Kind: "Deployment", Name: "flaky", Revision: "abc12", Mode: "canary", State: v1.WorkloadStateReverted, Reason: "canary pod flaky-abc12-q restarted 3 times with the agent"}}}}
	objects := []client.Object{rolled, canary, skipped, pending, reverted, plain, profile}
	objects = append(objects, podOf(rolled, "orders-abc12-a", true, map[string]string{v1.AnnotationInstrumentation: "collectorctrl-operator/default", v1.AnnotationInstrumentationMode: "all"})...)
	objects = append(objects, podOf(canary, "payments-abc12-a", true, map[string]string{v1.AnnotationInstrumentation: "collectorctrl-operator/default", v1.AnnotationInstrumentationMode: "canary"})...)
	objects = append(objects, podOf(skipped, "legacy-abc12-a", false, map[string]string{v1.AnnotationInstrumentationStatus: "skipped", v1.AnnotationInstrumentationReason: "agent_present: container app already loads a datadog java agent"})...)
	objects = append(objects, podOf(reverted, "flaky-abc12-r", false, map[string]string{v1.AnnotationInstrumentationStatus: "skipped", v1.AnnotationInstrumentationReason: "reverted: ..."})...)
	o := readinessFixture(t, false, objects...)
	o.InstrumentationEnabled = true
	rep, err := o.Readiness(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]*opamp.InstrumentationState{}
	for _, w := range rep.Workloads {
		byName[w.Name] = w.Instrumentation
	}
	if byName["web"] != nil {
		t.Errorf("web has no opt-in but state %+v", byName["web"])
	}
	check := func(name, state string, pods int, reasonFragment string) {
		st := byName[name]
		if st == nil {
			t.Fatalf("%s: no instrumentation state", name)
		}
		if st.State != state || st.Pods != pods || !strings.Contains(st.Reason, reasonFragment) {
			t.Errorf("%s: %+v", name, st)
		}
	}
	check("orders", "rolled-out", 1, "")
	check("payments", "canary", 1, "")
	check("legacy", "skipped", 0, "agent_present")
	check("fresh", "pending", 0, "")
	check("flaky", "reverted", 0, "restarted 3 times")
	if byName["orders"].Profile != "collectorctrl-operator/default" || byName["orders"].Requested != "all" || byName["payments"].Requested != "canary" {
		t.Errorf("profile/requested: %+v %+v", byName["orders"], byName["payments"])
	}
	// Serialises under the documented key.
	raw, _ := json.Marshal(rep.Workloads[0])
	if !strings.Contains(string(raw), `"instrumentation":{`) {
		t.Errorf("payload: %s", raw)
	}
}
