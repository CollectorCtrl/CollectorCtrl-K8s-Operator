// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 CollectorCtrl

package readiness

import (
	"sort"
	"strings"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

// Injector modes.
const (
	// ModeRequest injectors act only on pods that ask for it (the upstream OTel
	// Operator's inject annotations, New Relic's operator, Elastic's attacher).
	ModeRequest = "request"
	// ModeSelector injectors act on every pod their webhook selectors match
	// (Dynatrace, Datadog's admission controller, Instana).
	ModeSelector = "selector"
)

// Injector is an APM vendor's pod-mutating admission webhook.
type Injector struct {
	Name          string   `json:"name"` // MutatingWebhookConfiguration name
	Vendor        string   `json:"vendor"`
	Mode          string   `json:"mode"`
	Webhooks      []string `json:"webhooks"`
	FailurePolicy string   `json:"failure_policy,omitempty"` // Fail on any hook wins
	selectors     []hookSelector
}

type hookSelector struct{ namespace, object labels.Selector }

var injectorVendors = []vendorPattern{
	{"opentelemetry", "opentelemetry"},
	{"datadog", "datadog"},
	{"dynatrace", "dynatrace"},
	{"newrelic", "newrelic"},
	{"k8s-agents-operator", "newrelic"},
	{"instana", "instana"},
	{"appdynamics", "appdynamics"},
	{"elastic-apm", "elastic"},
	{"apm-attacher", "elastic"},
	{"splunk", "splunk"},
}

var requestModeVendors = map[string]bool{"opentelemetry": true, "newrelic": true, "elastic": true, "splunk": true}

// ClassifyWebhooks keeps the webhooks that belong to an APM vendor and mutate
// pods on CREATE. Everything else (policy engines, service meshes) is ignored.
func ClassifyWebhooks(configs []admissionv1.MutatingWebhookConfiguration) []Injector {
	var out []Injector
	for _, cfg := range configs {
		var inj *Injector
		for _, hook := range cfg.Webhooks {
			if !mutatesPodCreate(hook) {
				continue
			}
			text := strings.ToLower(cfg.Name + " " + hook.Name)
			if svc := hook.ClientConfig.Service; svc != nil {
				text += " " + strings.ToLower(svc.Namespace+"/"+svc.Name)
			}
			vendor := vendorOf(text, injectorVendors)
			if vendor == "" || !injectsAgent(vendor, hook.Name) {
				continue
			}
			if inj == nil {
				mode := ModeSelector
				if requestModeVendors[vendor] {
					mode = ModeRequest
				}
				inj = &Injector{Name: cfg.Name, Vendor: vendor, Mode: mode}
			}
			inj.Webhooks = append(inj.Webhooks, hook.Name)
			if hook.FailurePolicy != nil && (inj.FailurePolicy == "" || *hook.FailurePolicy == admissionv1.Fail) {
				inj.FailurePolicy = string(*hook.FailurePolicy)
			}
			inj.selectors = append(inj.selectors, hookSelector{namespace: selector(hook.NamespaceSelector), object: selector(hook.ObjectSelector)})
		}
		if inj != nil {
			sort.Strings(inj.Webhooks)
			out = append(out, *inj)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// injectsAgent drops hooks that only add metadata or settings. Datadog's
// cluster agent registers several pod hooks (agent config, standard tags, CWS,
// sidecars); only library injection loads an APM agent. New Relic and others
// ship metadata-injection webhooks that add environment variables only.
func injectsAgent(vendor, hook string) bool {
	h := strings.ToLower(hook)
	if strings.Contains(h, "metadata") {
		return false
	}
	if vendor == "datadog" {
		return strings.Contains(h, "lib.injection") || strings.Contains(h, "auto.instrumentation") || strings.Contains(h, "apm")
	}
	return true
}

// SameFamily reports whether two vendors ship the same agent family: Splunk
// distributes OpenTelemetry agents and installs the upstream OTel Operator.
func SameFamily(a, b string) bool {
	if a == b {
		return true
	}
	otel := func(v string) bool { return v == "opentelemetry" || v == "splunk" }
	return otel(a) && otel(b)
}

func mutatesPodCreate(hook admissionv1.MutatingWebhook) bool {
	for _, rule := range hook.Rules {
		ops := false
		for _, op := range rule.Operations {
			if op == admissionv1.Create || op == admissionv1.OperationAll {
				ops = true
			}
		}
		if !ops {
			continue
		}
		for _, res := range rule.Resources {
			if res == "pods" || res == "*" || res == "*/*" {
				return true
			}
		}
	}
	return false
}

// selector converts a webhook selector. A nil selector matches everything; an
// invalid one matches nothing (the API server would reject such a webhook).
func selector(s *metav1.LabelSelector) labels.Selector {
	if s == nil {
		return labels.Everything()
	}
	sel, err := metav1.LabelSelectorAsSelector(s)
	if err != nil {
		return labels.Nothing()
	}
	return sel
}

// Selects reports whether the injector would mutate a new pod with these
// namespace and pod labels. Request-mode injectors also need a request from
// the same vendor.
func (i Injector) Selects(nsLabels, podLabels map[string]string, requests []Request) bool {
	if i.Mode == ModeRequest {
		asked := false
		for _, r := range requests {
			if r.Vendor == i.Vendor || (i.Vendor == "splunk" && r.Vendor == "opentelemetry") {
				asked = true
			}
		}
		if !asked {
			return false
		}
	}
	for _, s := range i.selectors {
		if s.namespace.Matches(labels.Set(nsLabels)) && s.object.Matches(labels.Set(podLabels)) {
			return true
		}
	}
	return false
}
