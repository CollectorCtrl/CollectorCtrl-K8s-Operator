// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 CollectorCtrl

package controllers

import (
	"context"
	"fmt"
	"sort"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/collectorctrl/collectorctrl/internal/instrument"
	"github.com/collectorctrl/collectorctrl/internal/readiness"
	v1 "github.com/collectorctrl/collectorctrl/operator/api/v1alpha1"
)

// InstrumentationReconciler keeps Instrumentation status current: which
// workload revisions use the profile, how many pods carry the agent, and
// whether a canary failed. A canary pod that restarts too often reverts its
// workload revision: the revert is recorded in status (the webhook then skips
// that revision) and, unless the profile says otherwise, the pod is deleted so
// its replacement starts without the agent.
type InstrumentationReconciler struct {
	client.Client
	// Now is overridable in tests.
	Now func() time.Time
}

const instrumentationRequeue = 2 * time.Minute

// Reconcile implements reconcile.Reconciler.
func (r *InstrumentationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx).WithName("instrumentation")
	profile := &v1.Instrumentation{}
	if err := r.Get(ctx, req.NamespacedName, profile); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.MatchingLabels{v1.LabelInstrumented: readiness.LangJava}); err != nil {
		return ctrl.Result{}, err
	}
	ref := profile.Namespace + "/" + profile.Name
	var mine []corev1.Pod
	for _, p := range pods.Items {
		if p.Annotations[v1.AnnotationInstrumentation] == ref {
			mine = append(mine, p)
		}
	}

	status, reverts := r.rollUp(profile, mine)
	status.Conditions = []metav1.Condition{{
		Type: "Ready", Status: metav1.ConditionTrue, Reason: "Active", ObservedGeneration: profile.Generation,
		Message:            fmt.Sprintf("%d pods instrumented across %d workload revisions", status.PodsInstrumented, len(status.Workloads)),
		LastTransitionTime: metav1.NewTime(r.now()),
	}}
	if len(profile.Status.Conditions) == 1 && profile.Status.Conditions[0].Status == metav1.ConditionTrue {
		status.Conditions[0].LastTransitionTime = profile.Status.Conditions[0].LastTransitionTime
	}
	status.ObservedGeneration = profile.Generation

	// Status first: the webhook reads the revert from status, so it must be
	// persisted before the failing pod is deleted and a replacement admitted.
	// A merge patch cannot hit a resourceVersion conflict from a stale cache.
	if !statusEqual(profile.Status, status) {
		original := profile.DeepCopy()
		profile.Status = status
		if err := r.Status().Patch(ctx, profile, client.MergeFrom(original)); err != nil {
			log.Error(err, "instrumentation status not written; reverts are not applied until it is")
			return ctrl.Result{}, err
		}
	}
	for _, rv := range reverts {
		log.Info("canary reverted", "workload", rv.workload.Kind+"/"+rv.workload.Name, "namespace", rv.pod.Namespace, "pod", rv.pod.Name, "reason", rv.reason, "deletePod", profile.AutoRevertEnabled())
		if profile.AutoRevertEnabled() {
			if err := r.Delete(ctx, &rv.pod, client.Preconditions{UID: &rv.pod.UID}); err != nil && !apierrors.IsNotFound(err) {
				log.Error(err, "failed to delete reverted canary pod", "pod", rv.pod.Namespace+"/"+rv.pod.Name)
			}
		}
	}
	return ctrl.Result{RequeueAfter: instrumentationRequeue}, nil
}

type revert struct {
	pod      corev1.Pod
	workload instrument.Workload
	reason   string
}

