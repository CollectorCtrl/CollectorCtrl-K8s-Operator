package controllers

import (
	"context"
	"strings"
	"testing"

	"github.com/collectorctrl/collectorctrl/internal/otelconfig"
	v1 "github.com/collectorctrl/collectorctrl/operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
)

const agentConfig = `
receivers:
  kubeletstats: {}
  otlp: {}
exporters:
  otlp:
    endpoint: gateway-svc:4317
  coralogix:
    domain: eu2.coralogix.com
service:
  pipelines:
    metrics: {receivers: [kubeletstats, otlp], exporters: [otlp, coralogix]}
`

func labelledAgent() *appsv1.DaemonSet {
	return &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: "obs", UID: "agent-uid",
			Labels: map[string]string{LabelObserve: "true", "app.kubernetes.io/instance": "coralogix-otel", "app": "agent"}},
		Spec: appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "agent"}},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "otel", VolumeMounts: []corev1.VolumeMount{{Name: "config", MountPath: "/conf"}}}},
				Volumes:    []corev1.Volume{{Name: "config", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "agent-config"}}}}},
			}}},
	}
}

func gatewayObjects() (*appsv1.Deployment, *corev1.Service) {
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "gateway", Namespace: "obs", UID: "gw-uid"},
		Spec:       appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "gw", "tier": "otel"}}}},
	}
	s := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "gateway-svc", Namespace: "obs"}, Spec: corev1.ServiceSpec{Selector: map[string]string{"app": "gw"}}}
	return d, s
}

func labelFixture(t *testing.T) (*LabelReconciler, *CollectorMonitorReconciler) {
	t.Helper()
	gw, svc := gatewayObjects()
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "agent-config", Namespace: "obs"}, Data: map[string]string{"relay.yaml": agentConfig}}
	base := fixture(t, labelledAgent(), gw, svc, cm)
	base.DefaultServer = "wss://127.0.0.1:1/v1/opamp"
	base.EnrollmentToken = "cce_dummy"
	base.ClusterID = "cluster"
	base.CredentialDirectory = t.TempDir()
	base.lifetime = context.Background()
	t.Cleanup(func() {
		for k := range base.connections {
			base.closeConnection(k)
		}
	})
	return NewLabelReconcilers(base)[0], base
}

func reconcileAgent(t *testing.T, l *LabelReconciler) {
	t.Helper()
	if _, err := l.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "obs", Name: "agent"}}); err != nil {
		t.Fatal(err)
	}
}

func TestLabelledWorkloadIsObservedWithoutACR(t *testing.T) {
	l, base := labelFixture(t)
	reconcileAgent(t, l)
	c := base.connections["obs/@label/DaemonSet/agent"]
	if c == nil {
		t.Fatalf("no connection for labelled workload: %v", base.connections)
	}
	if c.identity != "k8s://cluster/agent-uid/otel" {
		t.Fatalf("identity: %s", c.identity)
	}
}

func TestRemovingLabelStopsObservation(t *testing.T) {
	l, base := labelFixture(t)
	reconcileAgent(t, l)
	ds := &appsv1.DaemonSet{}
	_ = base.Get(context.Background(), types.NamespacedName{Namespace: "obs", Name: "agent"}, ds)
	delete(ds.Labels, LabelObserve)
	if err := base.Update(context.Background(), ds); err != nil {
		t.Fatal(err)
	}
	reconcileAgent(t, l)
	if len(base.connections) != 0 {
		t.Fatalf("connection kept after opt-out: %v", base.connections)
	}
}

func TestCollectorMonitorTakesPrecedenceOverLabel(t *testing.T) {
	l, base := labelFixture(t)
	reconcileAgent(t, l)
	m := &v1.CollectorMonitor{ObjectMeta: metav1.ObjectMeta{Name: "explicit", Namespace: "obs"},
		Spec: v1.CollectorMonitorSpec{WorkloadSelector: v1.WorkloadSelector{Kind: "DaemonSet", Name: "agent"}}}
	if err := base.Create(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	reconcileAgent(t, l)
	if _, ok := base.connections["obs/@label/DaemonSet/agent"]; ok {
		t.Fatal("label discovery kept a workload owned by a CollectorMonitor")
	}
}

func TestEnsureConnectionTakesOverLabelIdentity(t *testing.T) {
	l, base := labelFixture(t)
	reconcileAgent(t, l)
	m := &v1.CollectorMonitor{ObjectMeta: metav1.ObjectMeta{Name: "explicit", Namespace: "obs"},
		Spec: v1.CollectorMonitorSpec{WorkloadSelector: v1.WorkloadSelector{Kind: "DaemonSet", Name: "agent"}}}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "agent-config", Namespace: "obs"}}
	c, err := base.ensureConnection(context.Background(), m, labelledAgent(), "DaemonSet", "otel", cm, connectionMeta{})
	if err != nil {
		t.Fatalf("monitor could not take over label identity: %v", err)
	}
	defer c.Stop()
	if _, ok := base.connections["obs/@label/DaemonSet/agent"]; ok {
		t.Fatal("label connection not released")
	}
	// The reverse is refused: label discovery never displaces a monitor.
	label := syntheticMonitor(labelledAgent(), "DaemonSet", base.DefaultServer)
	if _, err := base.ensureConnection(context.Background(), label, labelledAgent(), "DaemonSet", "otel", cm, connectionMeta{}); err == nil || !strings.Contains(err.Error(), "already observed") {
		t.Fatalf("label path displaced a monitor: %v", err)
	}
}

