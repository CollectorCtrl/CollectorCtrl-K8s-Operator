// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 CollectorCtrl

package v1alpha1

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

// The CRD is hand-written. Decode it strictly into the API server's own type
// so a mis-indented schema (a field where a keyword belongs) fails here, not
// at install time when server-side apply rejects it.
func TestCRDManifestsAreValidAndIdentical(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	for name, files := range map[string][2]string{
		"collectormonitors.collectorctrl.io": {"crd.yaml", "crd.yaml"},
		"instrumentations.collectorctrl.io":  {"instrumentation.yaml", "crd-instrumentation.yaml"},
	} {
		paths := []string{
			filepath.Join(root, "helm-charts", "collectorctrl-operator", "crds", files[0]),
			filepath.Join(root, "deploy", files[1]),
		}
		var first []byte
		for _, p := range paths {
			raw, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			var crd apiextensionsv1.CustomResourceDefinition
			if err := yaml.UnmarshalStrict(raw, &crd); err != nil {
				t.Fatalf("%s: %v", p, err)
			}
			if crd.Name != name || len(crd.Spec.Versions) != 1 || crd.Spec.Versions[0].Name != GroupVersion.Version {
				t.Fatalf("%s: unexpected CRD identity %s %+v", p, crd.Name, crd.Spec.Versions)
			}
			schema := crd.Spec.Versions[0].Schema.OpenAPIV3Schema
			status := schema.Properties["status"].Properties["conditions"].Items.Schema.Properties["status"]
			if len(status.Enum) != 3 {
				t.Errorf("%s: condition status enum = %v", p, status.Enum)
			}
			if crd.Spec.Versions[0].Subresources == nil || crd.Spec.Versions[0].Subresources.Status == nil {
				t.Errorf("%s: status subresource missing", p)
			}
			if first == nil {
				first = raw
			} else if !bytes.Equal(first, raw) {
				t.Errorf("%s differs from the Helm chart's CRD; keep them identical", p)
			}
		}
	}
}

// The Instrumentation schema must accept the documented example and the Go
// type must round-trip through JSON so the webhook reads what users wrote.
func TestInstrumentationRoundTrip(t *testing.T) {
	three := int32(3)
	in := &Instrumentation{Spec: InstrumentationSpec{
		Exporter: ExporterSpec{Endpoint: "http://otel-agent.observability:4318"}, Propagators: []string{"tracecontext", "baggage"},
		Sampler: &SamplerSpec{Type: "parentbased_traceidratio", Argument: "0.5"}, ResourceAttributes: map[string]string{"deployment.environment": "prod"},
		Registry: RegistrySpec{ReadImageConfig: true}, Canary: CanarySpec{MaxRestarts: &three},
	}}
	cp := in.DeepCopy()
	cp.Spec.ResourceAttributes["x"] = "y"
	*cp.Spec.Canary.MaxRestarts = 9
	if _, ok := in.Spec.ResourceAttributes["x"]; ok || *in.Spec.Canary.MaxRestarts != 3 {
		t.Fatal("DeepCopy shares memory with the original")
	}
	raw, err := yaml.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var back Instrumentation
	if err := yaml.UnmarshalStrict(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back.Spec.Sampler == nil || back.Spec.Sampler.Argument != "0.5" || back.MaxRestartsOrDefault() != 3 || !back.AutoRevertEnabled() {
		t.Fatalf("round trip lost fields: %+v", back.Spec)
	}
}
