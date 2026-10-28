package readiness

import (
	"encoding/json"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func tmpl(containers ...corev1.Container) corev1.PodTemplateSpec {
	return corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "payments"}, Annotations: map[string]string{}},
		Spec:       corev1.PodSpec{Containers: containers},
	}
}

func env(pairs ...string) []corev1.EnvVar {
	var out []corev1.EnvVar
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, corev1.EnvVar{Name: pairs[i], Value: pairs[i+1]})
	}
	return out
}

func one(t *testing.T, in Input) Container {
	t.Helper()
	got := Analyze(in)
	if len(got) != 1 {
		t.Fatalf("want 1 application container, got %d: %+v", len(got), got)
	}
	return got[0]
}

// hasCode checks a finding code and, when given, its subject.
func hasCode(c Container, code string, subject ...string) bool {
	for _, f := range c.Findings {
		if f.Code == code && (len(subject) == 0 || f.Subject == subject[0]) {
			return true
		}
	}
	return false
}

func hasReason(c Container, fragment string) bool {
	for _, r := range c.Reasons {
		if strings.Contains(r, fragment) {
			return true
		}
	}
	return false
}

func TestPlainJavaIsReadyButSaysImageWasNotChecked(t *testing.T) {
	c := one(t, Input{Template: tmpl(corev1.Container{Name: "app", Image: "ghcr.io/acme/payments:1.4", Command: []string{"java"}, Args: []string{"-jar", "/app/app.jar"}}), InjectorsChecked: true,
		Pod: &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}}}})
	if c.Verdict != VerdictReady || c.Language != LangJava || c.LanguageSource != "command" {
		t.Fatalf("got %s/%s/%s", c.Verdict, c.Language, c.LanguageSource)
	}
	if !hasCode(c, CodeImageEnvNotChecked) {
		t.Errorf("missing image caveat: %v", c.Reasons)
	}
}

func TestExistingJavaToolOptionsNeedsMerge(t *testing.T) {
	c := one(t, Input{Template: tmpl(corev1.Container{Name: "app", Image: "eclipse-temurin:21-jre", Env: env("JAVA_TOOL_OPTIONS", "-Xmx512m -Ddb.password=hunter2")}), InjectorsChecked: true})
	if c.Verdict != VerdictReady {
		t.Fatalf("a set launcher variable without an agent is context, not review: %s, %v", c.Verdict, c.Reasons)
	}
	if len(c.Launcher) != 1 || c.Launcher[0].OtherFlags != 2 || len(c.Launcher[0].Entries) != 0 {
		t.Fatalf("launcher %+v", c.Launcher)
	}
	if !hasCode(c, CodeLauncherSet, "JAVA_TOOL_OPTIONS") {
		t.Errorf("reasons %v", c.Reasons)
	}
	raw, _ := json.Marshal(c)
	if strings.Contains(string(raw), "hunter2") || strings.Contains(string(raw), "Xmx") {
		t.Errorf("launcher value leaked: %s", raw)
	}
}

func TestAgentOptionsAreDropped(t *testing.T) {
	c := one(t, Input{Template: tmpl(corev1.Container{Name: "app", Image: "acme/app", Env: env("JAVA_TOOL_OPTIONS", "-javaagent:/opt/dd/dd-java-agent.jar=apiKey=SECRET -Ddd.api.key=SECRET2")})})
	raw, _ := json.Marshal(c)
	if strings.Contains(string(raw), "SECRET") {
		t.Fatalf("secret leaked: %s", raw)
	}
	if len(c.Agents) != 1 || c.Agents[0].Vendor != "datadog" || c.Agents[0].Evidence != "-javaagent:/opt/dd/dd-java-agent.jar (JAVA_TOOL_OPTIONS)" {
		t.Fatalf("agents %+v", c.Agents)
	}
}

