// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 CollectorCtrl

package registry

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestParse(t *testing.T) {
	cases := map[string]string{
		"eclipse-temurin":        "registry-1.docker.io/library/eclipse-temurin:latest",
		"eclipse-temurin:21-jre": "registry-1.docker.io/library/eclipse-temurin:21-jre",
		"acme/orders:2.1":        "registry-1.docker.io/acme/orders:2.1",
		"docker.io/acme/orders":  "registry-1.docker.io/acme/orders:latest",
		"ghcr.io/acme/orders:v2": "ghcr.io/acme/orders:v2",
		"localhost:5000/orders":  "localhost:5000/orders:latest",
		"registry.corp:443/team/app@sha256:" + strings.Repeat("a", 64): "registry.corp:443/team/app@sha256:" + strings.Repeat("a", 64),
		"ghcr.io/acme/orders:v2@sha256:" + strings.Repeat("b", 64):     "ghcr.io/acme/orders:v2@sha256:" + strings.Repeat("b", 64),
	}
	for in, want := range cases {
		ref, err := Parse(in)
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if ref.String() != want {
			t.Errorf("%s: got %s want %s", in, ref, want)
		}
	}
	for _, bad := range []string{"", "ghcr.io/", "app@sha256:short"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

// fakeRegistry serves a multi-arch index, a per-platform manifest and a config
// blob, behind a bearer token challenge like Docker Hub and GHCR use.
func fakeRegistry(t *testing.T, requireAuth bool) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if requireAuth {
			if u, p, ok := r.BasicAuth(); !ok || u != "robot" || p != "s3cret" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
		}
		if r.URL.Query().Get("scope") != "repository:acme/orders:pull" {
			t.Errorf("scope = %q", r.URL.Query().Get("scope"))
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"token": "tok-123"})
	})
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("Authorization") != "Bearer tok-123" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="http://`+r.Host+`/token",service="fake",scope="repository:acme/orders:pull"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Header.Get("Authorization") != "" && r.URL.Path == "/token" {
			return
		}
		switch r.URL.Path {
		case "/v2/acme/orders/manifests/2.1":
			w.Header().Set("Content-Type", mtOCIIndex)
			_, _ = w.Write([]byte(`{"mediaType":"` + mtOCIIndex + `","manifests":[
			  {"mediaType":"` + mtOCIManifest + `","digest":"sha256:arm","platform":{"os":"linux","architecture":"arm64"}},
			  {"mediaType":"` + mtOCIManifest + `","digest":"sha256:amd","platform":{"os":"linux","architecture":"amd64"}},
			  {"mediaType":"` + mtOCIManifest + `","digest":"sha256:att","platform":{"os":"unknown","architecture":"unknown"}}]}`))
		case "/v2/acme/orders/manifests/sha256:amd":
			w.Header().Set("Content-Type", mtOCIManifest)
			_, _ = w.Write([]byte(`{"mediaType":"` + mtOCIManifest + `","config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"sha256:cfg"}}`))
		case "/v2/acme/orders/manifests/single":
			w.Header().Set("Content-Type", mtDockerV2)
			_, _ = w.Write([]byte(`{"config":{"digest":"sha256:cfg"}}`))
		case "/v2/acme/orders/blobs/sha256:cfg":
			_, _ = w.Write([]byte(`{"config":{"Env":["PATH=/usr/bin","JAVA_TOOL_OPTIONS=-Xmx512m -XX:+UseG1GC","JAVA_HOME=/opt/java"]}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &hits
}

func newTestReader(srv *httptest.Server) *Reader {
	r := NewReader()
	r.scheme = "http"
	return r
}

func TestConfigFollowsIndexAndTokenChallenge(t *testing.T) {
	srv, hits := fakeRegistry(t, false)
	host := strings.TrimPrefix(srv.URL, "http://")
	r := newTestReader(srv)
	cfg, err := r.Config(context.Background(), host+"/acme/orders:2.1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Digest != "sha256:amd" {
		t.Errorf("digest = %s", cfg.Digest)
	}
	if v, ok := cfg.Lookup("JAVA_TOOL_OPTIONS"); !ok || v != "-Xmx512m -XX:+UseG1GC" {
		t.Errorf("JAVA_TOOL_OPTIONS = %q %v", v, ok)
	}
	if _, ok := cfg.Lookup("NODE_OPTIONS"); ok {
		t.Error("NODE_OPTIONS should be absent")
	}
	before := hits.Load()
	if _, err := r.Config(context.Background(), host+"/acme/orders:2.1", nil); err != nil {
		t.Fatal(err)
	}
	if hits.Load() != before {
		t.Error("second lookup was not served from the cache")
	}
	// Single-platform Docker v2 manifest.
	cfg, err = r.Config(context.Background(), host+"/acme/orders:single", nil)
	if err != nil || len(cfg.Env) != 3 {
		t.Fatalf("single manifest: %v %+v", err, cfg)
	}
}

func TestConfigUsesDockerConfigCredentials(t *testing.T) {
	srv, _ := fakeRegistry(t, true)
	host := strings.TrimPrefix(srv.URL, "http://")
	r := newTestReader(srv)
	if _, err := r.Config(context.Background(), host+"/acme/orders:2.1", nil); err == nil {
		t.Fatal("anonymous access should fail")
	}
	creds, err := DockerConfig([]byte(`{"auths":{"` + host + `":{"auth":"cm9ib3Q6czNjcmV0"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	r2 := newTestReader(srv)
	if _, err := r2.Config(context.Background(), host+"/acme/orders:2.1", creds); err != nil {
		t.Fatal(err)
	}
}

func TestFailuresAreCachedBriefly(t *testing.T) {
	srv, hits := fakeRegistry(t, false)
	host := strings.TrimPrefix(srv.URL, "http://")
	r := newTestReader(srv)
	_, err := r.Config(context.Background(), host+"/acme/orders:missing", nil)
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("err = %v", err)
	}
	n := hits.Load()
	_, _ = r.Config(context.Background(), host+"/acme/orders:missing", nil)
	if hits.Load() != n {
		t.Error("failure was not cached")
	}
}

func TestDockerConfigHostMatching(t *testing.T) {
	src, err := DockerConfig([]byte(`{"auths":{"https://index.docker.io/v1/":{"username":"u","password":"p"},"ghcr.io":{"auth":"bm90OmJhc2U2NA=="}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if c := src("registry-1.docker.io"); c == nil || c.Username != "u" {
		t.Errorf("docker hub credentials not found: %+v", c)
	}
	if c := src("ghcr.io"); c == nil || c.Username != "not" || c.Password != "base64" {
		t.Errorf("ghcr credentials: %+v", c)
	}
	if src("quay.io") != nil {
		t.Error("quay should be anonymous")
	}
	if _, err := DockerConfig([]byte("{")); err == nil {
		t.Error("invalid JSON accepted")
	}
}
