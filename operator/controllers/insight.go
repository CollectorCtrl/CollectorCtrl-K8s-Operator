// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 CollectorCtrl

package controllers

import (
	"context"
	"net"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/collectorctrl/collectorctrl/internal/otelconfig"
	v1 "github.com/collectorctrl/collectorctrl/operator/api/v1alpha1"
	"github.com/collectorctrl/collectorctrl/pkg/opamp"
)

// Opt-in label and optional override annotations on collector workloads.
const (
	LabelObserve      = "collectorctrl.io/observe"
	AnnContainer      = "collectorctrl.io/container"
	AnnConfigMap      = "collectorctrl.io/config-map"
	AnnConfigKey      = "collectorctrl.io/config-key"
	AnnStack          = "collectorctrl.io/stack"
	AnnRole           = "collectorctrl.io/role"
	AnnReportConfig   = "collectorctrl.io/report-config"
	AnnReportInterval = "collectorctrl.io/report-interval"
)

const (
	discoveryLabel            = "label"
	discoveryCollectorMonitor = "collectormonitor"
)

// Export edge targets.
const (
	targetWorkload   = "workload"
	targetService    = "service"
	targetExternal   = "external"
	targetLocal      = "local"
	targetUnresolved = "unresolved"
)

type insight struct {
	analysis    *otelconfig.Analysis
	configError string
	role        otelconfig.Role
	roleSource  string
	stack       string
	stackSource string
	exports     []opamp.ExportEdge
}

var knownRoles = map[string]bool{
	otelconfig.RoleAgent: true, otelconfig.RoleGateway: true, otelconfig.RoleClusterReceiver: true,
	otelconfig.RoleAllInOne: true, otelconfig.RoleUnknown: true,
}

// annotation returns a monitor annotation, falling back to the workload's.
func annotation(m *v1.CollectorMonitor, w client.Object, key string) string {
	if v := strings.TrimSpace(m.GetAnnotations()[key]); v != "" {
		return v
	}
	return strings.TrimSpace(w.GetAnnotations()[key])
}

// analyse infers role, stack and export topology from the mounted config.
// Every result is labelled with its source so the UI can show why.
func (r *CollectorMonitorReconciler) analyse(ctx context.Context, m *v1.CollectorMonitor, w client.Object, kind, body string) insight {
	in := insight{roleSource: "inferred"}
	a, err := otelconfig.Parse(body)
	if err != nil {
		in.configError = err.Error()
	} else {
		in.analysis = a
	}
	in.role = otelconfig.InferRole(in.analysis, kind)
	if override := annotation(m, w, AnnRole); override != "" {
		if knownRoles[override] {
			in.role = otelconfig.Role{Name: override, Reasons: []string{"set by " + AnnRole}}
			in.roleSource = "annotation"
		} else {
			in.role.Reasons = append(in.role.Reasons, "ignored unknown "+AnnRole+" value "+override)
		}
	}
	in.stack, in.stackSource = stackOf(m, w)
	if in.analysis != nil {
		in.exports = r.resolveExports(ctx, w.GetNamespace(), in.analysis.ExportEndpoints())
	}
	return in
}

// stackOf groups collector workloads that were deployed together. The order is
// an explicit annotation, an owning OpenTelemetryCollector resource, the Helm
// release, the Argo CD application, and otherwise the workload alone.
func stackOf(m *v1.CollectorMonitor, w client.Object) (string, string) {
	if s := annotation(m, w, AnnStack); s != "" {
		return s, "annotation"
	}
	for _, ref := range w.GetOwnerReferences() {
		if ref.Kind == "OpenTelemetryCollector" {
			return ref.Name, "otel-operator"
		}
	}
	l := w.GetLabels()
	if s := l["app.kubernetes.io/instance"]; s != "" {
		return s, "helm-release"
	}
	if s := l["argocd.argoproj.io/instance"]; s != "" {
		return s, "argocd-app"
	}
	return w.GetName(), "standalone"
}

// resolveExports maps exporter endpoints to Services and the workloads behind
// them. Anything that cannot be matched in the cluster is reported as external
// or unresolved rather than guessed.
func (r *CollectorMonitorReconciler) resolveExports(ctx context.Context, namespace string, endpoints []otelconfig.Endpoint) []opamp.ExportEdge {
	out := make([]opamp.ExportEdge, 0, len(endpoints))
	for _, ep := range endpoints {
		edge := opamp.ExportEdge{Exporter: ep.Exporter, Endpoint: ep.Raw}
		switch {
		case ep.Unresolved:
			edge.Target = targetUnresolved
		case ep.Host == "localhost" || ep.Host == "0.0.0.0" || ep.Host == "::1" || strings.HasPrefix(ep.Host, "127."):
			edge.Target = targetLocal
		case net.ParseIP(ep.Host) != nil:
			edge.Target, edge.Name = targetExternal, ep.Host
		default:
			r.resolveHost(ctx, namespace, ep.Host, &edge)
		}
		out = append(out, edge)
	}
	return out
}

func (r *CollectorMonitorReconciler) resolveHost(ctx context.Context, namespace, host string, edge *opamp.ExportEdge) {
	parts := strings.Split(host, ".")
	svcName, svcNamespace, clusterForm := "", "", false
	switch {
	case len(parts) == 1:
		svcName, svcNamespace, clusterForm = parts[0], namespace, true
	case len(parts) >= 3 && parts[2] == "svc":
		svcName, svcNamespace, clusterForm = parts[0], parts[1], true
	case len(parts) == 2:
		svcName, svcNamespace = parts[0], parts[1] // service.namespace, or an external domain
	default:
		edge.Target, edge.Name = targetExternal, host
		return
	}
	svc := &corev1.Service{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: svcNamespace, Name: svcName}, svc); err != nil {
		if clusterForm {
			edge.Target = targetUnresolved
		} else {
			edge.Target, edge.Name = targetExternal, host
		}
		return
	}
	edge.Target, edge.Namespace, edge.Service = targetService, svcNamespace, svcName
	if len(svc.Spec.Selector) == 0 {
		return
	}
	selector := labels.SelectorFromSet(svc.Spec.Selector)
	var matches []struct{ kind, name string }
	opts := []client.ListOption{client.InNamespace(svcNamespace)}
	var ds appsv1.DaemonSetList
	if r.List(ctx, &ds, opts...) == nil {
		for _, w := range ds.Items {
			if selector.Matches(labels.Set(w.Spec.Template.Labels)) {
				matches = append(matches, struct{ kind, name string }{"DaemonSet", w.Name})
			}
		}
	}
	var deps appsv1.DeploymentList
	if r.List(ctx, &deps, opts...) == nil {
		for _, w := range deps.Items {
			if selector.Matches(labels.Set(w.Spec.Template.Labels)) {
				matches = append(matches, struct{ kind, name string }{"Deployment", w.Name})
			}
		}
	}
	var sts appsv1.StatefulSetList
	if r.List(ctx, &sts, opts...) == nil {
		for _, w := range sts.Items {
			if selector.Matches(labels.Set(w.Spec.Template.Labels)) {
				matches = append(matches, struct{ kind, name string }{"StatefulSet", w.Name})
			}
		}
	}
	if len(matches) == 1 {
		edge.Target, edge.Kind, edge.Name = targetWorkload, matches[0].kind, matches[0].name
	}
}

// isLabelKey reports whether a connection key belongs to label discovery.
func isLabelKey(key string) bool { return strings.Contains(key, "/"+labelPrefix) }
