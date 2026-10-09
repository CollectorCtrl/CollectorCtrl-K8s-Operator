//go:build k8s_integration

// A process boundary for the real server/operator integration test. Credentials
// arrive through stdin, never command-line arguments or diagnostic output.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/collectorctrl/collectorctrl/pkg/opamp"
)

func run() error {
	var cfg struct {
		Endpoint, AgentID, CA, EnrollmentToken, CredentialDirectory string
		Reject                                                      bool
	}
	input := json.NewDecoder(os.Stdin)
	if err := input.Decode(&cfg); err != nil {
		return err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(cfg.CA)) {
		return fmt.Errorf("invalid test CA")
	}
	c := opamp.NewClient(opamp.ClientConfig{
		Endpoint: cfg.Endpoint, AgentID: cfg.AgentID, EnrollmentToken: cfg.EnrollmentToken, CredentialDirectory: cfg.CredentialDirectory,
		TLSConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		Labels:    map[string]string{"collectorctrl.management.mode": "observe", "k8s.cluster.id": "integration-cluster", "k8s.namespace": "observability", "k8s.workload.uid": "workload-uid", "k8s.workload.name": "gateway", "k8s.workload.type": "Deployment", "k8s.container.name": "collector"},
	})
	if err := c.Start(context.Background()); err != nil {
		return err
	}
	defer c.Stop()
	done := make(chan struct{})
	go func() { var command string; _ = input.Decode(&command); close(done) }()
	timer := time.NewTicker(50 * time.Millisecond)
	defer timer.Stop()
	announced := false
	for {
		select {
		case <-done:
			return nil
		case <-timer.C:
			if cfg.Reject && c.ConnectionError() != nil {
				return json.NewEncoder(os.Stdout).Encode(map[string]string{"state": "rejected"})
			}
			if !c.Connected() {
				continue
			}
			if cfg.Reject {
				return fmt.Errorf("revoked identity connected")
			}
			if !announced {
				if err := json.NewEncoder(os.Stdout).Encode(map[string]string{"state": "connected"}); err != nil {
					return err
				}
				announced = true
			}
			o := opamp.Observation{Source: "kubernetes-api", ObservedAt: time.Now().UTC(), ReportIntervalSeconds: 30,
				Namespace: "observability", WorkloadKind: "Deployment", WorkloadName: "gateway", WorkloadUID: "workload-uid", CollectorContainer: "collector",
				ConfigMapName: "gateway-config", ConfigMapKey: "relay.yaml", DesiredPods: 1, ReadyPods: 1,
				Pods: []opamp.PodHealth{{Name: "gateway-0", UID: "pod-uid", Ready: true, Phase: "Running"}},
			}
			if err := c.Report(o); err != nil {
				return err
			}
		}
	}
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