func TestOperatorInjectedAgentSeenOnlyInPod(t *testing.T) {
	template := tmpl(corev1.Container{Name: "app", Image: "acme/orders:2", Command: []string{"java", "-jar", "app.jar"}})
	template.Annotations["instrumentation.opentelemetry.io/inject-java"] = "observability/default"
	pod := &corev1.Pod{Spec: corev1.PodSpec{
		InitContainers: []corev1.Container{{Name: "opentelemetry-auto-instrumentation-java", Image: "ghcr.io/open-telemetry/opentelemetry-operator/autoinstrumentation-java:2.10.0"}},
		Containers:     []corev1.Container{{Name: "app", Env: env("JAVA_TOOL_OPTIONS", " -javaagent:/otel-auto-instrumentation-java/javaagent.jar", "OTEL_SERVICE_NAME", "orders")}},
	}}
	webhook := otelOperatorWebhook()
	c := one(t, Input{Template: template, Pod: pod, Injectors: ClassifyWebhooks([]admissionv1.MutatingWebhookConfiguration{webhook}), InjectorsChecked: true})
	if c.Verdict != VerdictInstrumented {
		t.Fatalf("verdict %s: %v", c.Verdict, c.Reasons)
	}
	if len(c.Agents) != 1 || c.Agents[0].Vendor != "opentelemetry" || !c.Agents[0].Injected {
		t.Fatalf("agents %+v", c.Agents)
	}
	if len(c.SelectedBy) != 1 || c.SelectedBy[0] != "opentelemetry-operator-mutation" {
		t.Errorf("selected by %v", c.SelectedBy)
	}
	var inPod bool
	for _, v := range c.Launcher {
		inPod = inPod || (v.Name == "JAVA_TOOL_OPTIONS" && v.InPod)
	}
	if !inPod {
		t.Errorf("JAVA_TOOL_OPTIONS should be flagged as added in the pod: %+v", c.Launcher)
	}
}

func TestRequestWithoutAgentIsReview(t *testing.T) {
	template := tmpl(corev1.Container{Name: "app", Image: "acme/orders:2", Command: []string{"node", "server.js"}})
	template.Annotations["instrumentation.opentelemetry.io/inject-nodejs"] = "true"
	pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Command: []string{"node", "server.js"}}}}}
	c := one(t, Input{Template: template, Pod: pod, InjectorsChecked: true})
	if c.Verdict != VerdictReview || !hasCode(c, CodeInjectionNotApplied) {
		t.Fatalf("verdict %s: %v", c.Verdict, c.Reasons)
	}
}

func TestTwoVendorsConflict(t *testing.T) {
	c := one(t, Input{Template: tmpl(corev1.Container{Name: "app", Image: "acme/app",
		Env:  env("JAVA_TOOL_OPTIONS", "-javaagent:/dd/dd-java-agent.jar"),
		Args: []string{"java", "-javaagent:/otel/opentelemetry-javaagent.jar", "-jar", "app.jar"}})})
	if c.Verdict != VerdictConflict || len(c.Agents) != 2 {
		t.Fatalf("verdict %s agents %+v", c.Verdict, c.Agents)
	}
}

func TestSelectorInjectorAgainstExistingAgentConflicts(t *testing.T) {
	dynatrace := admissionv1.MutatingWebhookConfiguration{ObjectMeta: metav1.ObjectMeta{Name: "dynatrace-webhook"}, Webhooks: []admissionv1.MutatingWebhook{{
		Name: "webhook.pod.dynatrace.com", Rules: podCreate(),
		NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"dynakube.internal.dynatrace.com/instance": "prod"}},
	}}}
	injectors := ClassifyWebhooks([]admissionv1.MutatingWebhookConfiguration{dynatrace})
	ns := map[string]string{"dynakube.internal.dynatrace.com/instance": "prod"}

	nr := one(t, Input{Template: tmpl(corev1.Container{Name: "app", Image: "acme/app", Env: env("NODE_OPTIONS", "-r newrelic")}), Injectors: injectors, InjectorsChecked: true, NamespaceLabels: ns})
	if nr.Verdict != VerdictConflict {
		t.Fatalf("new relic + dynatrace webhook: %s %v", nr.Verdict, nr.Reasons)
	}
	clean := one(t, Input{Template: tmpl(corev1.Container{Name: "app", Image: "node:20-alpine", Command: []string{"node", "index.js"}}), Injectors: injectors, InjectorsChecked: true, NamespaceLabels: ns})
	if clean.Verdict != VerdictReview || !hasCode(clean, CodeInjectorSelects, "dynatrace-webhook") {
		t.Fatalf("clean node in dynatrace namespace: %s %v", clean.Verdict, clean.Reasons)
	}
	other := one(t, Input{Template: tmpl(corev1.Container{Name: "app", Image: "node:20-alpine", Command: []string{"node", "index.js"}}), Injectors: injectors, InjectorsChecked: true, NamespaceLabels: map[string]string{}})
	if other.Verdict != VerdictReady {
		t.Fatalf("namespace not selected: %s %v", other.Verdict, other.Reasons)
	}
}

