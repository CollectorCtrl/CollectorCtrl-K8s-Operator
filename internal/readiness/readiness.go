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
	// Injectors are the pod-mutating webhooks found in the cluster.
	Injectors []Injector
	// InjectorsChecked is false when webhooks could not be listed.
	InjectorsChecked bool
	Lookup           Lookup
	LookupAll        LookupAll
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

// decide sets the verdict and the reasons a reader needs to act on it.
func decide(res *Container, in Input, sawPod bool) {
	reasons := []string{}
	add := func(format string) { reasons = append(reasons, format) }

	vendors := vendorNames(res.Agents)
	for _, a := range res.Agents {
		how := a.Evidence
		if a.Injected {
			how += " (added by an admission webhook)"
		}
		add(vendorTitle(a.Vendor) + " " + a.Language + " agent: " + how)
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

	verdict := ""
	switch {
	case len(vendors) > 1:
		verdict = VerdictConflict
		add("More than one APM vendor is loaded: " + strings.Join(vendors, ", ") + ". Agents from different vendors in one process conflict")
	case len(vendors) == 1 && len(pending) > 0:
		verdict = VerdictConflict
		for v, name := range pending {
			add("Webhook " + name + " (" + vendorTitle(v) + ") also selects this pod and would add a second vendor's agent")
		}
	case len(vendors) == 1:
		verdict = VerdictInstrumented
		if vendors[0] == "unknown" {
			verdict = VerdictReview
			add("An agent is loaded but its vendor is not recognised; confirm before adding another")
		}
	}

	if verdict == "" {
		switch {
		case res.Language == LangUnknown:
			verdict = VerdictUnknown
			add("Language not recognisable from the command, environment or image name")
		case !supported(res.Language):
			verdict = VerdictUnsupported
			add("No OpenTelemetry auto-instrumentation path for " + res.Language + " here; instrument it with the SDK")
		default:
			verdict = VerdictReady
		}
		if verdict == VerdictReady || verdict == VerdictUnknown {
			for v, name := range pending {
				verdict = VerdictReview
				add("Webhook " + name + " (" + vendorTitle(v) + ") injects into new pods here; decide which vendor owns this workload")
			}
			for _, r := range res.Requests {
				verdict = VerdictReview
				if sawPod {
					add(vendorTitle(r.Vendor) + " injection is requested (" + r.Via + ") but the running pod has no agent; check the injector is installed and configured")
				} else {
					add(vendorTitle(r.Vendor) + " injection is requested (" + r.Via + ")")
				}
			}
			for _, e := range res.VendorEnv {
				if e.Vendor == "opentelemetry" {
					continue // OTEL_* also configure SDKs and collectors; not a conflict
				}
				verdict = Worst(verdict, VerdictReview)
				add(strconv.Itoa(e.Count) + " " + e.Prefix + "* variables are set but no " + vendorTitle(e.Vendor) + " agent loader is visible; the agent may be in the image")
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
				add(v.Name + " is not read; it could set JAVA_TOOL_OPTIONS or similar, so an injector must not assume they are empty")
			case !v.Resolved:
				verdict = Worst(verdict, VerdictReview)
				add(v.Name + " comes from " + sourceText[v.Source] + "; an injector must merge with its unknown value")
			case v.Name == "JAVA_OPTS":
				add("JAVA_OPTS is set; the JVM does not read it, only launch scripts do")
			default:
				verdict = Worst(verdict, VerdictReview)
				add(v.Name + " is already set in the manifest; an injector must append to it, not replace it")
			}
		}
	}

	if verdict != VerdictInstrumented && verdict != VerdictConflict && verdict != VerdictUnsupported {
		add("Image environment not checked: variables baked into the image (Dockerfile ENV) are not visible without registry access")
	}
	if !sawPod {
		add("No running pod: agents added by admission webhooks cannot be seen")
	}
	if !in.InjectorsChecked {
		add("Admission webhooks could not be listed (namespace-scoped install or missing permission)")
	}
	res.Verdict, res.Reasons = verdict, reasons
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
