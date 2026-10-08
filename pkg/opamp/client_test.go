package opamp

import (
	"crypto/tls"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	opampclient "github.com/open-telemetry/opamp-go/client"
	"github.com/open-telemetry/opamp-go/protobufs"
)

type captureClient struct {
	opampclient.OpAMPClient
	message *protobufs.CustomMessage
	health  *protobufs.ComponentHealth
}

func (c *captureClient) SetHealth(h *protobufs.ComponentHealth) error { c.health = h; return nil }
func (c *captureClient) SendCustomMessage(m *protobufs.CustomMessage) (chan struct{}, error) {
	c.message = m
	return nil, nil
}

func TestTLSVerificationCannotBeDisabled(t *testing.T) {
	cfg, err := TLSConfig("")
	if err != nil || cfg.InsecureSkipVerify || cfg.MinVersion < tls.VersionTLS12 {
		t.Fatalf("unsafe TLS default: %+v %v", cfg, err)
	}
	file := filepath.Join(t.TempDir(), "invalid.pem")
	if err := os.WriteFile(file, []byte("not a certificate"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := TLSConfig(file); err == nil {
		t.Fatal("invalid CA bundle accepted")
	}
}
func TestReadinessIncludesDesiredInstances(t *testing.T) {
	o := Observation{DesiredPods: 3, ReadyPods: 1, Pods: []PodHealth{{Name: "a", Ready: true}}}
	if readinessHealth(o).Healthy {
		t.Fatal("one ready pod incorrectly marked a three-replica workload healthy")
	}
	o.DesiredPods = 0
	o.ReadyPods = 0
	o.Pods = nil
	if readinessHealth(o).Healthy {
		t.Fatal("scaled-to-zero workload marked healthy")
	}
}
func TestObservationUsesCustomSnapshotNotEffectiveConfig(t *testing.T) {
	capture := &captureClient{}
	c := &Client{client: capture, connected: true}
	o := Observation{Source: "kubernetes-api", DesiredPods: 2, ReadyPods: 1, Pods: []PodHealth{{Name: "a", Ready: true}}}
	if err := c.Report(o); err != nil {
		t.Fatal(err)
	}
	if capture.message == nil || capture.message.Type != "k8s_observation_v1" {
		t.Fatal("observation not sent through versioned custom contract")
	}
	var decoded Observation
	if err := json.Unmarshal(capture.message.Data, &decoded); err != nil || decoded.RuntimeVerified || decoded.ConfigYAML != "" {
		t.Fatalf("snapshot misrepresented runtime config: %v", err)
	}
	if capture.health.ComponentHealthMap["fleet/ready_pods"].Status != "1" {
		t.Fatal("server pod count contract mismatch")
	}
}