func TestLanguagesAndMechanisms(t *testing.T) {
	cases := []struct {
		name      string
		c         corev1.Container
		vendor    string
		lang      string
		verdict   string
		mechanism string
	}{
		{"ddtrace-run", corev1.Container{Name: "app", Image: "acme/api", Command: []string{"ddtrace-run", "gunicorn", "app:app"}}, "datadog", LangPython, VerdictInstrumented, "python-wrapper"},
		{"otel python wrapper in shell", corev1.Container{Name: "app", Image: "acme/api", Command: []string{"/bin/sh", "-c"}, Args: []string{"exec opentelemetry-instrument python main.py"}}, "opentelemetry", LangPython, VerdictInstrumented, "python-wrapper"},
		{"new relic dotnet profiler", corev1.Container{Name: "app", Image: "acme/api", Env: env("CORECLR_ENABLE_PROFILING", "1", "CORECLR_PROFILER", "{36032161-FFC0-4B61-B559-F6C5D41BAE5A}")}, "newrelic", LangDotNet, VerdictInstrumented, "dotnet-profiler"},
		{"oneagent preload", corev1.Container{Name: "app", Image: "acme/api", Env: env("LD_PRELOAD", "/opt/dynatrace/oneagent/agent/lib64/liboneagentproc.so")}, "dynatrace", LangUnknown, VerdictInstrumented, "ld-preload"},
		{"splunk node", corev1.Container{Name: "app", Image: "acme/api", Env: env("NODE_OPTIONS", "--require @splunk/otel/instrument")}, "splunk", LangNode, VerdictInstrumented, "node-require"},
		{"unknown jar", corev1.Container{Name: "app", Image: "acme/api", Env: env("JAVA_TOOL_OPTIONS", "-javaagent:/opt/agent/agent.jar")}, "unknown", LangJava, VerdictReview, "javaagent"},
		{"go app", corev1.Container{Name: "app", Image: "acme/api", Env: env("GOMAXPROCS", "2")}, "", LangGo, VerdictUnsupported, ""},
		{"no evidence", corev1.Container{Name: "app", Image: "acme/node-exporter-like"}, "", LangUnknown, VerdictUnknown, ""},
		{"base image", corev1.Container{Name: "app", Image: "mcr.microsoft.com/dotnet/aspnet:8.0"}, "", LangDotNet, VerdictReady, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := one(t, Input{Template: tmpl(tc.c), InjectorsChecked: true})
			if c.Language != tc.lang || c.Verdict != tc.verdict {
				t.Fatalf("got %s/%s want %s/%s: %v", c.Language, c.Verdict, tc.lang, tc.verdict, c.Reasons)
			}
			if tc.vendor == "" {
				if len(c.Agents) != 0 {
					t.Fatalf("unexpected agents %+v", c.Agents)
				}
				return
			}
			if len(c.Agents) != 1 || c.Agents[0].Vendor != tc.vendor || c.Agents[0].Mechanism != tc.mechanism {
				t.Fatalf("agents %+v", c.Agents)
			}
		})
	}
}

func TestEnvFromConfigMapAndSecret(t *testing.T) {
	c := corev1.Container{Name: "app", Image: "acme/web",
		EnvFrom: []corev1.EnvFromSource{
			{ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "web-env"}}},
			{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "web-secrets"}}},
		},
		Env: []corev1.EnvVar{{Name: "JDK_JAVA_OPTIONS", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{Key: "opts"}}}},
	}
	lookupAll := func(name string) (map[string]string, bool) {
		if name == "web-env" {
			return map[string]string{"NODE_OPTIONS": "--require dd-trace/init --max-old-space-size=512"}, true
		}
		return nil, false
	}
	res := one(t, Input{Template: tmpl(c), LookupAll: lookupAll, InjectorsChecked: true})
	if res.Verdict != VerdictInstrumented || res.Agents[0].Vendor != "datadog" {
		t.Fatalf("verdict %s agents %+v", res.Verdict, res.Agents)
	}
	var nodeOpts, secretVar, secretFrom bool
	for _, v := range res.Launcher {
		switch {
		case v.Name == "NODE_OPTIONS":
			nodeOpts = len(v.Entries) == 1 && v.Entries[0] == "--require dd-trace/init" && v.OtherFlags == 1 && v.Source == "envfrom-configmap"
		case v.Name == "JDK_JAVA_OPTIONS":
			secretVar = !v.Resolved && v.Source == "secret"
		case v.Name == "envFrom Secret web-secrets":
			secretFrom = !v.Resolved
		}
	}
	if !nodeOpts || !secretVar || !secretFrom {
		t.Fatalf("launcher %+v", res.Launcher)
	}
}

