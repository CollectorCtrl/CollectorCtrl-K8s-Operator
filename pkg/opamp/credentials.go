package opamp

import (
	"crypto/md5"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
)

// ApprovedEndpoint binds all monitor credentials to the operator administrator's
// configured destination, including its path. A TLS certificate alone is not a
// grant to receive this operator's credentials.
func ApprovedEndpoint(configured, requested string) (string, error) {
	parse := func(raw string) (string, error) {
		u, err := url.Parse(strings.TrimSpace(raw))
		if err != nil || u.Scheme != "wss" || u.Hostname() == "" || u.User != nil || u.Fragment != "" || u.RawQuery != "" {
			return "", fmt.Errorf("OpAMP endpoint must be an administrator-configured wss URL without credentials, query or fragment")
		}
		u.Host = strings.ToLower(u.Host)
		return u.String(), nil
	}
	approved, err := parse(configured)
	if err != nil {
		return "", err
	}
	if requested == "" {
		return approved, nil
	}
	endpoint, err := parse(requested)
	if err != nil {
		return "", err
	}
	if endpoint != approved {
		return "", fmt.Errorf("monitor opampServer must match the operator's configured OPAMP_SERVER")
	}
	return approved, nil
}

func (c *Client) instanceUID() uuid.UUID { return uuid.UUID(md5.Sum([]byte(c.config.AgentID))) }

func (c *Client) credentialPath() string {
	key := sha256.Sum256([]byte(c.config.Endpoint + "\x00" + c.instanceUID().String()))
	return filepath.Join(c.config.CredentialDirectory, fmt.Sprintf("%x.credential", key))
}

func (c *Client) prepareCredentials() error {
	c.config.EnrollmentToken = strings.TrimSpace(c.config.EnrollmentToken)
	if c.config.CredentialDirectory == "" {
		return fmt.Errorf("persistent credential directory is required")
	}
	if err := os.MkdirAll(c.config.CredentialDirectory, 0700); err != nil {
		return fmt.Errorf("prepare credential storage: %w", err)
	}
	// Fail before enrollment consumes a token if this mount is not writable.
	probe, err := os.CreateTemp(c.config.CredentialDirectory, ".write-probe-*")
	if err != nil {
		return fmt.Errorf("credential storage must be writable: %w", err)
	}
	_ = probe.Close()
	_ = os.Remove(probe.Name())
	payload, err := os.ReadFile(c.credentialPath())
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read persisted credential: %w", err)
	}
	credential := strings.TrimSpace(string(payload))
	if err == nil && !validCredential(credential) {
		return fmt.Errorf("invalid persisted credential; restore its storage or reset enrollment explicitly")
	}
	c.credential = credential
	if credential == "" && c.config.EnrollmentToken != "" && (!strings.HasPrefix(c.config.EnrollmentToken, "cce_") || strings.ContainsAny(c.config.EnrollmentToken, " \t\r\n")) {
		return fmt.Errorf("enrollment-token must contain a cce_ enrollment token")
	}
	if credential == "" && c.config.EnrollmentToken == "" && c.config.LegacySecret == "" {
		return fmt.Errorf("enrollment token is required for a new workload")
	}
	return nil
}

func validCredential(credential string) bool {
	return strings.HasPrefix(credential, "cca_") && !strings.ContainsAny(credential, " \t\r\n")
}

// Persist before acknowledgement: a restart must never fall back from a bound
// (or revoked) identity to the cluster's enrollment token.
func (c *Client) saveCredential(credential string) error {
	if !validCredential(credential) {
		return fmt.Errorf("invalid server credential")
	}
	file, err := os.CreateTemp(c.config.CredentialDirectory, ".credential-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err = file.WriteString(credential + "\n"); err != nil {
		_ = file.Close()
		return err
	}
	if err = file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	if err = os.Rename(file.Name(), c.credentialPath()); err != nil {
		return err
	}
	c.mu.Lock()
	c.credential = credential
	c.authError = nil
	c.mu.Unlock()
	return nil
}

func (c *Client) authHeaders(h http.Header) http.Header {
	c.setConnected(false)
	if h == nil {
		h = make(http.Header)
	}
	h.Set("OpAMP-Instance-UID", c.instanceUID().String())
	c.mu.RLock()
	credential := c.credential
	c.mu.RUnlock()
	switch {
	case credential != "":
		h.Set("Authorization", "Agent "+credential)
	case c.config.EnrollmentToken != "":
		h.Set("Authorization", "Enroll "+c.config.EnrollmentToken)
	default:
		h.Set("Authorization", "Secret-Key "+c.config.LegacySecret)
	}
	return h
}
