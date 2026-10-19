package controllers

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/types"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/collectorctrl/collectorctrl/internal/otelconfig"
)

// Golden tests against manifests rendered from real vendor Helm charts
// (see hack/render-vendor-charts.sh for chart versions and values). They check
// what a customer would see after opting these collectors in: stack, role and
// where each collector sends data. Regenerate the fixtures when bumping charts
// and review every expectation change by hand.

type vendorWorkload struct {
	role    string
	stack   string
	exports []string // "exporter -> target[:kind/name]", sorted
}

var vendorExpectations = map[string]map[string]vendorWorkload{
	"upstream-agent.yaml": {
		"DaemonSet/up-agent-opentelemetry-collector-agent": {role: otelconfig.RoleAgent, stack: "up-agent", exports: []string{"debug -> none"}},
	},
	"upstream-gateway.yaml": {
		"Deployment/up-gateway-opentelemetry-collector": {role: otelconfig.RoleGateway, stack: "up-gateway", exports: []string{"debug -> none"}},
	},
	"upstream-cluster.yaml": {
		"Deployment/up-cluster-opentelemetry-collector": {role: otelconfig.RoleClusterReceiver, stack: "up-cluster", exports: []string{"debug -> none"}},
	},
	// Splunk: node agent forwards everything to the chart's gateway, which (with
	// the cluster receiver) exports to Splunk Observability Cloud. The chart uses
	// snake_case component names (kubelet_stats, otlp_grpc, k8s_cluster).
	"splunk.yaml": {
		"DaemonSet/splunk-splunk-otel-collector-agent": {role: otelconfig.RoleAgent, stack: "splunk", exports: []string{
			"otlp_grpc -> workload:Deployment/splunk-splunk-otel-collector",
			"otlp_http/entities -> workload:Deployment/splunk-splunk-otel-collector",
			"signalfx/histograms -> workload:Deployment/splunk-splunk-otel-collector",
			"signalfx/host_metadata -> workload:Deployment/splunk-splunk-otel-collector",
		}},
		"Deployment/splunk-splunk-otel-collector-k8s-cluster-receiver": {role: otelconfig.RoleClusterReceiver, stack: "splunk", exports: []string{
			"signalfx -> external:ingest.us0.observability.splunkcloud.com",
		}},
		"Deployment/splunk-splunk-otel-collector": {role: otelconfig.RoleGateway, stack: "splunk", exports: []string{
			"otlp_http -> external:ingest.us0.observability.splunkcloud.com",
			"otlp_http/entities -> external:ingest.us0.observability.splunkcloud.com",
			"signalfx -> external:ingest.us0.observability.splunkcloud.com",
		}},
	},
	// Coralogix otel-integration: node agent (uses groupbytrace and spanmetrics
	// connectors, which must not make it a gateway) and a cluster collector,
	// both exporting straight to the Coralogix domain.
	"coralogix.yaml": {
		"DaemonSet/coralogix-opentelemetry-agent": {role: otelconfig.RoleAgent, stack: "cx", exports: []string{
			"coralogix -> external:eu2.coralogix.com",
			"coralogix/resource_catalog -> external:eu2.coralogix.com",
		}},
		"Deployment/coralogix-opentelemetry-collector": {role: otelconfig.RoleClusterReceiver, stack: "cx", exports: []string{
			"coralogix -> external:eu2.coralogix.com",
			"coralogix/resource_catalog -> external:eu2.coralogix.com",
			"debug -> none",
		}},
	},
	// AWS ADOT for EKS on EC2: a prometheus-scraping DaemonSet. With no
	// role-specific receivers the DaemonSet kind decides; the remote-write
	// endpoint is left empty by the chart's defaults.
	"adot.yaml": {
		"DaemonSet/adot-collector-daemonset": {role: otelconfig.RoleAgent, stack: "adot", exports: []string{"prometheusremotewrite -> none"}},
	},
}