func TestUnreadSecretMakesReadyAppReview(t *testing.T) {
	c := corev1.Container{Name: "app", Image: "acme/web", Command: []string{"java", "-jar", "x.jar"},
		Env: []corev1.EnvVar{{Name: "JAVA_TOOL_OPTIONS", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{Key: "opts"}}}}}
	res := one(t, Input{Template: tmpl(c), InjectorsChecked: true})
	if res.Verdict != VerdictReview || !hasCode(res, CodeLauncherUnreadable, "JAVA_TOOL_OPTIONS") {
		t.Fatalf("verdict %s %v", res.Verdict, res.Reasons)
	}
}

func TestVendorEnvWithoutLoader(t *testing.T) {
	res := one(t, Input{Template: tmpl(corev1.Container{Name: "app", Image: "acme/web", Command: []string{"java", "-jar", "x.jar"}, Env: env("DD_SERVICE", "web", "DD_ENV", "prod", "OTEL_SERVICE_NAME", "web")}), InjectorsChecked: true})
	if res.Verdict != VerdictReview || !hasCode(res, CodeVendorEnvNoAgent, "DD_") || hasCode(res, CodeVendorEnvNoAgent, "OTEL_") || !hasReason(res, "2 DD_* variables") {
		t.Fatalf("verdict %s %v", res.Verdict, res.Reasons)
	}
}

func TestInfrastructureContainersSkipped(t *testing.T) {
	got := Analyze(Input{Template: tmpl(
		corev1.Container{Name: "istio-proxy", Image: "docker.io/istio/proxyv2:1.22.0"},
		corev1.Container{Name: "otel", Image: "otel/opentelemetry-collector-contrib:0.112.0"},
		corev1.Container{Name: "app", Image: "acme/web"},
	)})
	if len(got) != 1 || got[0].Name != "app" {
		t.Fatalf("got %+v", got)
	}
	if NotApplication("acme/vector-search") || NotApplication("acme/pause-service") {
		t.Error("application image treated as infrastructure")
	}
}

func TestClassifyWebhooks(t *testing.T) {
	ignore := admissionv1.Ignore
	fail := admissionv1.Fail
	configs := []admissionv1.MutatingWebhookConfiguration{
		otelOperatorWebhook(),
		{ObjectMeta: metav1.ObjectMeta{Name: "datadog-webhook"}, Webhooks: []admissionv1.MutatingWebhook{{
			Name: "datadog.webhook.lib.injection", Rules: podCreate(), FailurePolicy: &ignore,
			ObjectSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"admission.datadoghq.com/enabled": "true"}},
		}, {
			Name: "datadog.webhook.auto.instrumentation", Rules: podCreate(), FailurePolicy: &fail,
		}, {
			Name: "datadog.webhook.agent.config", Rules: podCreate(), // env only, not an agent
		}, {
			Name: "datadog.webhook.standard.tags", Rules: podCreate(),
		}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "newrelic-metadata-injection-cfg"}, Webhooks: []admissionv1.MutatingWebhook{{Name: "metadata-injection.newrelic.com", Rules: podCreate()}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "kyverno-resource-mutating-webhook-cfg"}, Webhooks: []admissionv1.MutatingWebhook{{Name: "mutate.kyverno.svc", Rules: podCreate()}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "datadog-deployments"}, Webhooks: []admissionv1.MutatingWebhook{{Name: "deploy.datadog", Rules: []admissionv1.RuleWithOperations{{
			Operations: []admissionv1.OperationType{admissionv1.Create}, Rule: admissionv1.Rule{Resources: []string{"deployments"}}}}}}},
	}
	got := ClassifyWebhooks(configs)
	if len(got) != 2 {
		t.Fatalf("got %+v", got)
	}
	dd, otel := got[0], got[1]
	if dd.Vendor != "datadog" || dd.Mode != ModeSelector || dd.FailurePolicy != "Fail" || len(dd.Webhooks) != 2 {
		t.Errorf("datadog %+v", dd)
	}
	if otel.Vendor != "opentelemetry" || otel.Mode != ModeRequest {
		t.Errorf("otel %+v", otel)
	}
	if otel.Selects(nil, nil, nil) {
		t.Error("request-mode injector selected a pod without a request")
	}
	if !otel.Selects(nil, nil, []Request{{Vendor: "opentelemetry"}}) {
		t.Error("request-mode injector ignored a request")
	}
}

