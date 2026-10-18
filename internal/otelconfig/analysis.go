// Package otelconfig derives facts from an OpenTelemetry Collector YAML
// configuration: which components the pipelines actually use, what role the
// collector most likely plays, and where its exporters send data.
//
// Everything here is inference from the configuration text. It never decides
// anything about the running collector, and callers must present results as
// "inferred" with the reasons attached.
package otelconfig

import (
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Analysis is the parsed, pipeline-aware view of a collector configuration.
type Analysis struct {
	// Receivers, Processors and Exporters list the component IDs referenced by
	// service.pipelines, sorted. Components defined but unused are excluded.
	Receivers  []string
	Processors []string
	Exporters  []string

	receiverCfg map[string]any
	exporterCfg map[string]any
}

// Parse reads collector YAML. An empty or non-mapping document is an error so
// callers can report "config unreadable" instead of guessing.
func Parse(body string) (*Analysis, error) {
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(body), &doc); err != nil {
		return nil, fmt.Errorf("parse collector config: %w", err)
	}
	if len(doc) == 0 {
		return nil, fmt.Errorf("collector config is empty or not a YAML mapping")
	}
	a := &Analysis{receiverCfg: asMap(doc["receivers"]), exporterCfg: asMap(doc["exporters"])}
	pipelines := asMap(asMap(doc["service"])["pipelines"])
	if len(pipelines) == 0 {
		return nil, fmt.Errorf("collector config has no service.pipelines")
	}
	recv, proc, exp := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, p := range pipelines {
		pm := asMap(p)
		for _, id := range asStrings(pm["receivers"]) {
			recv[id] = true
		}
		for _, id := range asStrings(pm["processors"]) {
			proc[id] = true
		}
		for _, id := range asStrings(pm["exporters"]) {
			exp[id] = true
		}
	}
	a.Receivers, a.Processors, a.Exporters = sortedKeys(recv), sortedKeys(proc), sortedKeys(exp)
	return a, nil
}

// ComponentType returns the type part of a component ID ("otlp/grpc" -> "otlp").
func ComponentType(id string) string {
	return strings.TrimSpace(strings.SplitN(id, "/", 2)[0])
}

func (a *Analysis) hasReceiverType(types ...string) []string {
	return matchTypes(a.Receivers, types)
}
func (a *Analysis) hasProcessorType(types ...string) []string {
	return matchTypes(a.Processors, types)
}
func (a *Analysis) hasExporterType(types ...string) []string {
	return matchTypes(a.Exporters, types)
}

func matchTypes(ids []string, types []string) []string {
	var out []string
	for _, id := range ids {
		t := ComponentType(id)
		for _, want := range types {
			if t == want {
				out = append(out, id)
			}
		}
	}
	return out
}

func asMap(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

func asStrings(v any) []string {
	switch t := v.(type) {
	case []any:
		out := make([]string, 0, len(t))
		for _, item := range t {
			if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, strings.TrimSpace(s))
			}
		}
		return out
	case string:
		if strings.TrimSpace(t) != "" {
			return []string{strings.TrimSpace(t)}
		}
	}
	return nil
}

func asString(v any) string {
	if s, ok := v.(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
