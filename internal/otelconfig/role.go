package otelconfig

import (
	"fmt"
	"strings"
)

// Roles a collector can be inferred to play. Names and kinds are never used to
// decide a role; only the configuration (and, as a weak tie-breaker, the
// workload kind) is.
const (
	RoleAgent           = "agent"
	RoleGateway         = "gateway"
	RoleClusterReceiver = "cluster-receiver"
	RoleAllInOne        = "all-in-one"
	RoleUnknown         = "unknown"
)

// Role is an inferred role with the evidence that produced it.
type Role struct {
	Name    string
	Reasons []string
}

var (
	nodeReceivers    = []string{"kubeletstats", "hostmetrics", "journald"}
	clusterReceivers = []string{"k8s_cluster", "k8sobjects", "k8s_events"}
	gatewayProcs     = []string{"tail_sampling", "groupbytrace"}
	gatewayExporters = []string{"loadbalancing"}
)

// InferRole infers the role from the pipelines' components. kind is the
// workload kind (DaemonSet, Deployment, StatefulSet); it only breaks ties when
// the configuration carries no role-specific components.
func InferRole(a *Analysis, kind string) Role {
	if a == nil {
		return Role{Name: RoleUnknown, Reasons: []string{"config unreadable"}}
	}
	type signal struct {
		role     string
		evidence []string
	}
	var found []signal

	agentEvidence := a.hasReceiverType(nodeReceivers...)
	agentEvidence = append(agentEvidence, a.podLogReceivers()...)
	if len(agentEvidence) > 0 {
		found = append(found, signal{RoleAgent, agentEvidence})
	}
	if ev := a.hasReceiverType(clusterReceivers...); len(ev) > 0 {
		found = append(found, signal{RoleClusterReceiver, ev})
	}
	gatewayEvidence := append(a.hasProcessorType(gatewayProcs...), a.hasExporterType(gatewayExporters...)...)
	if len(gatewayEvidence) > 0 {
		found = append(found, signal{RoleGateway, gatewayEvidence})
	}

	switch len(found) {
	case 0:
		otlp := a.hasReceiverType("otlp")
		switch {
		case len(otlp) > 0 && kind == "DaemonSet":
			return Role{RoleAgent, []string{"DaemonSet receiving OTLP from workloads on its node", "no node, cluster or gateway components"}}
		case len(otlp) > 0:
			return Role{RoleGateway, []string{"receives OTLP only", "no node or cluster receivers"}}
		case kind == "DaemonSet":
			return Role{RoleAgent, []string{"DaemonSet", "no role-specific components"}}
		}
		return Role{RoleUnknown, []string{"no role-specific components"}}
	case 1:
		return Role{found[0].role, []string{"uses " + strings.Join(found[0].evidence, ", ")}}
	}
	reasons := make([]string, 0, len(found))
	for _, s := range found {
		reasons = append(reasons, fmt.Sprintf("%s components: %s", s.role, strings.Join(s.evidence, ", ")))
	}
	return Role{RoleAllInOne, reasons}
}

// podLogReceivers returns filelog receivers that tail node-local pod logs.
func (a *Analysis) podLogReceivers() []string {
	var out []string
	for _, id := range a.hasReceiverType("filelog") {
		for _, path := range asStrings(asMap(a.receiverCfg[id])["include"]) {
			if strings.Contains(path, "/var/log/pods") || strings.Contains(path, "/var/log/containers") {
				out = append(out, id)
				break
			}
		}
	}
	return out
}
