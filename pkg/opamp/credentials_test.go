package opamp

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func TestEndpointIsBoundToAdministratorConfiguration(t *testing.T) {
	approved := "wss://control.example/v1/opamp"
	for _, requested := range []string{"wss://other.example/v1/opamp", "wss://control.example/other", "wss://control.example/v1/opamp?redirect=1", "wss://user:pass@control.example/v1/opamp", "ws://control.example/v1/opamp"} {
		if _, err := ApprovedEndpoint(approved, requested); err == nil {
			t.Fatalf("accepted unapproved endpoint %s", requested)
		}
	}
	if _, err := ApprovedEndpoint("", approved); err == nil {
		t.Fatal("monitor supplied its own trust anchor")
	}
	for _, requested := range []string{"", approved, "wss://CONTROL.example/v1/opamp"} {
		if got, err := ApprovedEndpoint(approved, requested); err != nil || got != approved {
			t.Fatalf("approved endpoint rejected: %v", err)
		}
	}
}

func TestPersistedCredentialsSurviveRestartAndRemainEndpointBound(t *testing.T) {
	cfg := ClientConfig{Endpoint: "wss://control.example/v1/opamp", AgentID: "k8s://cluster/workload/collector", EnrollmentToken: "cce_test", CredentialDirectory: t.TempDir()}
	first := NewClient(cfg)
	if err := first.prepareCredentials(); err != nil {
		t.Fatal(err)
	}
	if got := first.authHeaders(nil); got.Get("Authorization") != "Enroll cce_test" || got.Get("OpAMP-Instance-UID") == "" {
		t.Fatal("enrollment identity missing")
	}
	if err := first.saveCredential("cca_persisted"); err != nil {
		t.Fatal(err)
	}
	cfg.EnrollmentToken = ""
	restarted := NewClient(cfg)
	if err := restarted.prepareCredentials(); err != nil {
		t.Fatal(err)
	}
	if got := restarted.authHeaders(http.Header{}).Get("Authorization"); got != "Agent cca_persisted" {
		t.Fatalf("did not use persisted identity: %s", got)
	}
	cfg.Endpoint = "wss://other.example/v1/opamp"
	if err := NewClient(cfg).prepareCredentials(); err == nil {
		t.Fatal("credential reused for another endpoint")
	}
	cfg.Endpoint = first.config.Endpoint
	cfg.AgentID += "-another"
	if err := NewClient(cfg).prepareCredentials(); err == nil {
		t.Fatal("credential reused for another workload")
	}
	// A revoked or rejected credential must never cause token fallback.
	restarted.config.EnrollmentToken = "cce_new"
	restarted.connectionError = os.ErrPermission
	if got := restarted.authHeaders(nil).Get("Authorization"); got != "Agent cca_persisted" {
		t.Fatal("failed identity fell back to enrollment")
	}
}

func TestEnrollmentFailsClosedForUnavailableOrCorruptStorage(t *testing.T) {
	dir := t.TempDir()
	cfg := ClientConfig{Endpoint: "wss://control.example/v1/opamp", AgentID: "observer", EnrollmentToken: "cce_test"}
	if err := NewClient(cfg).Start(context.Background()); err == nil {
		t.Fatal("enrollment allowed without durable storage")
	}
	file := filepath.Join(dir, "file-not-directory")
	if err := os.WriteFile(file, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg.CredentialDirectory = file
	if err := NewClient(cfg).Start(context.Background()); err == nil {
		t.Fatal("enrollment allowed with unwritable storage")
	}
	cfg.CredentialDirectory = dir
	c := NewClient(cfg)
	if err := os.WriteFile(c.credentialPath(), []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := c.prepareCredentials(); err == nil {
		t.Fatal("corrupt identity silently fell back to enrollment")
	}
}