func TestSyntheticMonitorAnnotations(t *testing.T) {
	ds := labelledAgent()
	ds.Annotations = map[string]string{AnnContainer: "otel", AnnConfigKey: "relay.yaml", AnnReportInterval: "45s", AnnReportConfig: "true"}
	m := syntheticMonitor(ds, "DaemonSet", "wss://x/v1/opamp")
	if m.Spec.CollectorContainer != "otel" || m.Spec.ConfigMapSelector == nil || m.Spec.ConfigMapSelector.Key != "relay.yaml" ||
		m.Spec.HealthCheck.Interval.Duration.Seconds() != 45 || !m.Spec.ReportConfig || m.Spec.WorkloadSelector.Name != "agent" {
		t.Fatalf("annotations not applied: %+v", m.Spec)
	}
	if syntheticMonitor(labelledAgent(), "DaemonSet", "").Spec.ReportConfig {
		t.Fatal("config upload must stay off by default")
	}
}

func TestAnalyseRoleStackAndTopology(t *testing.T) {
	_, base := labelFixture(t)
	ds := labelledAgent()
	in := base.analyse(context.Background(), syntheticMonitor(ds, "DaemonSet", ""), ds, "DaemonSet", agentConfig)
	if in.role.Name != otelconfig.RoleAgent || in.roleSource != "inferred" {
		t.Fatalf("role: %+v", in.role)
	}
	if in.stack != "coralogix-otel" || in.stackSource != "helm-release" {
		t.Fatalf("stack: %s %s", in.stack, in.stackSource)
	}
	got := map[string]string{}
	for _, e := range in.exports {
		got[e.Exporter] = e.Target + ":" + e.Kind + "/" + e.Name
	}
	if got["otlp"] != "workload:Deployment/gateway" {
		t.Errorf("otlp edge: %s", got["otlp"])
	}
	if got["coralogix"] != "external:/eu2.coralogix.com" {
		t.Errorf("coralogix edge: %s", got["coralogix"])
	}
}

func TestRoleAnnotationOverride(t *testing.T) {
	_, base := labelFixture(t)
	ds := labelledAgent()
	ds.Annotations = map[string]string{AnnRole: "gateway"}
	in := base.analyse(context.Background(), syntheticMonitor(ds, "DaemonSet", ""), ds, "DaemonSet", agentConfig)
	if in.role.Name != "gateway" || in.roleSource != "annotation" {
		t.Fatalf("override ignored: %+v", in)
	}
	ds.Annotations[AnnRole] = "router"
	in = base.analyse(context.Background(), syntheticMonitor(ds, "DaemonSet", ""), ds, "DaemonSet", agentConfig)
	if in.role.Name != otelconfig.RoleAgent || in.roleSource != "inferred" {
		t.Fatalf("unknown override applied: %+v", in)
	}
}

func TestUnreadableConfigIsReportedNotFatal(t *testing.T) {
	_, base := labelFixture(t)
	ds := labelledAgent()
	in := base.analyse(context.Background(), syntheticMonitor(ds, "DaemonSet", ""), ds, "DaemonSet", "pipeline: ${file:/etc/real.yaml}")
	if in.configError == "" || in.analysis != nil || in.role.Name != otelconfig.RoleUnknown {
		t.Fatalf("got %+v", in)
	}
}

func TestStackPrecedence(t *testing.T) {
	m := &v1.CollectorMonitor{}
	w := labelledAgent()
	w.Labels["argocd.argoproj.io/instance"] = "observability"
	if s, src := stackOf(m, w); s != "coralogix-otel" || src != "helm-release" {
		t.Fatalf("%s %s", s, src)
	}
	w.OwnerReferences = []metav1.OwnerReference{{Kind: "OpenTelemetryCollector", Name: "cluster-gw"}}
	if s, src := stackOf(m, w); s != "cluster-gw" || src != "otel-operator" {
		t.Fatalf("%s %s", s, src)
	}
	w.Annotations = map[string]string{AnnStack: "platform"}
	if s, src := stackOf(m, w); s != "platform" || src != "annotation" {
		t.Fatalf("%s %s", s, src)
	}
	plain := labelledAgent()
	delete(plain.Labels, "app.kubernetes.io/instance")
	if s, src := stackOf(m, plain); s != "agent" || src != "standalone" {
		t.Fatalf("%s %s", s, src)
	}
}

func TestExportResolutionEdgeCases(t *testing.T) {
	_, base := labelFixture(t)
	eps := []otelconfig.Endpoint{
		{Exporter: "a", Raw: "${env:X}", Unresolved: true},
		{Exporter: "b", Raw: "localhost:4317", Host: "localhost"},
		{Exporter: "c", Raw: "missing.other.svc:4317", Host: "missing.other.svc"},
		{Exporter: "d", Raw: "ingest.example.com", Host: "ingest.example.com"},
		{Exporter: "e", Raw: "gateway-svc.obs:4317", Host: "gateway-svc.obs"},
	}
	want := map[string]string{"a": targetUnresolved, "b": targetLocal, "c": targetUnresolved, "d": targetExternal, "e": targetWorkload}
	for _, e := range base.resolveExports(context.Background(), "obs", eps) {
		if want[e.Exporter] != e.Target {
			t.Errorf("%s: got %s want %s", e.Exporter, e.Target, want[e.Exporter])
		}
	}
}
