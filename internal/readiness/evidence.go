// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 CollectorCtrl

package readiness

import (
	"path"
	"regexp"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// envVal is one resolved environment variable.
type envVal struct {
	value    string
	source   string // literal, configmap, secret, field, resource, envfrom-configmap
	resolved bool
}

// evidence is what one container (in a template or a running pod) exposes.
type evidence struct {
	args          []string
	env           map[string]envVal
	inits         []corev1.Container
	volumes       []string
	labels        map[string]string
	annotations   map[string]string
	unreadSecrets []string // envFrom Secrets whose keys are unknown
}

func collect(c corev1.Container, spec *corev1.PodSpec, labels, annotations map[string]string, lookup Lookup, lookupAll LookupAll) *evidence {
	e := &evidence{env: map[string]envVal{}, labels: labels, annotations: annotations}
	e.args = append(append(e.args, c.Command...), c.Args...)
	// envFrom first: explicit env entries override it, as in Kubernetes.
	for _, from := range c.EnvFrom {
		switch {
		case from.ConfigMapRef != nil:
			if lookupAll == nil {
				continue
			}
			if data, ok := lookupAll(from.ConfigMapRef.Name); ok {
				for k, v := range data {
					e.env[from.Prefix+k] = envVal{value: v, source: "envfrom-configmap", resolved: true}
				}
			}
		case from.SecretRef != nil:
			e.unreadSecrets = append(e.unreadSecrets, from.SecretRef.Name)
		}
	}
	for _, v := range c.Env {
		switch {
		case v.ValueFrom == nil:
			e.env[v.Name] = envVal{value: v.Value, source: "literal", resolved: true}
		case v.ValueFrom.ConfigMapKeyRef != nil:
			ref := v.ValueFrom.ConfigMapKeyRef
			val, ok := "", false
			if lookup != nil {
				val, ok = lookup(ref.Name, ref.Key)
			}
			e.env[v.Name] = envVal{value: val, source: "configmap", resolved: ok}
		case v.ValueFrom.SecretKeyRef != nil:
			e.env[v.Name] = envVal{source: "secret"}
		case v.ValueFrom.FieldRef != nil:
			e.env[v.Name] = envVal{source: "field"}
		default:
			e.env[v.Name] = envVal{source: "resource"}
		}
	}
	e.inits = spec.InitContainers
	for _, vol := range spec.Volumes {
		e.volumes = append(e.volumes, strings.ToLower(vol.Name))
		if vol.HostPath != nil {
			e.volumes = append(e.volumes, strings.ToLower(vol.HostPath.Path))
		}
	}
	return e
}

func (e *evidence) value(name string) (string, bool) {
	v, ok := e.env[name]
	if !ok || !v.resolved {
		return "", false
	}
	return v.value, true
}

// ---------------------------------------------------------------- agents

// vendorPattern maps a lower-case fragment of a jar path, module name, profiler
// path or preload library to a vendor. Order matters: first match wins.
type vendorPattern struct{ fragment, vendor string }

var javaAgentVendors = []vendorPattern{
	{"splunk-otel-javaagent", "splunk"},
	{"opentelemetry-javaagent", "opentelemetry"},
	{"otel-auto-instrumentation-java", "opentelemetry"}, // upstream OTel Operator injection path
	{"opentelemetry", "opentelemetry"},
	{"dd-java-agent", "datadog"},
	{"datadog", "datadog"},
	{"newrelic", "newrelic"},
	{"appdynamics", "appdynamics"},
	{"appagent", "appdynamics"},
	{"elastic-apm-agent", "elastic"},
	{"elastic", "elastic"},
	{"instana", "instana"},
	{"oneagent", "dynatrace"},
	{"dynatrace", "dynatrace"},
}

var nodeModuleVendors = []vendorPattern{
	{"@splunk/otel", "splunk"},
	{"@opentelemetry/auto-instrumentations-node", "opentelemetry"},
	{"otel-auto-instrumentation-nodejs", "opentelemetry"},
	{"dd-trace", "datadog"},
	{"newrelic", "newrelic"},
	{"appdynamics", "appdynamics"},
	{"elastic-apm-node", "elastic"},
	{"@instana/", "instana"},
	{"instana", "instana"},
	{"@dynatrace/", "dynatrace"},
}

var pythonWrapperVendors = map[string]string{
	"opentelemetry-instrument": "opentelemetry",
	"splunk-py-trace":          "splunk",
	"ddtrace-run":              "datadog",
	"newrelic-admin":           "newrelic",
	"pyagent":                  "appdynamics",
}

var pythonPathVendors = []vendorPattern{
	{"otel-auto-instrumentation-python", "opentelemetry"},
	{"opentelemetry/instrumentation/auto_instrumentation", "opentelemetry"},
	{"splunk_otel", "splunk"},
	{"ddtrace", "datadog"},
	{"datadog", "datadog"},
	{"newrelic", "newrelic"},
	{"appdynamics", "appdynamics"},
}

var dotnetProfilerGUIDs = map[string]string{
	"{918728dd-259f-4a6a-ac2b-b85e1b658318}": "opentelemetry", // also used by Splunk's OTel-based distribution
	"{846f5f1c-f9ae-4b07-969e-05c26bc060d8}": "datadog",
	"{36032161-ffc0-4b61-b559-f6c5d41bae5a}": "newrelic",
}

var nativePathVendors = []vendorPattern{
	{"opentelemetry", "opentelemetry"},
	{"otel", "opentelemetry"},
	{"datadog", "datadog"},
	{"newrelic", "newrelic"},
	{"appdynamics", "appdynamics"},
	{"elastic", "elastic"},
	{"instana", "instana"},
	{"oneagent", "dynatrace"},
	{"dynatrace", "dynatrace"},
}

func vendorOf(s string, patterns []vendorPattern) string {
	s = strings.ToLower(s)
	for _, p := range patterns {
		if strings.Contains(s, p.fragment) {
			return p.vendor
		}
	}
	return ""
}

var javaLaunchers = []string{"JAVA_TOOL_OPTIONS", "JDK_JAVA_OPTIONS", "_JAVA_OPTIONS", "JAVA_OPTS"}

// detectAgents finds loaded agents. Loaders (flags, profiler variables,
// preload libraries) come first; init containers and markers only add vendors
// not already found, so one agent is not listed twice.
func detectAgents(e *evidence) []Agent {
	var out []Agent
	seen := map[string]bool{}
	add := func(a Agent) {
		key := a.Vendor + "/" + a.Language
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, a)
	}

	// Java: -javaagent / -agentpath in launcher variables or the command line.
	type src struct{ text, where string }
	var javaSources []src
	for _, name := range javaLaunchers {
		if v, ok := e.value(name); ok {
			javaSources = append(javaSources, src{v, name})
		}
	}
	javaSources = append(javaSources, src{strings.Join(e.args, " "), "command"})
	for _, s := range javaSources {
		for _, tok := range strings.Fields(s.text) {
			flag, target := splitAgentFlag(tok)
			if flag == "" {
				continue
			}
			vendor := vendorOf(target, javaAgentVendors)
			if vendor == "" {
				vendor = "unknown"
			}
			add(Agent{Vendor: vendor, Language: LangJava, Mechanism: "javaagent", Evidence: flag + target + " (" + s.where + ")"})
		}
	}

	// Node.js: --require / --import of a known agent module.
	nodeText := []src{}
	if v, ok := e.value("NODE_OPTIONS"); ok {
		nodeText = append(nodeText, src{v, "NODE_OPTIONS"})
	}
	nodeText = append(nodeText, src{strings.Join(e.args, " "), "command"})
	for _, s := range nodeText {
		for _, mod := range nodeModules(s.text) {
			if vendor := vendorOf(mod, nodeModuleVendors); vendor != "" {
				add(Agent{Vendor: vendor, Language: LangNode, Mechanism: "node-require", Evidence: "--require " + mod + " (" + s.where + ")"})
			}
		}
	}

	// Python: wrapper commands and PYTHONPATH entries.
	for _, tok := range e.args {
		for _, word := range strings.Fields(tok) {
			if vendor, ok := pythonWrapperVendors[path.Base(word)]; ok {
				add(Agent{Vendor: vendor, Language: LangPython, Mechanism: "python-wrapper", Evidence: path.Base(word) + " (command)"})
			}
		}
	}
	if v, ok := e.value("PYTHONPATH"); ok {
		for _, entry := range splitList(v) {
			if vendor := vendorOf(entry, pythonPathVendors); vendor != "" {
				add(Agent{Vendor: vendor, Language: LangPython, Mechanism: "python-path", Evidence: entry + " (PYTHONPATH)"})
			}
		}
	}

	// .NET: CLR profiler and startup hooks.
	enabled, _ := e.value("CORECLR_ENABLE_PROFILING")
	if strings.TrimSpace(enabled) == "1" {
		profiler, _ := e.value("CORECLR_PROFILER")
		vendor := dotnetProfilerGUIDs[strings.ToLower(strings.TrimSpace(profiler))]
		evidenceText := "CORECLR_PROFILER " + strings.TrimSpace(profiler)
		if vendor == "" {
			for name, val := range e.env {
				if strings.HasPrefix(name, "CORECLR_PROFILER_PATH") && val.resolved {
					if vendor = vendorOf(val.value, nativePathVendors); vendor != "" {
						evidenceText = name + " " + val.value
						break
					}
				}
			}
		}
		if vendor == "" {
			vendor = "unknown"
		}
		add(Agent{Vendor: vendor, Language: LangDotNet, Mechanism: "dotnet-profiler", Evidence: evidenceText})
	}
	if v, ok := e.value("DOTNET_STARTUP_HOOKS"); ok {
		for _, hook := range splitList(v) {
			l := strings.ToLower(hook)
			switch {
			case strings.Contains(l, "opentelemetry"):
				add(Agent{Vendor: "opentelemetry", Language: LangDotNet, Mechanism: "dotnet-startup-hook", Evidence: hook + " (DOTNET_STARTUP_HOOKS)"})
			case strings.Contains(l, "elastic"):
				add(Agent{Vendor: "elastic", Language: LangDotNet, Mechanism: "dotnet-startup-hook", Evidence: hook + " (DOTNET_STARTUP_HOOKS)"})
			}
		}
	}

	// Any language: preloaded libraries (Dynatrace OneAgent, Datadog injector, ...).
	if v, ok := e.value("LD_PRELOAD"); ok {
		for _, lib := range splitList(v) {
			if vendor := vendorOf(lib, nativePathVendors); vendor != "" {
				add(Agent{Vendor: vendor, Language: LangNative, Mechanism: "ld-preload", Evidence: lib + " (LD_PRELOAD)"})
			}
		}
	}

	// Init containers and markers left by injectors.
	have := map[string]bool{}
	for _, a := range out {
		have[a.Vendor] = true
	}
	for _, ic := range e.inits {
		vendor, lang := initContainerAgent(ic)
		if vendor != "" && !have[vendor] {
			have[vendor] = true
			add(Agent{Vendor: vendor, Language: lang, Mechanism: "init-container", Evidence: "init container " + ic.Name})
		}
	}
	for key, val := range e.annotations {
		vendor := ""
		switch {
		case key == "oneagent.dynatrace.com/injected" && val == "true":
			vendor = "dynatrace"
		case key == "instana-autotrace-applied" && val == "true":
			vendor = "instana"
		}
		if vendor != "" && !have[vendor] {
			have[vendor] = true
			add(Agent{Vendor: vendor, Language: LangNative, Mechanism: "injected-annotation", Evidence: "annotation " + key})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Vendor < out[j].Vendor })
	return out
}

