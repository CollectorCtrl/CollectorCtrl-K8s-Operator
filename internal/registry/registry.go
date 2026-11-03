// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 CollectorCtrl

// Package registry reads an image's configuration (the Dockerfile ENV) from
// its container registry so the webhook can merge a JAVA_TOOL_OPTIONS baked
// into the image instead of shadowing it. It speaks the OCI distribution API
// directly (two or three small GETs per image), supports anonymous and
// docker-config credentials with the token challenge flow, and caches by
// reference. It never pulls layers.
package registry

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// ImageConfig is the part of an image's config blob the webhook needs.
type ImageConfig struct {
	Digest string   // manifest digest the config was read for
	Env    []string // "KEY=value" entries from config.Env
}

// Lookup returns the value of an environment variable in the image, if set.
func (c ImageConfig) Lookup(name string) (string, bool) {
	for _, kv := range c.Env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == name {
			return v, true
		}
	}
	return "", false
}

// Credentials are the registry credentials for one host.
type Credentials struct{ Username, Password string }

// CredentialSource returns credentials for a registry host; nil means anonymous.
type CredentialSource func(host string) *Credentials

// Reader fetches image configs with an in-memory cache.
type Reader struct {
	HTTP *http.Client
	// TTL bounds how long a result is reused for a tag reference (digest
	// references are immutable and cached for the process lifetime).
	TTL time.Duration

	mu    sync.Mutex
	cache map[string]cacheEntry
	// scheme is https; tests point it at a plain httptest server.
	scheme string
}

type cacheEntry struct {
	cfg     ImageConfig
	err     error
	expires time.Time
}

// NewReader returns a Reader with sane timeouts.
func NewReader() *Reader {
	return &Reader{HTTP: &http.Client{Timeout: 15 * time.Second}, TTL: 10 * time.Minute}
}

// Reference is a parsed image reference.
type Reference struct {
	Host       string // registry host, e.g. registry-1.docker.io
	Repository string // e.g. library/eclipse-temurin
	Tag        string // empty when a digest is given
	Digest     string // sha256:... when given
}

// Parse splits an image reference the way the container runtime does: a
// missing registry means Docker Hub, and a bare name there lives under library/.
func Parse(image string) (Reference, error) {
	image = strings.TrimSpace(image)
	if image == "" {
		return Reference{}, errors.New("empty image reference")
	}
	ref := Reference{}
	if i := strings.Index(image, "@"); i >= 0 {
		ref.Digest = image[i+1:]
		image = image[:i]
		if !strings.HasPrefix(ref.Digest, "sha256:") || len(ref.Digest) != 71 {
			return Reference{}, fmt.Errorf("unsupported digest %q", ref.Digest)
		}
	}
	host, rest := "", image
	if i := strings.Index(image, "/"); i >= 0 {
		first := image[:i]
		if strings.ContainsAny(first, ".:") || first == "localhost" {
			host, rest = first, image[i+1:]
		}
	}
	if i := strings.LastIndex(rest, ":"); i >= 0 && !strings.Contains(rest[i:], "/") {
		ref.Tag, rest = rest[i+1:], rest[:i]
	}
	if host == "" || host == "docker.io" || host == "index.docker.io" {
		host = "registry-1.docker.io"
		if !strings.Contains(rest, "/") {
			rest = "library/" + rest
		}
	}
	if rest == "" || strings.HasPrefix(rest, "/") || strings.HasSuffix(rest, "/") {
		return Reference{}, fmt.Errorf("invalid repository in %q", image)
	}
	if ref.Digest == "" && ref.Tag == "" {
		ref.Tag = "latest"
	}
	ref.Host, ref.Repository = host, rest
	return ref, nil
}

func (r Reference) String() string {
	s := r.Host + "/" + r.Repository
	if r.Tag != "" {
		s += ":" + r.Tag
	}
	if r.Digest != "" {
		s += "@" + r.Digest
	}
	return s
}

func (r Reference) reference() string {
	if r.Digest != "" {
		return r.Digest
	}
	return r.Tag
}

// Media types the reader understands.
const (
	mtOCIIndex      = "application/vnd.oci.image.index.v1+json"
	mtOCIManifest   = "application/vnd.oci.image.manifest.v1+json"
	mtDockerList    = "application/vnd.docker.distribution.manifest.list.v2+json"
	mtDockerV2      = "application/vnd.docker.distribution.manifest.v2+json"
	acceptManifests = mtOCIIndex + ", " + mtOCIManifest + ", " + mtDockerList + ", " + mtDockerV2
)

type descriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
	Platform  *struct {
		OS, Architecture string
	} `json:"platform,omitempty"`
}

type manifest struct {
	MediaType string       `json:"mediaType"`
	Config    descriptor   `json:"config"`
	Manifests []descriptor `json:"manifests"`
}

type configBlob struct {
	Config struct {
		Env []string `json:"Env"`
	} `json:"config"`
}

