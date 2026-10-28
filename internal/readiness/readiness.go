// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 CollectorCtrl

// Package readiness works out, from Kubernetes objects alone, whether an
// application container could take an OpenTelemetry auto-instrumentation agent:
// which language it runs, which APM agents (any vendor) are already loaded or
// requested, which admission webhooks would inject into it, and which launcher
// variables (JAVA_TOOL_OPTIONS, NODE_OPTIONS, ...) an injector would have to
// merge with.
//
// It is read-only inference. It never contacts a registry, so variables baked
// into the image (Dockerfile ENV) are invisible and every result says so. It
// never reads Secrets: values that come from Secrets are reported as
// unresolved. Launcher values are reduced to the agent-loading entries
// (-javaagent paths, --require modules, profiler IDs); other flags are counted,
// never copied, because they can carry credentials.
package readiness

import (
	"sort"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// Languages.
const (
	LangJava    = "java"
	LangNode    = "nodejs"
	LangPython  = "python"
	LangDotNet  = "dotnet"
	LangGo      = "go"
	LangNative  = "native" // agent mechanism works for any language (LD_PRELOAD, OneAgent)
	LangUnknown = "unknown"
)

// Verdicts, from most to least in need of attention.
const (
	VerdictConflict     = "conflict"     // more than one APM vendor would run in the process
	VerdictReview       = "review"       // could be instrumented, but something must be merged or confirmed first
	VerdictReady        = "ready"        // supported language, nothing in the way that Kubernetes shows
	VerdictUnknown      = "unknown"      // language could not be determined from the manifest
	VerdictUnsupported  = "unsupported"  // language has no OTel auto-instrumentation path here (Go, ...)
	VerdictInstrumented = "instrumented" // exactly one APM agent already loaded
)

var verdictRank = map[string]int{
	VerdictConflict: 6, VerdictReview: 5, VerdictReady: 4, VerdictUnknown: 3, VerdictUnsupported: 2, VerdictInstrumented: 1,
}

// Worst returns the verdict that needs the most attention.
func Worst(verdicts ...string) string {
	out := ""
	for _, v := range verdicts {
		if verdictRank[v] > verdictRank[out] {
			out = v
		}
	}
	return out
}

// Agent is an APM agent that is loaded, or will be loaded, in a container.
type Agent struct {
	Vendor    string `json:"vendor"`    // opentelemetry, splunk, datadog, newrelic, appdynamics, dynatrace, elastic, instana, unknown
	Language  string `json:"language"`  // java, nodejs, python, dotnet, native
	Mechanism string `json:"mechanism"` // javaagent, node-require, python-wrapper, python-path, dotnet-profiler, dotnet-startup-hook, ld-preload, init-container, injected-annotation
	Evidence  string `json:"evidence"`  // what was seen, without secret-bearing values
	// Injected is true when the agent is in the running pod but not in the
	// workload's pod template, i.e. an admission webhook added it.
	Injected bool `json:"injected,omitempty"`
}

// Request is an instrumentation request that an injector acts on, such as the
// upstream OTel Operator's inject annotation. A request is not proof an agent runs.
type Request struct {
	Vendor   string `json:"vendor"`
	Language string `json:"language,omitempty"`
	Via      string `json:"via"` // e.g. "pod annotation instrumentation.opentelemetry.io/inject-java"
}

// LauncherVar is an environment variable the runtime reads to load agents.
type LauncherVar struct {
	Name     string `json:"name"`
	Source   string `json:"source"` // literal, configmap, secret, field, resource, envfrom-configmap, envfrom-secret
	Resolved bool   `json:"resolved"`
	// Entries are the agent-loading parts only: -javaagent:/path, --require mod,
	// profiler GUIDs, preload paths. Options after "=" are dropped.
	Entries    []string `json:"entries,omitempty"`
	OtherFlags int      `json:"other_flags,omitempty"` // count of entries not copied
	InPod      bool     `json:"in_pod,omitempty"`      // set in the running pod only (added by a webhook)
}

// VendorEnv counts vendor configuration variables, e.g. DD_* without a loader.
type VendorEnv struct {
	Vendor string `json:"vendor"`
	Prefix string `json:"prefix"`
	Count  int    `json:"count"`
}

// Container is the readiness result for one application container.
type Container struct {
	Name           string        `json:"name"`
	Image          string        `json:"image"`
	Language       string        `json:"language"`
	LanguageSource string        `json:"language_source,omitempty"` // agent, command, env, image
	Agents         []Agent       `json:"agents,omitempty"`
	Requests       []Request     `json:"requests,omitempty"`
	SelectedBy     []string      `json:"selected_by,omitempty"` // injector names whose webhook selects this pod
	Launcher       []LauncherVar `json:"launcher,omitempty"`
	VendorEnv      []VendorEnv   `json:"vendor_env,omitempty"`
	Verdict        string        `json:"verdict"`
	Findings       []Finding     `json:"findings"`
	Reasons        []string      `json:"reasons"`
}

// Lookup resolves a ConfigMap key in the workload's namespace. Secrets are
// never looked up.
type Lookup func(configMap, key string) (string, bool)

// LookupAll returns all keys of a ConfigMap, for envFrom.
type LookupAll func(configMap string) (map[string]string, bool)

// Input is everything known about one workload.
type Input struct {
	Template corev1.PodTemplateSpec
	// Pod is a running pod of the workload, if any. Webhook-injected agents
	// are only visible here.
	Pod *corev1.Pod
	// NamespaceLabels and NamespaceAnnotations of the workload's namespace.
	NamespaceLabels      map[string]string
	NamespaceAnnotations map[string]string
	// WorkloadAnnotations are the workload's own annotations (not the pod
	// template's), for overrides such as collectorctrl.io/language.
	WorkloadAnnotations map[string]string
	// Injectors are the pod-mutating webhooks found in the cluster.
	Injectors []Injector
	// InjectorsChecked is false when webhooks could not be listed.
	InjectorsChecked bool
	Lookup           Lookup
	LookupAll        LookupAll
}

// AnnotationLanguage states a workload's language when it cannot be
// inferred: "java", or per container "app=java,worker=python".
const AnnotationLanguage = "collectorctrl.io/language"

func languageOverride(ann map[string]string, container string) (string, bool) {
	raw := strings.TrimSpace(ann[AnnotationLanguage])
	if raw == "" {
		return "", false
	}
	valid := func(l string) bool {
		l = strings.ToLower(strings.TrimSpace(l))
		return supported(l) || l == LangGo
	}
	if !strings.Contains(raw, "=") {
		if valid(raw) {
			return strings.ToLower(raw), true
		}
		return "", false
	}
	for _, part := range strings.Split(raw, ",") {
		name, lang, ok := strings.Cut(part, "=")
		if ok && strings.TrimSpace(name) == container && valid(lang) {
			return strings.ToLower(strings.TrimSpace(lang)), true
		}
	}
	return "", false
}

// NotApplication reports whether an image is infrastructure rather than an
// application: collectors, service-mesh proxies, log shippers and similar
// sidecars are left out of readiness.
func NotApplication(image string) bool {
	repo := repository(image)
	for _, hint := range infraImageHints {
		if strings.Contains(repo, hint) {
			return true
		}
	}
	for _, suffix := range infraImageSuffixes {
		if strings.HasSuffix(repo, suffix) {
			return true
		}
	}
	return false
}

var infraImageSuffixes = []string{"/pause", "/envoy", "/vector"}

var infraImageHints = []string{
	"opentelemetry-collector", "otelcol", "otel-collector", "aws-otel-collector", "grafana/alloy",
	"istio/proxy", "/proxyv2", "linkerd2-proxy", "linkerd-proxy", "envoyproxy/",
	"fluent-bit", "fluentbit", "fluentd", "timberio/vector", "beats/filebeat", "grafana/promtail",
	"hashicorp/vault", "vault-agent", "cloud-sql-proxy", "cloudsql-proxy",
	"datadog/agent", "dynatrace/oneagent", "newrelic/infrastructure", "instana/agent",
	"kube-rbac-proxy", "oauth2-proxy",
	"opentelemetry-operator/autoinstrumentation", // OTel Operator's init images
}

// repository returns the lower-cased image repository without tag or digest.
func repository(image string) string {
	repo := strings.ToLower(image)
	if i := strings.Index(repo, "@"); i >= 0 {
		repo = repo[:i]
	}
	if i := strings.LastIndex(repo, ":"); i > strings.LastIndex(repo, "/") {
		repo = repo[:i]
	}
	return repo
}

// Analyze returns one result per application container of the template.
func Analyze(in Input) []Container {
	var out []Container
	for _, c := range in.Template.Spec.Containers {
		if NotApplication(c.Image) {
			continue
		}
		out = append(out, analyzeContainer(in, c))
	}
	return out
}

func analyzeContainer(in Input, c corev1.Container) Container {
	res := Container{Name: c.Name, Image: c.Image, Reasons: []string{}}
	tmpl := collect(c, &in.Template.Spec, in.Template.Labels, in.Template.Annotations, in.Lookup, in.LookupAll)
	var live *evidence
	if in.Pod != nil {
		for _, pc := range in.Pod.Spec.Containers {
			if pc.Name == c.Name {
				live = collect(pc, &in.Pod.Spec, in.Pod.Labels, in.Pod.Annotations, in.Lookup, in.LookupAll)
				break
			}
		}
	}

	// Agents: everything in the template, plus what only the running pod shows.
	res.Agents = detectAgents(tmpl)
	if live != nil {
		seen := map[string]bool{}
		for _, a := range res.Agents {
			seen[a.Vendor+"/"+a.Language] = true
		}
		for _, a := range detectAgents(live) {
			if !seen[a.Vendor+"/"+a.Language] {
				a.Injected = true
				res.Agents = append(res.Agents, a)
				seen[a.Vendor+"/"+a.Language] = true
			}
		}
	}
	res.Requests = detectRequests(tmpl, in.NamespaceAnnotations)

	// Launcher variables, from the template; flag those only the pod has.
	res.Launcher = launcherVars(tmpl)
	if live != nil {
		have := map[string]bool{}
		for _, v := range res.Launcher {
			have[v.Name] = true
		}
		for _, v := range launcherVars(live) {
			if !have[v.Name] {
				v.InPod = true
				res.Launcher = append(res.Launcher, v)
			}
		}
	}
	res.VendorEnv = vendorEnv(tmpl)

	res.Language, res.LanguageSource = detectLanguage(c, tmpl, res.Agents)
	if lang, ok := languageOverride(in.WorkloadAnnotations, c.Name); ok {
		res.Language, res.LanguageSource = lang, "annotation"
	}

	if in.InjectorsChecked {
		podLabels := in.Template.Labels
		for _, inj := range in.Injectors {
			if inj.Selects(in.NamespaceLabels, podLabels, res.Requests) {
				res.SelectedBy = append(res.SelectedBy, inj.Name)
			}
		}
		sort.Strings(res.SelectedBy)
	}

	decide(&res, in, live != nil)
	return res
}

func vendorNames(agents []Agent) []string {
	set := map[string]bool{}
	for _, a := range agents {
		set[a.Vendor] = true
	}
	out := make([]string, 0, len(set))
	for v := range set {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

func supported(lang string) bool {
	switch lang {
	case LangJava, LangNode, LangPython, LangDotNet:
		return true
	}
	return false
}

// Finding severities.
const (
	SeverityBlocker = "blocker" // must be resolved before any instrumentation
	SeverityWarning = "warning" // decide or confirm before instrumenting
	SeverityInfo    = "info"    // context; nothing to do
)

// Finding codes are a stable, machine-readable contract: the server maps them
// to next steps, and UIs and AI agents act on them without parsing Message.
// New codes may be added; existing codes never change meaning.
const (
	CodeMultipleVendors     = "multiple_vendors"         // blocker; subject: vendors, comma-separated
	CodeInjectorAddsVendor  = "injector_adds_vendor"     // blocker; subject: webhook configuration name
	CodeUnrecognisedAgent   = "unrecognised_agent"       // warning; subject: evidence
	CodeInjectorSelects     = "injector_selects"         // warning; subject: webhook configuration name
	CodeInjectionRequested  = "injection_requested"      // warning; subject: annotation or label
	CodeInjectionNotApplied = "injection_not_applied"    // warning; subject: annotation or label
	CodeVendorEnvNoAgent    = "vendor_env_without_agent" // warning; subject: variable prefix
	CodeLauncherSet         = "launcher_set"             // warning; subject: variable name
	CodeLauncherUnreadable  = "launcher_unreadable"      // warning; subject: variable name
	CodeEnvFromSecretUnread = "envfrom_secret_unread"    // warning; subject: Secret name
	CodeAgentLoaded         = "agent_loaded"             // info; subject: vendor
	CodeLanguageUnknown     = "language_unknown"         // info
	CodeLanguageUnsupported = "language_unsupported"     // info; subject: language
	CodeJavaOptsIgnored     = "java_opts_ignored"        // info
	CodeImageEnvNotChecked  = "image_env_not_checked"    // info
	CodeNoRunningPod        = "no_running_pod"           // info
	CodeInjectorsNotChecked = "injectors_not_checked"    // info
)

// Finding is one fact behind a verdict.
type Finding struct {
	Code     string `json:"code"`
	Severity string `json:"severity"`
	Subject  string `json:"subject,omitempty"`
	Message  string `json:"message"`
}

// decide sets the verdict and the findings behind it. Reasons mirrors the
// finding messages for readers of older payloads.
func decide(res *Container, in Input, sawPod bool) {
	var findings []Finding
	add := func(code, severity, subject, message string) {
		findings = append(findings, Finding{Code: code, Severity: severity, Subject: subject, Message: message})
	}

	vendors := vendorNames(res.Agents)
	for _, a := range res.Agents {
		how := a.Evidence
		if a.Injected {
			how += ", added by an admission webhook"
		}
		add(CodeAgentLoaded, SeverityInfo, a.Vendor, vendorTitle(a.Vendor)+" "+langTitle(a.Language)+" agent ("+how+")")
	}

	// Vendors an injector would add at the next pod start.
	pending := map[string]string{}
	for _, name := range res.SelectedBy {
		for _, inj := range in.Injectors {
			if inj.Name != name {
				continue
			}
			loaded := false
			for _, v := range vendors {
				loaded = loaded || SameFamily(v, inj.Vendor)
			}
			if !loaded {
				pending[inj.Vendor] = name
			}
		}
	}
	pendingVendors := make([]string, 0, len(pending))
	for v := range pending {
		pendingVendors = append(pendingVendors, v)
	}
	sort.Strings(pendingVendors)

	verdict := ""
	switch {
	case len(vendors) > 1:
		verdict = VerdictConflict
		titles := make([]string, len(vendors))
		for i, v := range vendors {
			titles[i] = vendorTitle(v)
		}
		add(CodeMultipleVendors, SeverityBlocker, strings.Join(vendors, ","), strings.Join(titles, " and ")+" agents are both loaded in one process")
	case len(vendors) == 1 && len(pending) > 0:
		verdict = VerdictConflict
		for _, v := range pendingVendors {
			add(CodeInjectorAddsVendor, SeverityBlocker, pending[v], vendorTitle(v)+"'s injector ("+pending[v]+") would add a second agent at the next restart")
		}
	case len(vendors) == 1:
		verdict = VerdictInstrumented
		if vendors[0] == "unknown" {
			verdict = VerdictReview
			add(CodeUnrecognisedAgent, SeverityWarning, res.Agents[0].Evidence, "An agent is loaded but its vendor is not recognised")
		}
	}

	if verdict == "" {
		switch {
		case res.Language == LangUnknown:
			verdict = VerdictUnknown
			add(CodeLanguageUnknown, SeverityInfo, "", "Language not recognisable from the command, environment or image name")
		case !supported(res.Language):
			verdict = VerdictUnsupported
			add(CodeLanguageUnsupported, SeverityInfo, res.Language, langTitle(res.Language)+" has no auto-instrumentation path; use the OpenTelemetry SDK")
		default:
			verdict = VerdictReady
		}
		if verdict == VerdictReady || verdict == VerdictUnknown {
			for _, v := range pendingVendors {
				verdict = VerdictReview
				add(CodeInjectorSelects, SeverityWarning, pending[v], vendorTitle(v)+"'s injector ("+pending[v]+") will instrument new pods here")
			}
			for _, r := range res.Requests {
				verdict = VerdictReview
				if sawPod {
					add(CodeInjectionNotApplied, SeverityWarning, r.Via, vendorTitle(r.Vendor)+" injection is requested but the running pod has no agent")
				} else {
					add(CodeInjectionRequested, SeverityWarning, r.Via, vendorTitle(r.Vendor)+" injection is requested")
				}
			}
			for _, e := range res.VendorEnv {
				if e.Vendor == "opentelemetry" {
					continue // OTEL_* also configure SDKs and collectors; not a conflict
				}
				verdict = Worst(verdict, VerdictReview)
				add(CodeVendorEnvNoAgent, SeverityWarning, e.Prefix, strconv.Itoa(e.Count)+" "+e.Prefix+"* variables set, but no "+vendorTitle(e.Vendor)+" agent is visible (it may be in the image)")
			}
		}
	}

	// Launcher variables an injector must merge with rather than replace.
	if verdict == VerdictReady || verdict == VerdictReview || verdict == VerdictUnknown {
		for _, v := range res.Launcher {
			if !relevantLauncher(v.Name, res.Language) {
				continue
			}
			switch {
			case v.Source == "envfrom-secret":
				verdict = Worst(verdict, VerdictReview)
				secret := strings.TrimPrefix(v.Name, "envFrom Secret ")
				add(CodeEnvFromSecretUnread, SeverityWarning, secret, "Variables from Secret "+secret+" are not read and could set launcher options")
			case !v.Resolved:
				verdict = Worst(verdict, VerdictReview)
				add(CodeLauncherUnreadable, SeverityWarning, v.Name, v.Name+" comes from "+sourceText[v.Source])
			case v.Name == "JAVA_OPTS":
				add(CodeJavaOptsIgnored, SeverityInfo, v.Name, "JAVA_OPTS is set; only launch scripts read it, not the JVM")
			default:
				// Set but loading no agent: an injector appends to it. Context,
				// not a blocker; CollectorCtrl's injector always merges.
				add(CodeLauncherSet, SeverityInfo, v.Name, v.Name+" is already set (no agent); an injector must append to it, not replace it")
			}
		}
	}

	if verdict != VerdictInstrumented && verdict != VerdictConflict && verdict != VerdictUnsupported {
		add(CodeImageEnvNotChecked, SeverityInfo, "", "Image environment (Dockerfile ENV) not checked")
	}
	if !sawPod {
		add(CodeNoRunningPod, SeverityInfo, "", "No running pod, so webhook-injected agents cannot be seen")
	}
	if !in.InjectorsChecked {
		add(CodeInjectorsNotChecked, SeverityInfo, "", "Admission webhooks could not be listed")
	}
	reasons := make([]string, 0, len(findings))
	for _, f := range findings {
		reasons = append(reasons, f.Message)
	}
	res.Verdict, res.Findings, res.Reasons = verdict, findings, reasons
}

var langTitles = map[string]string{
	LangJava: "Java", LangNode: "Node.js", LangPython: "Python", LangDotNet: ".NET", LangGo: "Go", LangNative: "native", LangUnknown: "unknown",
}

func langTitle(l string) string {
	if t, ok := langTitles[l]; ok {
		return t
	}
	return l
}

// relevantLauncher limits merge warnings to the variables the container's
// runtime reads. LD_PRELOAD matters for every language.
func relevantLauncher(name, lang string) bool {
	if strings.HasPrefix(name, "envFrom Secret ") {
		return true
	}
	switch name {
	case "LD_PRELOAD":
		return true
	case "JAVA_TOOL_OPTIONS", "JDK_JAVA_OPTIONS", "_JAVA_OPTIONS", "JAVA_OPTS":
		return lang == LangJava || lang == LangUnknown
	case "NODE_OPTIONS":
		return lang == LangNode || lang == LangUnknown
	case "PYTHONPATH":
		return lang == LangPython || lang == LangUnknown
	case "CORECLR_ENABLE_PROFILING", "CORECLR_PROFILER", "CORECLR_PROFILER_PATH", "DOTNET_STARTUP_HOOKS":
		return lang == LangDotNet || lang == LangUnknown
	}
	return false
}

var sourceText = map[string]string{
	"secret": "a Secret, which is never read", "configmap": "a ConfigMap key that could not be read",
	"field": "a pod field reference", "resource": "a resource field reference",
}

var vendorTitles = map[string]string{
	"opentelemetry": "OpenTelemetry", "splunk": "Splunk", "datadog": "Datadog", "newrelic": "New Relic",
	"appdynamics": "AppDynamics", "dynatrace": "Dynatrace", "elastic": "Elastic", "instana": "Instana", "unknown": "Unrecognised",
}

func vendorTitle(v string) string {
	if t, ok := vendorTitles[v]; ok {
		return t
	}
	return v
}