// rollUp computes the new status from the profile's pods, carrying reverted
// revisions forward while any pod of the workload still asks for the agent.
func (r *InstrumentationReconciler) rollUp(profile *v1.Instrumentation, pods []corev1.Pod) (v1.InstrumentationStatus, []revert) {
	type key struct{ ns, kind, name, rev string }
	groups := map[key]*v1.InstrumentedWorkload{}
	previous := map[key]v1.InstrumentedWorkload{}
	for _, w := range profile.Status.Workloads {
		previous[key{w.Namespace, w.Kind, w.Name, w.Revision}] = w
	}
	var reverts []revert
	now := metav1.NewTime(r.now())
	total := int32(0)
	for i := range pods {
		p := &pods[i]
		if p.DeletionTimestamp != nil || p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		w := instrument.OwnerOf(p)
		if rev := p.Annotations[v1.AnnotationInstrumentationRevision]; rev != "" {
			w.Revision = rev
		}
		k := key{p.Namespace, w.Kind, w.Name, w.Revision}
		g := groups[k]
		if g == nil {
			g = &v1.InstrumentedWorkload{Namespace: p.Namespace, Kind: w.Kind, Name: w.Name, Revision: w.Revision, Mode: v1.InstrumentModeCanary, State: v1.WorkloadStateCanary}
			if prev, ok := previous[k]; ok {
				g.State, g.Reason, g.LastTransition = prev.State, prev.Reason, prev.LastTransition
			}
			groups[k] = g
		}
		g.Pods++
		total++
		if p.Annotations[v1.AnnotationInstrumentationMode] == v1.InstrumentModeAll {
			g.Mode = v1.InstrumentModeAll
			if g.State != v1.WorkloadStateReverted {
				g.State = v1.WorkloadStateRolled
			}
		}
		if g.State == v1.WorkloadStateReverted {
			// Admitted with the agent in the moment between the revert and the
			// webhook seeing it: replace it like the canary itself. Only pods
			// created after the revert qualify; the reverted canary itself can
			// still show up once more before its deletion reaches the cache.
			if p.Annotations[v1.AnnotationInstrumentationMode] == v1.InstrumentModeCanary &&
				g.LastTransition != nil && p.CreationTimestamp.After(g.LastTransition.Time) {
				reverts = append(reverts, revert{pod: *p, workload: w, reason: g.Reason + " (replacement pod admitted before the revert was visible)"})
			}
			continue
		}
		if p.Annotations[v1.AnnotationInstrumentationMode] == v1.InstrumentModeCanary {
			if reason, failing := canaryFailing(p, profile.MaxRestartsOrDefault()); failing {
				g.State, g.Reason, g.LastTransition = v1.WorkloadStateReverted, reason, &now
				reverts = append(reverts, revert{pod: *p, workload: w, reason: reason})
			}
		}
	}
	// Keep reverted revisions that currently have no instrumented pod (the
	// failing pod was deleted): without the record the next pod would be
	// instrumented again. They are dropped once no pod of that revision asks.
	for k, prev := range previous {
		if prev.State != v1.WorkloadStateReverted {
			continue
		}
		if _, ok := groups[k]; ok {
			continue
		}
		if r.revisionStillRequests(prev) {
			prev.Pods = 0
			groups[k] = &prev
		}
	}
	out := v1.InstrumentationStatus{PodsInstrumented: total}
	for _, g := range groups {
		if g.LastTransition == nil {
			g.LastTransition = &now
			if prev, ok := previous[key{g.Namespace, g.Kind, g.Name, g.Revision}]; ok && prev.LastTransition != nil && prev.State == g.State {
				g.LastTransition = prev.LastTransition
			}
		}
		out.Workloads = append(out.Workloads, *g)
	}
	sort.Slice(out.Workloads, func(i, j int) bool {
		a, b := out.Workloads[i], out.Workloads[j]
		return a.Namespace+"/"+a.Kind+"/"+a.Name+"/"+a.Revision < b.Namespace+"/"+b.Kind+"/"+b.Name+"/"+b.Revision
	})
	return out, reverts
}

// revertGrace keeps a fresh revert record even when nothing can be seen
// asking for the agent: the failing pod is terminating and its replacement is
// not in the cache yet.
const revertGrace = 15 * time.Minute

// revisionStillRequests reports whether the reverted workload revision still
// exists and asks for the agent, in which case the record must stay or the
// next pod would be instrumented again. It decides from the owning object
// (the ReplicaSet with that pod-template-hash, or the StatefulSet/DaemonSet),
// not from pods, which come and go.
func (r *InstrumentationReconciler) revisionStillRequests(w v1.InstrumentedWorkload) bool {
	if w.LastTransition != nil && r.now().Sub(w.LastTransition.Time) < revertGrace {
		return true
	}
	ctx := context.Background()
	switch w.Kind {
	case "Deployment":
		var sets appsv1.ReplicaSetList
		if err := r.List(ctx, &sets, client.InNamespace(w.Namespace), client.MatchingLabels{"pod-template-hash": w.Revision}); err != nil {
			return true // be conservative: keep the revert
		}
		for i := range sets.Items {
			rs := &sets.Items[i]
			owner := metav1.GetControllerOf(rs)
			if owner != nil && owner.Kind == "Deployment" && owner.Name == w.Name && instrument.ModeOf(rs.Spec.Template.Labels) != "" {
				return true
			}
		}
		return false
	case "StatefulSet":
		sts := &appsv1.StatefulSet{}
		if err := r.Get(ctx, client.ObjectKey{Namespace: w.Namespace, Name: w.Name}, sts); err != nil {
			return !apierrors.IsNotFound(err)
		}
		return instrument.ModeOf(sts.Spec.Template.Labels) != "" && (w.Revision == "" || sts.Status.UpdateRevision == w.Revision || sts.Status.CurrentRevision == w.Revision)
	case "DaemonSet":
		ds := &appsv1.DaemonSet{}
		if err := r.Get(ctx, client.ObjectKey{Namespace: w.Namespace, Name: w.Name}, ds); err != nil {
			return !apierrors.IsNotFound(err)
		}
		return instrument.ModeOf(ds.Spec.Template.Labels) != ""
	}
	// Other owners (ReplicaSet, Job, bare Pod): keep while a pod of it exists.
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(w.Namespace), client.HasLabels{v1.LabelInstrumentJava}); err != nil {
		return true
	}
	for i := range pods.Items {
		o := instrument.OwnerOf(&pods.Items[i])
		if o.Kind == w.Kind && o.Name == w.Name && (w.Revision == "" || o.Revision == w.Revision) {
			return true
		}
	}
	return false
}

