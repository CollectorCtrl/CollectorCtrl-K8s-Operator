package controllers

import (
	"context"
	"sort"
	"time"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/collectorctrl/collectorctrl/internal/readiness"
	v1 "github.com/collectorctrl/collectorctrl/operator/api/v1alpha1"
	"github.com/collectorctrl/collectorctrl/pkg/opamp"
)

// maxReadinessWorkloads bounds one readiness message.
const maxReadinessWorkloads = 2000

// DefaultReadinessExclusions are namespaces never scanned for applications.
var DefaultReadinessExclusions = []string{"kube-system", "kube-public", "kube-node-lease"}

// Readiness builds the instrumentation readiness report: for every
// application workload, which language it runs, which APM agents are loaded
// or requested, which injector webhooks select it, and which launcher
// variables an injector would have to merge with. It reads Kubernetes objects
// only. It never reads Secrets or contacts an image registry.
func (o *ClusterObserver) Readiness(ctx context.Context) (opamp.InstrumentationReadiness, error) {
	r := o.Monitors
	rep := opamp.InstrumentationReadiness{
		Source: "kubernetes-api", ObservedAt: time.Now().UTC(), ClusterID: r.ClusterID, ClusterName: o.clusterName(),
		RegistryChecked: false, Injectors: []readiness.Injector{}, Workloads: []opamp.ReadinessWorkload{}, Summary: map[string]int{},
	}

	// Cluster-scoped reads go to the API server directly: they are small, run
	// every few minutes, and must fail softly in namespace-scoped installs.
	reader := o.APIReader
	if reader == nil {
		reader = r.Client
	}
	nsLabels, nsAnnotations := map[string]map[string]string{}, map[string]map[string]string{}
	var namespaces corev1.NamespaceList
	nsErr := reader.List(ctx, &namespaces)
	if nsErr == nil {
		for _, ns := range namespaces.Items {
			nsLabels[ns.Name], nsAnnotations[ns.Name] = ns.Labels, ns.Annotations
		}
	}
	var hooks admissionv1.MutatingWebhookConfigurationList
	if err := reader.List(ctx, &hooks); err == nil && nsErr == nil {
		rep.InjectorsChecked = true
		rep.Injectors = readiness.ClassifyWebhooks(hooks.Items)
	}

	podsByOwner, err := o.podsByOwner(ctx)
	if err != nil {
		return rep, err
	}
	excluded := map[string]bool{}
	for _, ns := range o.ReadinessExclude {
		excluded[ns] = true
	}

	add := func(obj client.Object, kind string, tmpl corev1.PodTemplateSpec) {
		ns := obj.GetNamespace()
		if excluded[ns] {
			return
		}
		in := readiness.Input{
			Template: tmpl, WorkloadAnnotations: obj.GetAnnotations(), NamespaceLabels: nsLabels[ns], NamespaceAnnotations: nsAnnotations[ns],
			Injectors: rep.Injectors, InjectorsChecked: rep.InjectorsChecked,
			Lookup: func(name, key string) (string, bool) {
				cm := &corev1.ConfigMap{}
				if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, cm); err != nil {
					return "", false
				}
				v, ok := cm.Data[key]
				return v, ok
			},
			LookupAll: func(name string) (map[string]string, bool) {
				cm := &corev1.ConfigMap{}
				if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, cm); err != nil {
					return nil, false
				}
				return cm.Data, true
			},
		}
		pod := newestRunning(podsByOwner[obj.GetUID()])
		in.Pod = pod
		containers := readiness.Analyze(in)
		if len(containers) == 0 {
			return // collectors and other infrastructure only
		}
		stack, source := stackOf(&v1.CollectorMonitor{}, obj)
		w := opamp.ReadinessWorkload{Namespace: ns, Kind: kind, Name: obj.GetName(), UID: string(obj.GetUID()),
			Stack: stack, StackSource: source, Containers: containers}
		if pod != nil {
			w.Pod = pod.Name
		}
		verdicts := make([]string, 0, len(containers))
		for _, c := range containers {
			verdicts = append(verdicts, c.Verdict)
		}
		w.Verdict = readiness.Worst(verdicts...)
		rep.Workloads = append(rep.Workloads, w)
	}

	var deps appsv1.DeploymentList
	if err := r.List(ctx, &deps); err != nil {
		return rep, err
	}
	for i := range deps.Items {
		add(&deps.Items[i], "Deployment", deps.Items[i].Spec.Template)
	}
	var sts appsv1.StatefulSetList
	if err := r.List(ctx, &sts); err != nil {
		return rep, err
	}
	for i := range sts.Items {
		add(&sts.Items[i], "StatefulSet", sts.Items[i].Spec.Template)
	}
	var ds appsv1.DaemonSetList
	if err := r.List(ctx, &ds); err != nil {
		return rep, err
	}
	for i := range ds.Items {
		add(&ds.Items[i], "DaemonSet", ds.Items[i].Spec.Template)
	}

	sort.Slice(rep.Workloads, func(i, j int) bool {
		a, b := rep.Workloads[i], rep.Workloads[j]
		return a.Namespace+"/"+a.Kind+"/"+a.Name < b.Namespace+"/"+b.Kind+"/"+b.Name
	})
	for _, w := range rep.Workloads {
		rep.Summary[w.Verdict]++
	}
	if len(rep.Workloads) > maxReadinessWorkloads {
		rep.Workloads, rep.Truncated = rep.Workloads[:maxReadinessWorkloads], true
	}
	return rep, nil
}

// podsByOwner indexes pods by their workload's UID (Deployment through its
// ReplicaSets), listing pods and ReplicaSets once per scan.
func (o *ClusterObserver) podsByOwner(ctx context.Context) (map[types.UID][]corev1.Pod, error) {
	r := o.Monitors
	var replicas appsv1.ReplicaSetList
	if err := r.List(ctx, &replicas); err != nil {
		return nil, err
	}
	rsOwner := map[types.UID]types.UID{}
	for _, rs := range replicas.Items {
		if owner := metav1.GetControllerOf(&rs); owner != nil && owner.Kind == "Deployment" {
			rsOwner[rs.UID] = owner.UID
		}
	}
	var pods corev1.PodList
	if err := r.List(ctx, &pods); err != nil {
		return nil, err
	}
	out := map[types.UID][]corev1.Pod{}
	for _, p := range pods.Items {
		owner := metav1.GetControllerOf(&p)
		if owner == nil {
			continue
		}
		uid := owner.UID
		if d, ok := rsOwner[uid]; ok {
			uid = d
		}
		out[uid] = append(out[uid], p)
	}
	return out, nil
}

// newestRunning picks the most recently created running, non-terminating pod:
// it reflects the current template and whatever webhooks injected.
func newestRunning(pods []corev1.Pod) *corev1.Pod {
	var best *corev1.Pod
	for i := range pods {
		p := &pods[i]
		if p.Status.Phase != corev1.PodRunning || p.DeletionTimestamp != nil {
			continue
		}
		if best == nil || p.CreationTimestamp.After(best.CreationTimestamp.Time) ||
			(p.CreationTimestamp.Equal(&best.CreationTimestamp) && p.Name > best.Name) {
			best = p
		}
	}
	return best
}
