package otelconfig

import (
	"reflect"
	"strings"
	"testing"
)

const nodeAgent = `
receivers:
  otlp: {protocols: {grpc: {}}}
  kubeletstats: {auth_type: serviceAccount}
  filelog:
    include: [/var/log/pods/*/*/*.log]
  prometheus/unused: {}
processors:
  batch: {}
exporters:
  otlp:
    endpoint: coralogix-opentelemetry-collector:4317
service:
  pipelines:
    metrics: {receivers: [otlp, kubeletstats], processors: [batch], exporters: [otlp]}
    logs: {receivers: [filelog], processors: [batch], exporters: [otlp]}
`

const gateway = `
receivers:
  otlp: {protocols: {grpc: {}, http: {}}}
processors:
  tail_sampling: {}
exporters:
  coralogix:
    domain: eu2.coralogix.com
  otlphttp/backup:
    endpoint: https://ingest.example.com:4318/v1/traces
service:
  pipelines:
    traces: {receivers: [otlp], processors: [tail_sampling], exporters: [coralogix, otlphttp/backup]}
`

const clusterReceiver = `
receivers:
  k8s_cluster: {}
  k8sobjects: {}
exporters:
  otlp: {endpoint: "dns:///gateway.observability.svc.cluster.local:4317"}
service:
  pipelines:
    metrics: {receivers: [k8s_cluster], exporters: [otlp]}
    logs: {receivers: [k8sobjects], exporters: [otlp]}
`

func mustParse(t *testing.T, body string) *Analysis {
	t.Helper()
	a, err := Parse(body)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestOnlyPipelineComponentsCount(t *testing.T) {
	a := mustParse(t, nodeAgent)
	if !reflect.DeepEqual(a.Receivers, []string{"filelog", "kubeletstats", "otlp"}) {
		t.Fatalf("receivers: %v", a.Receivers)
	}
	for _, r := range a.Receivers {
		if r == "prometheus/unused" {
			t.Fatal("unused receiver counted")
		}
	}
}

func TestRoleInference(t *testing.T) {
	cases := []struct {
		name, body, kind, want string
	}{
		{"node agent", nodeAgent, "DaemonSet", RoleAgent},
		{"agent evidence beats kind", nodeAgent, "Deployment", RoleAgent},
		{"gateway", gateway, "Deployment", RoleGateway},
		{"gateway as daemonset", gateway, "DaemonSet", RoleGateway},
		{"cluster receiver", clusterReceiver, "Deployment", RoleClusterReceiver},
		{"otlp-only deployment", "receivers: {otlp: {}}\nexporters: {debug: {}}\nservice: {pipelines: {traces: {receivers: [otlp], exporters: [debug]}}}", "Deployment", RoleGateway},
		{"otlp-only daemonset", "receivers: {otlp: {}}\nexporters: {debug: {}}\nservice: {pipelines: {traces: {receivers: [otlp], exporters: [debug]}}}", "DaemonSet", RoleAgent},
		{"no signals", "receivers: {prometheus: {}}\nexporters: {debug: {}}\nservice: {pipelines: {metrics: {receivers: [prometheus], exporters: [debug]}}}", "StatefulSet", RoleUnknown},
		{"filelog off-node path is not agent evidence", "receivers: {filelog: {include: [/data/app.log]}}\nexporters: {debug: {}}\nservice: {pipelines: {logs: {receivers: [filelog], exporters: [debug]}}}", "Deployment", RoleUnknown},
	}
	for _, c := range cases {
		got := InferRole(mustParse(t, c.body), c.kind)
		if got.Name != c.want || len(got.Reasons) == 0 {
			t.Errorf("%s: got %s %v, want %s", c.name, got.Name, got.Reasons, c.want)
		}
	}
}

func TestAllInOne(t *testing.T) {
	body := `
receivers: {kubeletstats: {}, k8s_cluster: {}, otlp: {}}
processors: {tail_sampling: {}}
exporters: {debug: {}}
service:
  pipelines:
    metrics: {receivers: [kubeletstats, k8s_cluster], exporters: [debug]}
    traces: {receivers: [otlp], processors: [tail_sampling], exporters: [debug]}
`
	got := InferRole(mustParse(t, body), "Deployment")
	if got.Name != RoleAllInOne || len(got.Reasons) != 3 {
		t.Fatalf("got %+v", got)
	}
}

func TestUnreadableConfig(t *testing.T) {
	for _, body := range []string{"", "not: [valid", "receivers: {otlp: {}}"} {
		if _, err := Parse(body); err == nil {
			t.Errorf("accepted %q", body)
		}
	}
	if InferRole(nil, "DaemonSet").Name != RoleUnknown {
		t.Fatal("nil analysis must be unknown")
	}
}

func TestExportEndpoints(t *testing.T) {
	got := map[string]Endpoint{}
	for _, body := range []string{nodeAgent, gateway, clusterReceiver} {
		for _, e := range mustParse(t, body).ExportEndpoints() {
			got[e.Exporter+"|"+e.Raw] = e
		}
	}
	check := func(key, host, port string) {
		t.Helper()
		e, ok := got[key]
		if !ok || e.Host != host || e.Port != port || e.Unresolved {
			t.Errorf("%s: got %+v", key, e)
		}
	}
	check("otlp|coralogix-opentelemetry-collector:4317", "coralogix-opentelemetry-collector", "4317")
	check("coralogix|eu2.coralogix.com", "eu2.coralogix.com", "")
	check("otlphttp/backup|https://ingest.example.com:4318/v1/traces", "ingest.example.com", "4318")
	check("otlp|dns:///gateway.observability.svc.cluster.local:4317", "gateway.observability.svc.cluster.local", "4317")
}

func TestLoadbalancingAndEnvEndpoints(t *testing.T) {
	body := `
receivers: {otlp: {}}
exporters:
  loadbalancing:
    resolver:
      k8s: {service: sampler.observability}
  otlp/env:
    endpoint: ${env:BACKEND_ENDPOINT}
service:
  pipelines:
    traces: {receivers: [otlp], exporters: [loadbalancing, otlp/env]}
`
	eps := mustParse(t, body).ExportEndpoints()
	if len(eps) != 2 {
		t.Fatalf("got %+v", eps)
	}
	for _, e := range eps {
		switch e.Exporter {
		case "loadbalancing":
			if e.Host != "sampler.observability" {
				t.Errorf("k8s resolver host: %+v", e)
			}
		case "otlp/env":
			if !e.Unresolved || e.Host != "" {
				t.Errorf("env endpoint must stay unresolved: %+v", e)
			}
		}
	}
	if !strings.HasPrefix(InferRole(mustParse(t, body), "Deployment").Reasons[0], "uses loadbalancing") {
		t.Error("loadbalancing exporter should mark a gateway")
	}
}
