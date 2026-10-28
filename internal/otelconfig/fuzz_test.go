// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 CollectorCtrl

package otelconfig

import "testing"

// FuzzAnalyse feeds arbitrary text through the whole analysis path. Collector
// YAML comes from ConfigMaps the operator does not control, so parsing,
// role inference and endpoint extraction must never panic, whatever the input.
// Run longer with: go test ./internal/otelconfig -fuzz FuzzAnalyse -fuzztime 60s
func FuzzAnalyse(f *testing.F) {
	for _, seed := range []string{
		"",
		"receivers: {otlp: {protocols: {grpc: {}}}}\nexporters: {otlp: {endpoint: gw:4317}}\nservice: {pipelines: {traces: {receivers: [otlp], exporters: [otlp]}}}",
		"exporters: {loadbalancing: {resolver: {dns: {hostname: x}, static: {hostnames: [a, b]}, k8s: {service: s.ns}}}}\nservice: {pipelines: {traces: {exporters: [loadbalancing]}}}",
		"receivers: {filelog: {include: [/var/log/pods/*]}}\nservice: {pipelines: {logs: {receivers: [filelog]}}}",
		"service: {pipelines: {metrics: {receivers: [k8s_cluster], exporters: [otlphttp/x]}}}\nexporters: {otlphttp/x: {endpoint: 'https://[::1]:4318', traces_endpoint: '${env:X}'}}",
		"service: [1, 2]", "a: &a [*a]", "- - - -", "receivers: null",
	} {
		f.Add(seed, "DaemonSet")
	}
	f.Fuzz(func(t *testing.T, body, kind string) {
		a, err := Parse(body)
		if err != nil {
			return
		}
		_ = InferRole(a, kind)
		for _, e := range a.ExportEndpoints() {
			_ = e.Host
		}
	})
}