// canaryFailing reports whether a canary pod's containers restarted too often
// or sit in CrashLoopBackOff.
func canaryFailing(p *corev1.Pod, maxRestarts int32) (string, bool) {
	var restarts int32
	crash := ""
	// The agent init container counts too: a bad agent image fails there.
	statuses := append(append([]corev1.ContainerStatus(nil), p.Status.InitContainerStatuses...), p.Status.ContainerStatuses...)
	for _, cs := range statuses {
		restarts += cs.RestartCount
		if cs.State.Waiting != nil && cs.State.Waiting.Reason == "CrashLoopBackOff" {
			crash = cs.Name
		}
		if cs.LastTerminationState.Terminated != nil && cs.LastTerminationState.Terminated.Reason == "OOMKilled" && cs.RestartCount > 0 {
			crash = cs.Name + " (OOMKilled)"
		}
	}
	switch {
	case restarts >= maxRestarts:
		return fmt.Sprintf("canary pod %s restarted %d times with the agent", p.Name, restarts), true
	case crash != "" && restarts > 0:
		return fmt.Sprintf("canary pod %s: container %s is crash-looping with the agent", p.Name, crash), true
	}
	return "", false
}

func statusEqual(a, b v1.InstrumentationStatus) bool {
	if a.PodsInstrumented != b.PodsInstrumented || a.ObservedGeneration != b.ObservedGeneration || len(a.Workloads) != len(b.Workloads) || len(a.Conditions) != len(b.Conditions) {
		return false
	}
	for i := range a.Workloads {
		x, y := a.Workloads[i], b.Workloads[i]
		x.LastTransition, y.LastTransition = nil, nil
		if x != y {
			return false
		}
	}
	for i := range a.Conditions {
		x, y := a.Conditions[i], b.Conditions[i]
		if x.Type != y.Type || x.Status != y.Status || x.Reason != y.Reason || x.Message != y.Message {
			return false
		}
	}
	return true
}

func (r *InstrumentationReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// SetupWithManager watches Instrumentations and the pods that carry an agent.
func (r *InstrumentationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	podToProfile := handler.EnqueueRequestsFromMapFunc(func(_ context.Context, obj client.Object) []reconcile.Request {
		ref := obj.GetAnnotations()[v1.AnnotationInstrumentation]
		if ref == "" {
			return nil
		}
		ns, name := instrument.ProfileRef(map[string]string{v1.AnnotationInstrumentation: ref})
		if ns == "" || name == "" {
			return nil
		}
		return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}}}
	})
	instrumented := predicate.NewPredicateFuncs(func(obj client.Object) bool {
		return obj.GetLabels()[v1.LabelInstrumented] != ""
	})
	return ctrl.NewControllerManagedBy(mgr).
		Named("instrumentation").
		For(&v1.Instrumentation{}).
		Watches(&corev1.Pod{}, podToProfile, builder.WithPredicates(instrumented, predicate.Funcs{
			CreateFunc: func(event.CreateEvent) bool { return true },
			DeleteFunc: func(event.DeleteEvent) bool { return true },
			UpdateFunc: func(e event.UpdateEvent) bool {
				// Only status changes matter: restarts, phase, deletion.
				o, n := e.ObjectOld.(*corev1.Pod), e.ObjectNew.(*corev1.Pod)
				return o.Status.Phase != n.Status.Phase || restarts(o) != restarts(n) || (o.DeletionTimestamp == nil) != (n.DeletionTimestamp == nil)
			},
		})).
		Complete(r)
}

func restarts(p *corev1.Pod) int32 {
	var n int32
	for _, cs := range p.Status.InitContainerStatuses {
		n += cs.RestartCount
	}
	for _, cs := range p.Status.ContainerStatuses {
		n += cs.RestartCount
	}
	return n
}
