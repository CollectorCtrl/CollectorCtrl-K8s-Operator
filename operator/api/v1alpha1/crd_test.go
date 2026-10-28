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
	paths := []string{
		filepath.Join("..", "..", "..", "helm-charts", "collectorctrl-operator", "crds", "crd.yaml"),
		filepath.Join("..", "..", "..", "deploy", "crd.yaml"),
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
		if crd.Name != "collectormonitors.collectorctrl.io" || len(crd.Spec.Versions) != 1 || crd.Spec.Versions[0].Name != GroupVersion.Version {
			t.Fatalf("%s: unexpected CRD identity %s %+v", p, crd.Name, crd.Spec.Versions)
		}
		schema := crd.Spec.Versions[0].Schema.OpenAPIV3Schema
		status := schema.Properties["status"].Properties["conditions"].Items.Schema.Properties["status"]
		if len(status.Enum) != 3 {
			t.Errorf("%s: condition status enum = %v", p, status.Enum)
		}
		if first == nil {
			first = raw
		} else if !bytes.Equal(first, raw) {
			t.Errorf("%s differs from the Helm chart's CRD; keep them identical", p)
		}
	}
}
