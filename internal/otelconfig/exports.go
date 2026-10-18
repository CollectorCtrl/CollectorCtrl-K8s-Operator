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
		switch ComponentType(id) {
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
			if e := asString(cfg["endpoint"]); e != "" {
				raws = append(raws, e)
			} else if d := asString(cfg["domain"]); d != "" {
				raws = append(raws, d)
			}
		}
		for _, raw := range raws {
			out = append(out, parseEndpoint(id, raw))
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