func TestVendorChartGolden(t *testing.T) {
	files, err := filepath.Glob("testdata/vendorcharts/*.yaml")
	if err != nil || len(files) == 0 {
		t.Fatalf("no vendor chart fixtures: %v", err)
	}
	for _, file := range files {
		name := filepath.Base(file)
		t.Run(name, func(t *testing.T) {
			got := analyseRenderedChart(t, file)
			want, ok := vendorExpectations[name]
			if !ok {
				for k, v := range got {
					t.Logf("%s: role=%s stack=%s exports=%v", k, v.role, v.stack, v.exports)
				}
				t.Fatalf("no expectations recorded for %s", name)
			}
			for key, w := range want {
				g, ok := got[key]
				if !ok {
					t.Errorf("%s: workload not analysed (have %v)", key, keys(got))
					continue
				}
				if g.role != w.role || g.stack != w.stack || strings.Join(g.exports, "|") != strings.Join(w.exports, "|") {
					t.Errorf("%s:\n got  role=%s stack=%s exports=%v\n want role=%s stack=%s exports=%v", key, g.role, g.stack, g.exports, w.role, w.stack, w.exports)
				}
			}
			for key := range got {
				if _, ok := want[key]; !ok {
					t.Errorf("unexpected collector workload %s", key)
				}
			}
		})
	}
}

func keys(m map[string]vendorWorkload) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// analyseRenderedChart loads every object into a fake API and runs the same
// discovery and analysis the label path uses, for each workload that mounts a
// collector configuration.
func analyseRenderedChart(t *testing.T, path string) map[string]vendorWorkload {
	t.Helper()
	objects := decodeManifests(t, path)
	r := fixture(t, objects...)
	out := map[string]vendorWorkload{}
	for _, obj := range objects {
		var kind string
		var tmpl *corev1.PodTemplateSpec
		switch w := obj.(type) {
		case *appsv1.DaemonSet:
			kind, tmpl = "DaemonSet", &w.Spec.Template
		case *appsv1.Deployment:
			kind, tmpl = "Deployment", &w.Spec.Template
		case *appsv1.StatefulSet:
			kind, tmpl = "StatefulSet", &w.Spec.Template
		default:
			continue
		}
		for _, c := range tmpl.Spec.Containers {
			m := syntheticMonitor(obj, kind, "")
			m.Spec.CollectorContainer = c.Name
			cm, key, err := r.discoverConfig(context.Background(), m, obj, c.Name)
			if err != nil {
				continue // not a collector container, or no single collector config
			}
			in := r.analyse(context.Background(), m, obj, kind, cm.Data[key])
			if in.analysis == nil {
				continue
			}
			var exports []string
			for _, e := range in.exports {
				target := e.Target
				if e.Kind != "" {
					target += ":" + e.Kind + "/" + e.Name
				} else if e.Target == targetExternal || e.Target == targetService {
					target += ":" + e.Name + e.Service
				}
				exports = append(exports, e.Exporter+" -> "+target)
			}
			// Exporters with no endpoint (debug, nop, awsemf...) still matter to a reader.
			seen := map[string]bool{}
			for _, e := range in.exports {
				seen[e.Exporter] = true
			}
			for _, id := range in.analysis.Exporters {
				if !seen[id] {
					exports = append(exports, id+" -> none")
				}
			}
			sort.Strings(exports)
			out[kind+"/"+obj.GetName()] = vendorWorkload{role: in.role.Name, stack: in.stack, exports: exports}
			break
		}
	}
	return out
}

func decodeManifests(t *testing.T, path string) []client.Object {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := runtime.NewScheme()
	_ = appsv1.AddToScheme(s)
	_ = corev1.AddToScheme(s)
	decoder := serializer.NewCodecFactory(s).UniversalDeserializer()
	reader := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(raw)))
	var out []client.Object
	for {
		doc, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if len(bytes.TrimSpace(doc)) == 0 {
			continue
		}
		obj, _, err := decoder.Decode(doc, nil, nil)
		if err != nil {
			continue // kinds outside apps/v1 and core/v1 (RBAC, CRs) are irrelevant here
		}
		switch o := obj.(type) {
		case *appsv1.DaemonSet, *appsv1.Deployment, *appsv1.StatefulSet, *corev1.ConfigMap, *corev1.Service:
			co := o.(client.Object)
			if co.GetNamespace() == "" {
				co.SetNamespace(namespaceOf(path))
			}
			co.SetUID(typesUID(co))
			out = append(out, co)
		}
	}
	return out
}

// helm template omits metadata.namespace for most charts; use the release
// namespace recorded in the render script.
func namespaceOf(path string) string {
	switch filepath.Base(path) {
	case "splunk.yaml":
		return "splunk"
	case "adot.yaml":
		return "amazon-metrics"
	}
	return "observability"
}

func typesUID(o client.Object) types.UID {
	return types.UID(strings.ToLower(o.GetNamespace() + "-" + o.GetName() + "-" + strings.TrimPrefix(reflect.TypeOf(o).String(), "*")))
}
