// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 CollectorCtrl

// Package webhook serves the pod-mutating admission webhook that adds the
// OpenTelemetry Java agent to opted-in pods, and manages the certificate the
// API server uses to call it.
package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/collectorctrl/collectorctrl/internal/instrument"
	"github.com/collectorctrl/collectorctrl/internal/readiness"
	"github.com/collectorctrl/collectorctrl/internal/registry"
	v1 "github.com/collectorctrl/collectorctrl/operator/api/v1alpha1"
)

// Path the webhook is served on.
const Path = "/mutate-pod-java"

// DefaultProfileName is the profile in the operator namespace that applies
// when a namespace has none of its own.
const DefaultProfileName = "default"

// PodInjector handles pod CREATE admission. Every answer is "allowed": the
// webhook changes pods or records why it did not, and never blocks a start
// (the configuration also uses failurePolicy Ignore).
type PodInjector struct {
	// Client is the manager's cached client (Instrumentations, Pods, ConfigMaps).
	Client client.Client
	// APIReader reads objects that are not cached (Namespaces, webhook
	// configurations, the registry credentials Secret).
	APIReader client.Reader
	// OperatorNamespace holds the default profile.
	OperatorNamespace string
	// OwnWebhookName is this operator's MutatingWebhookConfiguration, left out
	// of the "another injector selects this pod" check.
	OwnWebhookName string
	// Registry reads image configuration when a profile allows it.
	Registry *registry.Reader
	// Decoder decodes admission payloads.
	Decoder admission.Decoder

	mu        sync.Mutex
	injectors []readiness.Injector
	hooksAt   time.Time
	nsLabels  map[string]nsEntry
}

type nsEntry struct {
	labels map[string]string
	at     time.Time
}

const cacheTTL = time.Minute

// Handle implements admission.Handler.
func (h *PodInjector) Handle(ctx context.Context, req admission.Request) admission.Response {
	log := logf.FromContext(ctx).WithName("java-webhook").WithValues("namespace", req.Namespace, "pod", req.Name)
	pod := &corev1.Pod{}
	if err := h.Decoder.Decode(req, pod); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	if instrument.ModeOf(pod.Labels) == "" {
		return admission.Allowed("not opted in")
	}
	in := instrument.Input{Pod: pod, Namespace: req.Namespace, Workload: instrument.OwnerOf(pod), InjectorsChecked: true}
	in.Profile = h.profile(ctx, req.Namespace, pod.Annotations)
	in.NamespaceLabels = h.namespaceLabels(ctx, req.Namespace)
	in.Injectors, in.InjectorsChecked = h.otherInjectors(ctx)
	in.Lookup = func(name, key string) (string, bool) {
		cm := &corev1.ConfigMap{}
		if err := h.Client.Get(ctx, client.ObjectKey{Namespace: req.Namespace, Name: name}, cm); err != nil {
			return "", false
		}
		v, ok := cm.Data[key]
		return v, ok
	}
	in.LookupAll = func(name string) (map[string]string, bool) {
		cm := &corev1.ConfigMap{}
		if err := h.Client.Get(ctx, client.ObjectKey{Namespace: req.Namespace, Name: name}, cm); err != nil {
			return nil, false
		}
		return cm.Data, true
	}
	if in.Profile != nil && in.Profile.Spec.Registry.ReadImageConfig && h.Registry != nil {
		in.ImageEnv = h.imageEnv(ctx, in.Profile)
	}
	if in.Profile != nil && instrument.ModeOf(pod.Labels) == v1.InstrumentModeCanary {
		in.CanaryTaken = h.canaryTaken(ctx, req.Namespace, in.Workload)
	}

	res := instrument.Decide(in)
	if res.Code == instrument.CodeAlreadyInjected {
		log.V(1).Info("webhook re-invoked on an already instrumented pod; no change")
		return admission.Allowed("already instrumented")
	}
	mutated := pod.DeepCopy()
	instrument.Apply(mutated, in, res)
	raw, err := json.Marshal(mutated)
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}
	if res.Injected {
		log.Info("java agent injected", "container", res.Container, "mode", res.Mode, "profile", in.Profile.Namespace+"/"+in.Profile.Name, "notes", res.Notes)
	} else {
		log.Info("java agent not injected", "code", res.Code, "reason", res.Reason)
	}
	return admission.PatchResponseFromRaw(req.Object.Raw, raw)
}

// profile picks the Instrumentation for a pod: the annotated one, else the
// single profile in the namespace, else "default" in the operator namespace.
func (h *PodInjector) profile(ctx context.Context, namespace string, annotations map[string]string) *v1.Instrumentation {
	ns, name := instrument.ProfileRef(annotations)
	if name != "" {
		if ns == "" {
			ns = namespace
		}
		p := &v1.Instrumentation{}
		if err := h.Client.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, p); err != nil {
			return nil
		}
		return p
	}
	var list v1.InstrumentationList
	if err := h.Client.List(ctx, &list, client.InNamespace(namespace)); err == nil && len(list.Items) == 1 {
		return &list.Items[0]
	}
	if h.OperatorNamespace == "" {
		return nil
	}
	p := &v1.Instrumentation{}
	if err := h.Client.Get(ctx, client.ObjectKey{Namespace: h.OperatorNamespace, Name: DefaultProfileName}, p); err != nil {
		return nil
	}
	return p
}

