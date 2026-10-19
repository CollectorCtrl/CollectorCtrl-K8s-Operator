package otelconfig

import (
	"net"
	"strings"
)

// Endpoint is one destination an exporter sends data to, as written in the
// configuration. Host is empty when it cannot be determined without runtime
// information (for example an ${env:...} reference).
type Endpoint struct {
	Exporter string
	Raw      string
	Host     string
	Port     string
	// Unresolved is true when the value depends on environment expansion or
	// another runtime source the operator does not resolve.
	Unresolved bool
}

// ExportEndpoints lists the destinations of the exporters used in pipelines.
// It reads the common shapes: `endpoint`, the loadbalancing exporter's
// resolvers, and a few vendor exporters that configure a `domain`.
func (a *Analysis) ExportEndpoints() []Endpoint {
	var out []Endpoint
	for _, id := range a.Exporters {
		cfg := asMap(a.exporterCfg[id])
		var raws []string
		switch normalType(ComponentType(id)) {
		case "loadbalancing":
			resolver := asMap(cfg["resolver"])
			raws = append(raws, asStrings(asMap(resolver["static"])["hostnames"])...)
			if h := asString(asMap(resolver["dns"])["hostname"]); h != "" {
				raws = append(raws, h)
			}
			if svc := asString(asMap(resolver["k8s"])["service"]); svc != "" {
				raws = append(raws, svc)
			}
		default:
			// endpoint, per-signal endpoints (otlphttp), ingest_url (signalfx),
			// then a vendor domain. Duplicate hosts are collapsed below.
			for _, k := range []string{"endpoint", "traces_endpoint", "metrics_endpoint", "logs_endpoint", "ingest_url"} {
				if e := asString(cfg[k]); e != "" {
					raws = append(raws, e)
				}
			}
			if len(raws) == 0 {
				if d := asString(cfg["domain"]); d != "" {
					raws = append(raws, d)
				}
			}
		}
		seen := map[string]bool{}
		for _, raw := range raws {
			e := parseEndpoint(id, raw)
			if k := e.Host + ":" + e.Port; !e.Unresolved && seen[k] {
				continue
			} else {
				seen[k] = true
			}
			out = append(out, e)
		}
	}
	return out
}

func parseEndpoint(exporter, raw string) Endpoint {
	e := Endpoint{Exporter: exporter, Raw: raw}
	if strings.Contains(raw, "${") || strings.Contains(raw, "$(") {
		e.Unresolved = true
		return e
	}
	rest := raw
	if i := strings.Index(rest, "://"); i >= 0 {
		rest = rest[i+3:]
	}
	rest = strings.TrimPrefix(rest, "/") // dns:///host:port
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		rest = rest[:i]
	}
	if i := strings.LastIndex(rest, "@"); i >= 0 {
		rest = rest[i+1:]
	}
	host, port, err := net.SplitHostPort(rest)
	if err != nil {
		host, port = rest, ""
	}
	e.Host, e.Port = strings.ToLower(strings.Trim(host, "[]")), port
	if e.Host == "" {
		e.Unresolved = true
	}
	return e
}