func podCreate() []admissionv1.RuleWithOperations {
	return []admissionv1.RuleWithOperations{{
		Operations: []admissionv1.OperationType{admissionv1.Create},
		Rule:       admissionv1.Rule{APIGroups: []string{""}, APIVersions: []string{"v1"}, Resources: []string{"pods"}},
	}}
}

func otelOperatorWebhook() admissionv1.MutatingWebhookConfiguration {
	return admissionv1.MutatingWebhookConfiguration{ObjectMeta: metav1.ObjectMeta{Name: "opentelemetry-operator-mutation"}, Webhooks: []admissionv1.MutatingWebhook{{
		Name: "mpod.kb.io", Rules: podCreate(),
		ClientConfig: admissionv1.WebhookClientConfig{Service: &admissionv1.ServiceReference{Namespace: "opentelemetry-operator-system", Name: "opentelemetry-operator-webhook"}},
	}}}
}

func TestSplunkOperatorIsNotAConflictWithOTelAgents(t *testing.T) {
	splunk := admissionv1.MutatingWebhookConfiguration{ObjectMeta: metav1.ObjectMeta{Name: "splunk-otel-collector-operator-mutation"}, Webhooks: []admissionv1.MutatingWebhook{{
		Name: "mpod.kb.io", Rules: podCreate(),
	}}}
	template := tmpl(corev1.Container{Name: "app", Image: "acme/orders", Env: env("JAVA_TOOL_OPTIONS", "-javaagent:/otel-auto-instrumentation-java/javaagent.jar")})
	template.Annotations["instrumentation.opentelemetry.io/inject-java"] = "true"
	c := one(t, Input{Template: template, Injectors: ClassifyWebhooks([]admissionv1.MutatingWebhookConfiguration{splunk}), InjectorsChecked: true})
	if c.Verdict != VerdictInstrumented || len(c.SelectedBy) != 1 {
		t.Fatalf("verdict %s selected %v reasons %v", c.Verdict, c.SelectedBy, c.Reasons)
	}
}

func TestFindingsCarryCodesAndSeverities(t *testing.T) {
	c := one(t, Input{Template: tmpl(corev1.Container{Name: "app", Image: "acme/app",
		Env:  env("JAVA_TOOL_OPTIONS", "-javaagent:/dd/dd-java-agent.jar"),
		Args: []string{"java", "-javaagent:/otel/opentelemetry-javaagent.jar", "-jar", "app.jar"}})})
	if !hasCode(c, CodeMultipleVendors, "datadog,opentelemetry") || !hasCode(c, CodeAgentLoaded, "datadog") || !hasCode(c, CodeNoRunningPod) {
		t.Fatalf("findings %+v", c.Findings)
	}
	for _, f := range c.Findings {
		if f.Severity != SeverityBlocker && f.Severity != SeverityWarning && f.Severity != SeverityInfo {
			t.Errorf("bad severity %+v", f)
		}
		if f.Code == CodeMultipleVendors && f.Severity != SeverityBlocker {
			t.Errorf("conflict is not a blocker: %+v", f)
		}
	}
	if len(c.Reasons) != len(c.Findings) {
		t.Errorf("reasons should mirror findings")
	}
}

func TestLanguageAnnotationResolvesUnknown(t *testing.T) {
	template := tmpl(corev1.Container{Name: "app", Image: "acme/app"}, corev1.Container{Name: "worker", Image: "acme/worker"})
	got := Analyze(Input{Template: template, InjectorsChecked: true, WorkloadAnnotations: map[string]string{AnnotationLanguage: "app=java, worker=python"}})
	if len(got) != 2 || got[0].Language != LangJava || got[0].LanguageSource != "annotation" || got[0].Verdict != VerdictReady || got[1].Language != LangPython {
		t.Fatalf("got %+v", got)
	}
	whole := one(t, Input{Template: tmpl(corev1.Container{Name: "app", Image: "acme/app"}), WorkloadAnnotations: map[string]string{AnnotationLanguage: "Node.js"}})
	if whole.Language != LangUnknown {
		t.Errorf("invalid value accepted: %s", whole.Language)
	}
	ok := one(t, Input{Template: tmpl(corev1.Container{Name: "app", Image: "acme/app"}), WorkloadAnnotations: map[string]string{AnnotationLanguage: "nodejs"}})
	if ok.Language != LangNode {
		t.Errorf("got %s", ok.Language)
	}
}