// maxBlob bounds manifest and config downloads: real ones are a few KB.
const maxBlob = 4 << 20

// Config returns the image config for a reference. Results, including
// failures, are cached: a registry that is down must not slow down every pod
// admission.
func (r *Reader) Config(ctx context.Context, image string, creds CredentialSource) (ImageConfig, error) {
	ref, err := Parse(image)
	if err != nil {
		return ImageConfig{}, err
	}
	key := ref.String()
	r.mu.Lock()
	if r.cache == nil {
		r.cache = map[string]cacheEntry{}
	}
	if e, ok := r.cache[key]; ok && time.Now().Before(e.expires) {
		r.mu.Unlock()
		return e.cfg, e.err
	}
	r.mu.Unlock()

	cfg, err := r.fetch(ctx, ref, creds)
	ttl := r.TTL
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	switch {
	case err != nil:
		ttl = time.Minute // retry failures soon, but not on every admission
	case ref.Digest != "":
		ttl = 24 * time.Hour
	}
	r.mu.Lock()
	if len(r.cache) > 4096 {
		r.cache = map[string]cacheEntry{}
	}
	r.cache[key] = cacheEntry{cfg: cfg, err: err, expires: time.Now().Add(ttl)}
	r.mu.Unlock()
	return cfg, err
}

func (r *Reader) fetch(ctx context.Context, ref Reference, creds CredentialSource) (ImageConfig, error) {
	var cred *Credentials
	if creds != nil {
		cred = creds(ref.Host)
		if cred == nil && ref.Host == "registry-1.docker.io" {
			cred = creds("https://index.docker.io/v1/")
			if cred == nil {
				cred = creds("docker.io")
			}
		}
	}
	scheme := r.scheme
	if scheme == "" {
		scheme = "https"
	}
	base := fmt.Sprintf("%s://%s/v2/%s", scheme, ref.Host, ref.Repository)
	s := &session{reader: r, host: ref.Host, cred: cred}
	body, contentType, err := s.get(ctx, base+"/manifests/"+ref.reference(), acceptManifests)
	if err != nil {
		return ImageConfig{}, err
	}
	var m manifest
	if err := json.Unmarshal(body, &m); err != nil {
		return ImageConfig{}, fmt.Errorf("manifest of %s: %w", ref, err)
	}
	mediaType := m.MediaType
	if mediaType == "" {
		mediaType = strings.Split(contentType, ";")[0]
	}
	digest := ref.Digest
	if mediaType == mtOCIIndex || mediaType == mtDockerList || len(m.Manifests) > 0 {
		d, ok := pickPlatform(m.Manifests)
		if !ok {
			return ImageConfig{}, fmt.Errorf("manifest list of %s has no linux image", ref)
		}
		body, _, err = s.get(ctx, base+"/manifests/"+d.Digest, acceptManifests)
		if err != nil {
			return ImageConfig{}, err
		}
		m = manifest{}
		if err := json.Unmarshal(body, &m); err != nil {
			return ImageConfig{}, fmt.Errorf("platform manifest of %s: %w", ref, err)
		}
		digest = d.Digest
	}
	if m.Config.Digest == "" {
		return ImageConfig{}, fmt.Errorf("manifest of %s has no config descriptor", ref)
	}
	body, _, err = s.get(ctx, base+"/blobs/"+m.Config.Digest, "application/octet-stream, "+m.Config.MediaType)
	if err != nil {
		return ImageConfig{}, err
	}
	var cfg configBlob
	if err := json.Unmarshal(body, &cfg); err != nil {
		return ImageConfig{}, fmt.Errorf("config of %s: %w", ref, err)
	}
	return ImageConfig{Digest: digest, Env: cfg.Config.Env}, nil
}

// pickPlatform prefers linux/amd64, then any linux image: the ENV of an image
// is the same across architectures in practice.
func pickPlatform(list []descriptor) (descriptor, bool) {
	var linux *descriptor
	for i := range list {
		d := &list[i]
		if d.Platform == nil || d.Platform.OS != "linux" {
			continue
		}
		if d.Platform.Architecture == "amd64" {
			return *d, true
		}
		if linux == nil {
			linux = d
		}
	}
	if linux != nil {
		return *linux, true
	}
	return descriptor{}, false
}

// session holds the bearer token for one registry host.
type session struct {
	reader *Reader
	host   string
	cred   *Credentials
	token  string
}

