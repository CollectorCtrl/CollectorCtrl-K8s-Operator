// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 CollectorCtrl

package readiness

import (
	"encoding/json"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

const fuzzSecret = "S3cretValue9"

// FuzzAnalyseRedacts drives readiness analysis with arbitrary launcher
// variables, arguments and annotations. It must never panic, and a credential
// passed as a JVM system property or a Node option must never reach the
// report, whatever comes before it.
// Run longer with: go test ./internal/readiness -fuzz FuzzAnalyseRedacts -fuzztime 60s
func FuzzAnalyseRedacts(f *testing.F) {
	f.Add("-javaagent:/otel/opentelemetry-javaagent.jar=x=y -Xmx1g", "--require dd-trace/init", "java -jar app.jar", "java", "/opt/x.so")
	f.Add("", "-r newrelic --import=@opentelemetry/auto-instrumentations-node/register", "ddtrace-run gunicorn", "app=python", "")
	f.Add("-agentpath:/dt/liboneagentproc.so", "", "dotnet app.dll", "", "liboneagentproc.so:/x")
	f.Add("\"-javaagent:'\"", "--require", "-jar", "app=,=java", ":::")
	f.Fuzz(func(t *testing.T, jto, nodeOpts, args, lang, preload string) {
		if strings.Contains(jto+nodeOpts+args+lang+preload, fuzzSecret) {
			t.Skip()
		}
		c := corev1.Container{Name: "app", Image: "acme/app", Args: strings.Fields(args), Env: []corev1.EnvVar{
			{Name: "JAVA_TOOL_OPTIONS", Value: jto + " -Dapi.key=" + fuzzSecret},
			{Name: "NODE_OPTIONS", Value: nodeOpts + " --api-key=" + fuzzSecret},
			{Name: "LD_PRELOAD", Value: preload},
			{Name: "CORECLR_ENABLE_PROFILING", Value: "1"},
		}}
		in := Input{
			Template:            corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{c}}},
			WorkloadAnnotations: map[string]string{AnnotationLanguage: lang},
			InjectorsChecked:    true,
		}
		raw, err := json.Marshal(Analyze(in))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), fuzzSecret) {
			t.Fatalf("secret leaked into the report: %s", raw)
		}
	})
}
