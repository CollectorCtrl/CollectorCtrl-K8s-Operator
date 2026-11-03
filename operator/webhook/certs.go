// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 CollectorCtrl

package webhook

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// Certificate file names controller-runtime's webhook server expects.
const (
	CertFile = "tls.crt"
	KeyFile  = "tls.key"
)

// GenerateCertificates writes a fresh self-signed CA and a serving
// certificate for the webhook Service into dir and returns the CA bundle.
// The operator regenerates them at every start (one replica, no shared
// state) and publishes the CA in its webhook configuration.
func GenerateCertificates(dir string, dnsNames []string, validity time.Duration) ([]byte, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	now := time.Now()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: "collectorctrl-operator-webhook-ca", Organization: []string{"CollectorCtrl"}}, //nolint:misspell // Go's field name
		NotBefore:             now.Add(-5 * time.Minute),
		NotAfter:              now.Add(validity),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, err
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		return nil, err
	}
	srvKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	srvTmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: dnsNames[0]},
		NotBefore:    now.Add(-5 * time.Minute),
		NotAfter:     now.Add(validity),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     dnsNames,
	}
	srvDER, err := x509.CreateCertificate(rand.Reader, srvTmpl, caCert, &srvKey.PublicKey, caKey)
	if err != nil {
		return nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(srvKey)
	if err != nil {
		return nil, err
	}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srvDER})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(filepath.Join(dir, CertFile), certPEM, 0o600); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, KeyFile), keyPEM, 0o600); err != nil {
		return nil, err
	}
	return caPEM, nil
}

func serial() *big.Int {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return big.NewInt(time.Now().UnixNano())
	}
	return n
}

// CABundlePublisher keeps the CA bundle in the operator's
// MutatingWebhookConfiguration equal to the serving CA. It retries until the
// configuration exists (Helm may create it after the pod) and re-checks
// periodically, because a chart upgrade rewrites the configuration without
// the bundle.
type CABundlePublisher struct {
	// Reader reads the configuration uncached: the operator may list but not
	// watch webhook configurations, so the cached client cannot be used.
	Reader   client.Reader
	Client   client.Client
	Name     string
	CABundle []byte
	Interval time.Duration
}

// NeedLeaderElection: the webhook server runs on every replica, so does this.
func (p *CABundlePublisher) NeedLeaderElection() bool { return false }

// Start implements manager.Runnable.
func (p *CABundlePublisher) Start(ctx context.Context) error {
	log := logf.FromContext(ctx).WithName("webhook-ca")
	interval := p.Interval
	if interval <= 0 {
		interval = time.Minute
	}
	retry := 5 * time.Second
	for {
		changed, err := p.Publish(ctx)
		wait := interval
		switch {
		case err != nil:
			log.Info("webhook configuration not updated yet", "name", p.Name, "error", err.Error())
			wait = retry
		case changed:
			log.Info("webhook CA bundle published", "name", p.Name)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
	}
}

// Publish writes the CA bundle into every webhook of the configuration.
func (p *CABundlePublisher) Publish(ctx context.Context) (bool, error) {
	cfg := &admissionv1.MutatingWebhookConfiguration{}
	reader := p.Reader
	if reader == nil {
		reader = p.Client
	}
	if err := reader.Get(ctx, client.ObjectKey{Name: p.Name}, cfg); err != nil {
		return false, err
	}
	changed := false
	for i := range cfg.Webhooks {
		if !bytes.Equal(cfg.Webhooks[i].ClientConfig.CABundle, p.CABundle) {
			cfg.Webhooks[i].ClientConfig.CABundle = append([]byte(nil), p.CABundle...)
			changed = true
		}
	}
	if !changed {
		return false, nil
	}
	if err := p.Client.Update(ctx, cfg); err != nil {
		return false, fmt.Errorf("update %s: %w", p.Name, err)
	}
	return true, nil
}