func (s *session) get(ctx context.Context, rawURL, accept string) ([]byte, string, error) {
	for attempt := 0; attempt < 2; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			return nil, "", err
		}
		req.Header.Set("Accept", accept)
		switch {
		case s.token != "":
			req.Header.Set("Authorization", "Bearer "+s.token)
		case s.cred != nil:
			req.SetBasicAuth(s.cred.Username, s.cred.Password)
		}
		resp, err := s.reader.HTTP.Do(req)
		if err != nil {
			return nil, "", fmt.Errorf("registry %s: %w", s.host, err)
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxBlob))
		_ = resp.Body.Close()
		if err != nil {
			return nil, "", err
		}
		switch {
		case resp.StatusCode == http.StatusOK:
			return body, resp.Header.Get("Content-Type"), nil
		case resp.StatusCode == http.StatusUnauthorized && attempt == 0 && s.token == "":
			if err := s.authenticate(ctx, resp.Header.Get("WWW-Authenticate")); err != nil {
				return nil, "", err
			}
			continue
		default:
			return nil, "", fmt.Errorf("registry %s answered %d for %s", s.host, resp.StatusCode, redact(rawURL))
		}
	}
	return nil, "", fmt.Errorf("registry %s: authentication failed", s.host)
}

// authenticate follows a Bearer challenge (realm, service, scope), anonymously
// or with the credentials, and keeps the token for the session. A Basic
// challenge is answered by retrying with basic auth.
func (s *session) authenticate(ctx context.Context, challenge string) error {
	scheme, params, _ := strings.Cut(strings.TrimSpace(challenge), " ")
	switch strings.ToLower(scheme) {
	case "basic":
		// Basic credentials were already sent with the first request.
		if s.cred == nil {
			return fmt.Errorf("registry %s requires credentials", s.host)
		}
		return fmt.Errorf("registry %s rejected the credentials", s.host)
	case "bearer":
	default:
		return fmt.Errorf("registry %s: unsupported challenge %q", s.host, scheme)
	}
	kv := map[string]string{}
	for _, part := range splitChallenge(params) {
		k, v, ok := strings.Cut(part, "=")
		if ok {
			kv[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"`)
		}
	}
	realm := kv["realm"]
	if realm == "" {
		return fmt.Errorf("registry %s: bearer challenge without realm", s.host)
	}
	u, err := url.Parse(realm)
	if err != nil {
		return err
	}
	q := u.Query()
	if kv["service"] != "" {
		q.Set("service", kv["service"])
	}
	if kv["scope"] != "" {
		q.Set("scope", kv["scope"])
	}
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	if s.cred != nil {
		req.SetBasicAuth(s.cred.Username, s.cred.Password)
	}
	resp, err := s.reader.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("token for %s: %w", s.host, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("token endpoint for %s answered %d", s.host, resp.StatusCode)
	}
	var tok struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBlob)).Decode(&tok); err != nil {
		return err
	}
	s.token = tok.Token
	if s.token == "" {
		s.token = tok.AccessToken
	}
	if s.token == "" {
		return fmt.Errorf("token endpoint for %s returned no token", s.host)
	}
	return nil
}

// splitChallenge splits `a="x",b="y,z"` on commas outside quotes.
func splitChallenge(s string) []string {
	var out []string
	var cur strings.Builder
	quoted := false
	for _, r := range s {
		switch {
		case r == '"':
			quoted = !quoted
			cur.WriteRune(r)
		case r == ',' && !quoted:
			out = append(out, cur.String())
			cur.Reset()
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

func redact(rawURL string) string {
	if u, err := url.Parse(rawURL); err == nil {
		return u.Path
	}
	return ""
}

// DockerConfig parses a kubernetes.io/dockerconfigjson payload (or the legacy
// .dockercfg map) into a CredentialSource. Hosts are matched exactly, then
// without scheme and path, so "https://index.docker.io/v1/" also serves
// docker.io lookups.
func DockerConfig(raw []byte) (CredentialSource, error) {
	var full struct {
		Auths map[string]dockerAuth `json:"auths"`
	}
	if err := json.Unmarshal(raw, &full); err != nil {
		return nil, fmt.Errorf("docker config: %w", err)
	}
	auths := full.Auths
	if auths == nil {
		if err := json.Unmarshal(raw, &auths); err != nil {
			return nil, fmt.Errorf("docker config: %w", err)
		}
	}
	byHost := map[string]*Credentials{}
	for host, a := range auths {
		c := &Credentials{Username: a.Username, Password: a.Password}
		if a.Auth != "" {
			dec, err := base64.StdEncoding.DecodeString(a.Auth)
			if err == nil {
				if u, p, ok := strings.Cut(string(dec), ":"); ok {
					c.Username, c.Password = u, p
				}
			}
		}
		if c.Username == "" && c.Password == "" {
			continue
		}
		byHost[host] = c
		byHost[normalizeHost(host)] = c
	}
	return func(host string) *Credentials {
		if c, ok := byHost[host]; ok {
			return c
		}
		return byHost[normalizeHost(host)]
	}, nil
}

type dockerAuth struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Auth     string `json:"auth"`
}

func normalizeHost(h string) string {
	h = strings.TrimPrefix(strings.TrimPrefix(h, "https://"), "http://")
	if i := strings.Index(h, "/"); i >= 0 {
		h = h[:i]
	}
	if h == "index.docker.io" || h == "docker.io" {
		return "registry-1.docker.io"
	}
	return h
}
