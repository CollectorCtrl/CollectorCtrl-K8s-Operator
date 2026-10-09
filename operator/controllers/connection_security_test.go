package controllers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	v1 "github.com/collectorctrl/collectorctrl/operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestMonitorCannotRedirectOperatorCredential(t *testing.T) {
	var requests atomic.Int32
	endpoint := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) { requests.Add(1); w.WriteHeader(403) }))
	defer endpoint.Close()
	r := fixture(t)
	r.DefaultServer = "wss://approved.example/v1/opamp"
	r.EnrollmentToken = "cce_dummy"
	r.ClusterID = "cluster"
	r.CredentialDirectory = t.TempDir()
	r.TLSConfig = endpoint.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	m := monitor()
	m.Spec.OpAMPServer = "wss" + strings.TrimPrefix(endpoint.URL, "https")
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "config", Namespace: "obs"}}
	if c, err := r.ensureConnection(context.Background(), m, workload(), "Deployment", "collector", cm); err == nil {
		c.Stop()
		t.Fatal("monitor redirected operator credential")
	}
	if requests.Load() != 0 {
		t.Fatal("unapproved endpoint received a request")
	}
}

func TestMonitorCannotSelectAnotherNamespacesSecret(t *testing.T) {
	r := fixture(t, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "allowed-name", Namespace: "other"}, Data: map[string][]byte{"enrollment-token": []byte("cce_dummy")}})
	r.DefaultServer = "wss://approved.example/v1/opamp"
	r.ClusterID = "cluster"
	m := monitor()
	m.Spec.Auth.SecretRef = &v1.SecretRef{Name: "allowed-name", Namespace: "other"}
	if _, err := r.ensureConnection(context.Background(), m, workload(), "Deployment", "collector", &corev1.ConfigMap{}); err == nil || !strings.Contains(err.Error(), "namespace") {
		t.Fatalf("cross-namespace auth reference accepted: %v", err)
	}
}

func TestOneMonitorOwnsEachCredentialIdentity(t *testing.T) {
	r := fixture(t)
	r.DefaultServer = "wss://approved.example/v1/opamp"
	r.ClusterID = "cluster"
	r.connections = map[string]*connection{"obs/first": {identity: "k8s://cluster/workload/collector"}}
	m := monitor()
	m.Name = "duplicate"
	if _, err := r.ensureConnection(context.Background(), m, workload(), "Deployment", "collector", &corev1.ConfigMap{}); err == nil || !strings.Contains(err.Error(), "already observed") {
		t.Fatalf("duplicate identity accepted: %v", err)
	}
}