func (h *PodInjector) namespaceLabels(ctx context.Context, namespace string) map[string]string {
	h.mu.Lock()
	if h.nsLabels == nil {
		h.nsLabels = map[string]nsEntry{}
	}
	if e, ok := h.nsLabels[namespace]; ok && time.Since(e.at) < cacheTTL {
		h.mu.Unlock()
		return e.labels
	}
	h.mu.Unlock()
	ns := &corev1.Namespace{}
	labels := map[string]string{}
	if h.APIReader != nil {
		if err := h.APIReader.Get(ctx, client.ObjectKey{Name: namespace}, ns); err == nil {
			labels = ns.Labels
		}
	}
	h.mu.Lock()
	if len(h.nsLabels) > 1024 {
		h.nsLabels = map[string]nsEntry{}
	}
	h.nsLabels[namespace] = nsEntry{labels: labels, at: time.Now()}
	h.mu.Unlock()
	return labels
}

// otherInjectors lists APM injector webhooks other than this operator's own,
// refreshed once a minute. checked is false when they could not be listed.
func (h *PodInjector) otherInjectors(ctx context.Context) ([]readiness.Injector, bool) {
	h.mu.Lock()
	if time.Since(h.hooksAt) < cacheTTL {
		out := h.injectors
		h.mu.Unlock()
		return out, true
	}
	h.mu.Unlock()
	if h.APIReader == nil {
		return nil, false
	}
	var hooks admissionv1.MutatingWebhookConfigurationList
	if err := h.APIReader.List(ctx, &hooks); err != nil {
		return nil, false
	}
	var out []readiness.Injector
	for _, inj := range readiness.ClassifyWebhooks(hooks.Items) {
		if inj.Name == h.OwnWebhookName || (h.OwnWebhookName == "" && inj.Vendor == "collectorctrl") {
			continue
		}
		out = append(out, inj)
	}
	h.mu.Lock()
	h.injectors, h.hooksAt = out, time.Now()
	h.mu.Unlock()
	return out, true
}

// canaryTaken reports whether a non-terminating instrumented pod of the same
// controller and revision already exists. Best effort: the cache may lag a
// few hundred milliseconds behind, so two pods created at the same instant
// can both become canaries.
func (h *PodInjector) canaryTaken(ctx context.Context, namespace string, w instrument.Workload) bool {
	var pods corev1.PodList
	if err := h.Client.List(ctx, &pods, client.InNamespace(namespace), client.MatchingLabels{v1.LabelInstrumented: readiness.LangJava}); err != nil {
		return false
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.DeletionTimestamp != nil || p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		o := instrument.OwnerOf(p)
		if o.OwnerUID == w.OwnerUID && (w.Revision == "" || p.Annotations[v1.AnnotationInstrumentationRevision] == w.Revision) {
			return true
		}
	}
	return false
}

// imageEnv returns a lookup into image configuration, with the profile's
// registry credentials when it names a Secret.
func (h *PodInjector) imageEnv(ctx context.Context, p *v1.Instrumentation) instrument.ImageEnvLookup {
	var creds registry.CredentialSource
	if name := p.Spec.Registry.CredentialsSecret; name != "" && h.APIReader != nil {
		secret := &corev1.Secret{}
		if err := h.APIReader.Get(ctx, client.ObjectKey{Namespace: p.Namespace, Name: name}, secret); err == nil {
			raw := secret.Data[corev1.DockerConfigJsonKey]
			if raw == nil {
				raw = secret.Data[corev1.DockerConfigKey]
			}
			if raw != nil {
				if src, err := registry.DockerConfig(raw); err == nil {
					creds = src
				}
			}
		} else if !apierrors.IsNotFound(err) {
			logf.FromContext(ctx).Info("registry credentials secret not readable", "secret", p.Namespace+"/"+name, "error", err.Error())
		}
	}
	return func(image, name string) (string, bool, error) {
		cfg, err := h.Registry.Config(ctx, image, creds)
		if err != nil {
			return "", false, err
		}
		v, ok := cfg.Lookup(name)
		return v, ok, nil
	}
}

// ServiceDNSNames returns the names the serving certificate must cover.
func ServiceDNSNames(service, namespace string) []string {
	return []string{
		service,
		fmt.Sprintf("%s.%s", service, namespace),
		fmt.Sprintf("%s.%s.svc", service, namespace),
		fmt.Sprintf("%s.%s.svc.cluster.local", service, namespace),
	}
}