// splitAgentFlag returns ("-javaagent:", "/path.jar") for an agent flag,
// dropping options after "=" because they can carry keys.
func splitAgentFlag(tok string) (string, string) {
	for _, flag := range []string{"-javaagent:", "-agentpath:"} {
		if strings.HasPrefix(tok, flag) {
			target := strings.TrimPrefix(tok, flag)
			if i := strings.Index(target, "="); i >= 0 {
				target = target[:i]
			}
			return flag, strings.Trim(target, `"'`)
		}
	}
	return "", ""
}

// nodeModules lists modules loaded with --require/-r/--import/--loader.
func nodeModules(text string) []string {
	mods, _ := nodeModulesConsumed(text)
	return mods
}

// nodeModulesConsumed also reports how many fields the module flags used, so
// the remaining flags can be counted without being copied. A value that looks
// like another option (starts with "-") is never taken as a module: with a
// trailing "--require", the next option may carry a credential.
func nodeModulesConsumed(text string) ([]string, int) {
	var out []string
	consumed := 0
	module := func(v string) (string, bool) {
		v = strings.Trim(v, `"'`)
		if v == "" || strings.HasPrefix(v, "-") || len(v) > 256 {
			return "", false
		}
		return v, true
	}
	fields := strings.Fields(text)
	for i := 0; i < len(fields); i++ {
		f := strings.Trim(fields[i], `"'`)
		for _, flag := range []string{"--require", "-r", "--import", "--loader", "--experimental-loader"} {
			switch {
			case f == flag && i+1 < len(fields):
				if m, ok := module(fields[i+1]); ok {
					out = append(out, m)
					consumed += 2
					i++
				}
			case strings.HasPrefix(f, flag+"="):
				if m, ok := module(strings.TrimPrefix(f, flag+"=")); ok {
					out = append(out, m)
					consumed++
				}
			}
		}
	}
	return out, consumed
}

