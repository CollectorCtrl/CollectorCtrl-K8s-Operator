package controllers

import (
	"context"
	"testing"

	v1 "github.com/collectorctrl/collectorctrl/operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestLooksLikeCollector(t *testing.T) {
	yes := []string{
		"otel/opentelemetry-collector-contrib:0.112.0",
		"ghcr.io/open-telemetry/opentelemetry-collector-releases/opentelemetry-collector-k8s:0.140.0",
		"quay.io/signalfx/splunk-otel-collector:0.162.0",
		"public.ecr.aws/aws-observability/aws-otel-collector:v0.40.0",
		"registry.corp/platform/otelcol-custom@sha256:abc",
		"docker.io/grafana/alloy:v1.5.0",
	}
	no := []string{"nginx:1.27", "ghcr.io/acme/payments-api:otel-collector", "localhost:5000/payments:otelcol"}
	for _, img := range yes {
		if !looksLikeCollector(img) {
			t.Errorf("missed collector image %s", img)
		}
	}
	for _, img := range no {
		if looksLikeCollector(img) {
			t.Errorf("false positive %s", img)
		}
	}
	// Tags never count: an app tagged "otel-collector" is not a collector.
	if looksLikeCollector("ghcr.io/acme/payments-api:otel-collector") {
		t.Error("tag matched as repository")
	}
}

func deploymentWith(name string, labels map[string]string, images ...string) *appsv1.Deployment {
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "obs", UID: types.UID("uid-" + name), Labels: labels}}
	for i, img := range images {
		d.Spec.Template.Spec.Containers = append(d.Spec.Template.Spec.Containers, corev1.Container{Name: []string{"app", "collector"}[i%2], Image: img})
	}
	return d
}

func TestClusterInventoryMarksObservedWorkloads(t *testing.T) {
	agent := labelledAgent()
	agent.Spec.Template.Spec.Containers[0].Image = "otel/opentelemetry-collector-contrib:0.112.0"
	monitored := deploymentWith("gateway", map[string]string{"app.kubernetes.io/instance": "platform"}, "otel/opentelemetry-collector-k8s:0.140.0")
	candidate := deploymentWith("legacy", nil, "busybox:1", "quay.io/signalfx/splunk-otel-collector:0.162.0")
	app := deploymentWith("payments", nil, "ghcr.io/acme/payments-api:1.0")
	cr := &v1.CollectorMonitor{ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "obs"},
		Spec: v1.CollectorMonitorSpec{WorkloadSelector: v1.WorkloadSelector{Kind: "Deployment", Name: "gateway"}}}
	r := fixture(t, agent, monitored, candidate, app, cr)
	r.ClusterID = "cluster-1"
	inv, err := (&ClusterObserver{Monitors: r, ClusterName: "prod-eu"}).Inventory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if inv.ClusterID != "cluster-1" || inv.ClusterName != "prod-eu" || inv.Source != "kubernetes-api" {
		t.Fatalf("header: %+v", inv)
	}
	got := map[string]string{}
	for _, c := range inv.Collectors {
		state := "candidate"
		if c.OptedIn {
			state = "opted-in"
		} else if c.Monitored {
			state = "monitored"
		}
		got[c.Kind+"/"+c.Name] = state + "|" + c.Container + "|" + c.Stack
	}
	want := map[string]string{
		"DaemonSet/agent":    "opted-in|otel|coralogix-otel",
		"Deployment/gateway": "monitored|app|platform",
		"Deployment/legacy":  "candidate|collector|legacy",
	}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: got %q want %q", k, got[k], v)
		}
	}
}

func TestClusterInventoryOnVendorCharts(t *testing.T) {
	objects := decodeManifests(t, "testdata/vendorcharts/splunk.yaml")
	objects = append(objects, decodeManifests(t, "testdata/vendorcharts/coralogix.yaml")...)
	r := fixture(t, objects...)
	r.ClusterID = "c"
	inv, err := (&ClusterObserver{Monitors: r}).Inventory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, c := range inv.Collectors {
		names[c.Name] = true
	}
	for _, n := range []string{"splunk-splunk-otel-collector-agent", "splunk-splunk-otel-collector-k8s-cluster-receiver", "splunk-splunk-otel-collector", "coralogix-opentelemetry-agent", "coralogix-opentelemetry-collector"} {
		if !names[n] {
			t.Errorf("vendor collector %s not listed as a candidate (have %v)", n, names)
		}
	}
}
