// internal/validator/otel.go
// Config validation engine for OpenTelemetry Collector YAML.
// Pre-flights configs before they hit Git or the cluster.

package validator

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// Result contains the outcome of a validation run.
type Result struct {
	Valid   bool     `json:"valid"`
	Errors  []Error  `json:"errors,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
}

// Error is a structured validation error.
type Error struct {
	Path    string `json:"path"`    // e.g., "service.pipelines.metrics.processors.1"
	Message string `json:"message"` // human-readable error
	Code    string `json:"code"`    // e.g., "UNKNOWN_COMPONENT", "INVALID_ENDPOINT"
}

// Validator checks OTel Collector configurations for correctness.
type Validator struct {
	// knownComponents is a registry of valid receiver/processor/exporter/extension names.
	// In production, this should be loaded from the collector's component registry.
	knownComponents map[string]bool
}

// NewValidator creates a validator with a component registry.
func NewValidator() *Validator {
	return &Validator{
		knownComponents: defaultKnownComponents(),
	}
}

// Validate parses and checks an OTel Collector config YAML.
func (v *Validator) Validate(configYAML string) *Result {
	result := &Result{Valid: true}

	// 1. Basic YAML parse
	var root map[string]interface{}
	if err := yaml.Unmarshal([]byte(configYAML), &root); err != nil {
		result.Valid = false
		result.Errors = append(result.Errors, Error{
			Path:    "",
			Message: fmt.Sprintf("YAML parse error: %v", err),
			Code:    "YAML_PARSE_ERROR",
		})
		return result
	}

	// 2. Validate top-level sections
	for section, required := range map[string]bool{
		"receivers":  true,
		"exporters":  true,
		"service":    true,
		"processors": false,
		"extensions": false,
	} {
		if _, ok := root[section]; !ok && required {
			result.Valid = false
			result.Errors = append(result.Errors, Error{
				Path:    section,
				Message: fmt.Sprintf("missing required section: %s", section),
				Code:    "MISSING_SECTION",
			})
		}
	}

	// 3. Validate pipelines reference known components
	if svc, ok := root["service"].(map[string]interface{}); ok {
		if pipelines, ok := svc["pipelines"].(map[string]interface{}); ok {
			for pipelineName, pipeline := range pipelines {
				v.validatePipeline(result, fmt.Sprintf("service.pipelines.%s", pipelineName), pipeline)
			}
		}
	}

	// 4. Check for unknown components
	for _, section := range []string{"receivers", "processors", "exporters", "extensions"} {
		if components, ok := root[section].(map[string]interface{}); ok {
			for name := range components {
				componentName := strings.Split(name, "/")[0] // strip named variant
				if !v.knownComponents[componentName] {
					result.Warnings = append(result.Warnings,
						fmt.Sprintf("Unknown component in %s: %s", section, componentName))
				}
			}
		}
	}

	return result
}

func (v *Validator) validatePipeline(result *Result, path string, pipeline interface{}) {
	p, ok := pipeline.(map[string]interface{})
	if !ok {
		result.Valid = false
		result.Errors = append(result.Errors, Error{
			Path:    path,
			Message: "pipeline must be a map",
			Code:    "INVALID_PIPELINE_TYPE",
		})
		return
	}

	for _, stage := range []string{"receivers", "processors", "exporters"} {
		items, ok := p[stage].([]interface{})
		if !ok && stage != "processors" {
			result.Valid = false
			result.Errors = append(result.Errors, Error{
				Path:    fmt.Sprintf("%s.%s", path, stage),
				Message: fmt.Sprintf("%s must be a list", stage),
				Code:    "INVALID_STAGE_TYPE",
			})
			continue
		}

		for i, item := range items {
			name, ok := item.(string)
			if !ok {
				result.Valid = false
				result.Errors = append(result.Errors, Error{
					Path:    fmt.Sprintf("%s.%s.%d", path, stage, i),
					Message: "component name must be a string",
					Code:    "INVALID_COMPONENT_NAME",
				})
				continue
			}

			componentName := strings.Split(name, "/")[0]
			if !v.knownComponents[componentName] {
				result.Warnings = append(result.Warnings,
					fmt.Sprintf("Unknown component referenced in pipeline: %s", componentName))
			}
		}
	}
}

// defaultKnownComponents returns a starter registry.
// In production, load this from the collector's actual component metadata.
func defaultKnownComponents() map[string]bool {
	return map[string]bool{
		// Receivers
		"otlp": true, "prometheus": true, "jaeger": true, "zipkin": true,
		"hostmetrics": true, "filelog": true, "kubeletstats": true,
		"k8scluster": true, "k8sobjects": true, "k8sevents": true,

		// Processors
		"batch": true, "memory_limiter": true, "resource": true,
		"filter": true, "attributes": true, "k8sattributes": true,
		"transform": true, "tail_sampling": true, "group_by_attrs": true,

		// Exporters
		"debug": true, "otlphttp": true,
		"splunk_hec": true, "splunk_hec_logs": true,
		"prometheusremotewrite": true, "loki": true, "datadog": true,

		// Extensions
		"health_check": true, "pprof": true, "zpages": true,
		"opamp": true, "storage": true, "file_storage": true,
	}
}