func splitList(v string) []string {
	return strings.FieldsFunc(v, func(r rune) bool { return r == ':' || r == ';' || r == ' ' || r == '\t' })
}

var initLangSuffix = map[string]string{"java": LangJava, "nodejs": LangNode, "node": LangNode, "python": LangPython, "dotnet": LangDotNet}

func langFromName(name string) string {
	for _, part := range strings.FieldsFunc(name, func(r rune) bool { return r == '-' || r == '_' || r == '/' }) {
		if l, ok := initLangSuffix[part]; ok {
			return l
		}
	}
	return LangNative
}

// initContainerAgent recognises the init containers injectors add to copy an
// agent into the pod.
func initContainerAgent(ic corev1.Container) (string, string) {
	name := strings.ToLower(ic.Name)
	image := repository(ic.Image)
	switch {
	case strings.HasPrefix(name, "opentelemetry-auto-instrumentation"),
		strings.Contains(image, "opentelemetry-operator/autoinstrumentation"):
		return "opentelemetry", langFromName(name + "-" + path.Base(image))
	case strings.HasPrefix(name, "datadog-lib-"), strings.HasPrefix(name, "datadog-init-apm-inject"):
		return "datadog", langFromName(name)
	case strings.HasPrefix(name, "newrelic-instrumentation"):
		return "newrelic", langFromName(name)
	case strings.HasPrefix(name, "appd-agent-attach"):
		return "appdynamics", langFromName(name)
	case name == "install-oneagent", strings.Contains(image, "dynatrace"):
		return "dynatrace", LangNative
	case strings.HasPrefix(name, "instana-instrumentation"):
		return "instana", LangNative
	case strings.Contains(image, "elastic.co/observability/apm-agent-"):
		return "elastic", langFromName(path.Base(image))
	}
	return "", ""
}

// ---------------------------------------------------------------- requests

var otelInjectLang = map[string]string{
	"java": LangJava, "nodejs": LangNode, "python": LangPython, "dotnet": LangDotNet, "go": LangGo,
}

// detectRequests finds instrumentation requests on the pod template or its
// namespace. They show intent, not a running agent.
func detectRequests(e *evidence, nsAnnotations map[string]string) []Request {
	var out []Request
	scan := func(m map[string]string, where string) {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			v := strings.ToLower(strings.TrimSpace(m[k]))
			switch {
			case strings.HasPrefix(k, "instrumentation.opentelemetry.io/inject-") && v != "" && v != "false":
				lang := strings.TrimPrefix(k, "instrumentation.opentelemetry.io/inject-")
				if l, ok := otelInjectLang[lang]; ok {
					out = append(out, Request{Vendor: "opentelemetry", Language: l, Via: where + " " + k})
				} else if lang != "sdk" {
					out = append(out, Request{Vendor: "opentelemetry", Language: lang, Via: where + " " + k})
				}
			case strings.HasPrefix(k, "instrumentation.newrelic.com/inject-") && v != "" && v != "false":
				out = append(out, Request{Vendor: "newrelic", Language: langOrEmpty(strings.TrimPrefix(k, "instrumentation.newrelic.com/inject-")), Via: where + " " + k})
			case strings.HasPrefix(k, "admission.datadoghq.com/") && strings.HasSuffix(k, "-lib.version"):
				lang := strings.TrimSuffix(strings.TrimPrefix(k, "admission.datadoghq.com/"), "-lib.version")
				out = append(out, Request{Vendor: "datadog", Language: langOrEmpty(lang), Via: where + " " + k})
			case k == "co.elastic.apm/attach" && v != "":
				out = append(out, Request{Vendor: "elastic", Via: where + " " + k})
			}
		}
	}
	scan(e.annotations, "pod annotation")
	scan(nsAnnotations, "namespace annotation")
	if e.labels["admission.datadoghq.com/enabled"] == "true" {
		out = append(out, Request{Vendor: "datadog", Via: "pod label admission.datadoghq.com/enabled"})
	}
	return out
}

func langOrEmpty(s string) string {
	if l, ok := initLangSuffix[s]; ok {
		return l
	}
	if s == "js" {
		return LangNode
	}
	return s
}

// ---------------------------------------------------------------- launchers

var launcherNames = []string{
	"JAVA_TOOL_OPTIONS", "JDK_JAVA_OPTIONS", "_JAVA_OPTIONS", "JAVA_OPTS",
	"NODE_OPTIONS", "PYTHONPATH",
	"CORECLR_ENABLE_PROFILING", "CORECLR_PROFILER", "CORECLR_PROFILER_PATH", "DOTNET_STARTUP_HOOKS",
	"LD_PRELOAD",
}

func launcherVars(e *evidence) []LauncherVar {
	var out []LauncherVar
	for _, name := range launcherNames {
		v, ok := e.env[name]
		if !ok {
			continue
		}
		lv := LauncherVar{Name: name, Source: v.source, Resolved: v.resolved}
		if v.resolved {
			lv.Entries, lv.OtherFlags = launcherEntries(name, v.value)
		}
		out = append(out, lv)
	}
	for _, s := range e.unreadSecrets {
		out = append(out, LauncherVar{Name: "envFrom Secret " + s, Source: "envfrom-secret"})
	}
	return out
}

// launcherEntries keeps only the agent-loading parts of a launcher value.
func launcherEntries(name, value string) ([]string, int) {
	var entries []string
	other := 0
	switch name {
	case "JAVA_TOOL_OPTIONS", "JDK_JAVA_OPTIONS", "_JAVA_OPTIONS", "JAVA_OPTS":
		for _, tok := range strings.Fields(value) {
			if flag, target := splitAgentFlag(tok); flag != "" {
				entries = append(entries, flag+target)
			} else {
				other++
			}
		}
	case "NODE_OPTIONS":
		mods, consumed := nodeModulesConsumed(value)
		for _, m := range mods {
			entries = append(entries, "--require "+m)
		}
		other = len(strings.Fields(value)) - consumed
	case "CORECLR_ENABLE_PROFILING", "CORECLR_PROFILER":
		v := strings.TrimSpace(value)
		if guidOrFlag.MatchString(v) {
			entries = append(entries, v)
		} else if v != "" {
			other++
		}
	default: // PYTHONPATH, CORECLR_PROFILER_PATH, DOTNET_STARTUP_HOOKS, LD_PRELOAD: paths
		entries = append(entries, splitList(value)...)
	}
	if other < 0 {
		other = 0
	}
	return entries, other
}

var guidOrFlag = regexp.MustCompile(`^(\{?[0-9A-Fa-f-]{36}\}?|[01])$`)

var vendorEnvPrefixes = []struct{ prefix, vendor string }{
	{"OTEL_", "opentelemetry"}, {"SPLUNK_", "splunk"}, {"DD_", "datadog"}, {"NEW_RELIC_", "newrelic"},
	{"APPDYNAMICS_", "appdynamics"}, {"DT_", "dynatrace"}, {"ELASTIC_APM_", "elastic"}, {"INSTANA_", "instana"},
}

func vendorEnv(e *evidence) []VendorEnv {
	var out []VendorEnv
	for _, p := range vendorEnvPrefixes {
		n := 0
		for name := range e.env {
			if strings.HasPrefix(name, p.prefix) {
				n++
			}
		}
		if n > 0 {
			out = append(out, VendorEnv{Vendor: p.vendor, Prefix: p.prefix, Count: n})
		}
	}
	return out
}

// ---------------------------------------------------------------- language

var commandLang = map[string]string{
	"java": LangJava, "catalina.sh": LangJava, "jshell": LangJava,
	"node": LangNode, "nodejs": LangNode, "npm": LangNode, "npx": LangNode, "yarn": LangNode, "pnpm": LangNode, "pm2-runtime": LangNode,
	"python": LangPython, "python3": LangPython, "gunicorn": LangPython, "uvicorn": LangPython, "celery": LangPython,
	"hypercorn": LangPython, "daphne": LangPython, "flask": LangPython, "django-admin": LangPython,
	"dotnet": LangDotNet,
}

var envLang = map[string]string{
	"JAVA_HOME": LangJava, "JAVA_VERSION": LangJava, "JAVA_TOOL_OPTIONS": LangJava, "JDK_JAVA_OPTIONS": LangJava,
	"JAVA_OPTS": LangJava, "CATALINA_OPTS": LangJava, "SPRING_PROFILES_ACTIVE": LangJava,
	"NODE_ENV": LangNode, "NODE_OPTIONS": LangNode,
	"PYTHONPATH": LangPython, "PYTHONUNBUFFERED": LangPython, "PYTHONDONTWRITEBYTECODE": LangPython,
	"DJANGO_SETTINGS_MODULE": LangPython, "GUNICORN_CMD_ARGS": LangPython,
	"ASPNETCORE_URLS": LangDotNet, "ASPNETCORE_ENVIRONMENT": LangDotNet, "DOTNET_RUNNING_IN_CONTAINER": LangDotNet,
	"CORECLR_ENABLE_PROFILING": LangDotNet,
	"GOMAXPROCS":               LangGo, "GOGC": LangGo, "GOMEMLIMIT": LangGo, "GODEBUG": LangGo,
}

var imageLang = []struct {
	re   *regexp.Regexp
	lang string
}{
	{regexp.MustCompile(`^(openjdk|eclipse-temurin|temurin|amazoncorretto|ibm-semeru-runtimes|ibmjava|adoptopenjdk|tomcat|jetty|wildfly|java\d*|jdk\d*|jre\d*)(-(alpine|slim|debian\d*|ubi\d*|jre|jdk|minimal))*$`), LangJava},
	{regexp.MustCompile(`^(node|nodejs)\d*(-(alpine|slim|bullseye|bookworm|debian\d*|lts|minimal))*$`), LangNode},
	{regexp.MustCompile(`^python\d*(\.\d+)?(-(alpine|slim|bullseye|bookworm|debian\d*|minimal))*$`), LangPython},
	{regexp.MustCompile(`^(aspnet|dotnet)$`), LangDotNet},
	{regexp.MustCompile(`^golang$`), LangGo},
}

// detectLanguage prefers the strongest evidence: a language-specific agent,
// then the command line, then environment names, then a well-known base image
// name. Image ENV is not visible, so many containers stay unknown.
func detectLanguage(c corev1.Container, e *evidence, agents []Agent) (string, string) {
	for _, a := range agents {
		if a.Language != LangNative {
			return a.Language, "agent"
		}
	}
	tokens := []string{}
	for _, arg := range e.args {
		tokens = append(tokens, strings.Fields(arg)...)
	}
	for i, tok := range tokens {
		base := path.Base(strings.Trim(tok, `"'`))
		if l, ok := commandLang[base]; ok {
			return l, "command"
		}
		if strings.HasPrefix(base, "python3.") {
			return LangPython, "command"
		}
		if base == "-jar" || (strings.HasSuffix(base, ".jar") && i > 0) {
			return LangJava, "command"
		}
	}
	votes := map[string]int{}
	for name := range e.env {
		if l, ok := envLang[name]; ok {
			votes[l]++
		} else if strings.HasPrefix(name, "DOTNET_") || strings.HasPrefix(name, "ASPNETCORE_") {
			votes[LangDotNet]++
		}
	}
	best, bestN, tie := "", 0, false
	for l, n := range votes {
		switch {
		case n > bestN:
			best, bestN, tie = l, n, false
		case n == bestN:
			tie = true
		}
	}
	if best != "" && !tie {
		return best, "env"
	}
	repo := repository(c.Image)
	if strings.Contains(repo, "mcr.microsoft.com/dotnet/") {
		return LangDotNet, "image"
	}
	for _, seg := range strings.Split(repo, "/") {
		for _, h := range imageLang {
			if h.re.MatchString(seg) {
				return h.lang, "image"
			}
		}
	}
	return LangUnknown, ""
}
