// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 CollectorCtrl

package controllers

import (
	"context"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
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

	v1 "github.com/collectorctrl/collectorctrl/operator/api/v1alpha1"
)

// labelPrefix marks connection keys owned by label discovery. "@" cannot appear
// in a Kubernetes object name, so these keys never collide with a monitor's.
const labelPrefix = "@label/"

// LabelReconciler observes collector workloads that opt in with the label
// collectorctrl.io/observe=true. It builds the monitor in memory, so nobody
// writes a CollectorMonitor. An explicit CollectorMonitor for the same
// workload takes precedence. It shares connections with the CollectorMonitor
// reconciler and, like it, never writes to workloads.
type LabelReconciler struct {
	*CollectorMonitorReconciler
	Kind    string
	newObj  func() client.Object
	newList func() client.ObjectList
}

// NewLabelReconcilers returns one reconciler per supported workload kind.
func NewLabelReconcilers(base *CollectorMonitorReconciler) []*LabelReconciler {
	return []*LabelReconciler{
		{CollectorMonitorReconciler: base, Kind: "DaemonSet", newObj: func() client.Object { return &appsv1.DaemonSet{} }, newList: func() client.ObjectList { return &appsv1.DaemonSetList{} }},
		{CollectorMonitorReconciler: base, Kind: "Deployment", newObj: func() client.Object { return &appsv1.Deployment{} }, newList: func() client.ObjectList { return &appsv1.DeploymentList{} }},
		{CollectorMonitorReconciler: base, Kind: "StatefulSet", newObj: func() client.Object { return &appsv1.StatefulSet{} }, newList: func() client.ObjectList { return &appsv1.StatefulSetList{} }},
	}
}

func optedIn(o client.Object) bool { return o != nil && o.GetLabels()[LabelObserve] == "true" }

func (l *LabelReconciler) connectionKey(namespace, name string) string {
	return namespace + "/" + labelPrefix + l.Kind + "/" + name
}

func (l *LabelReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx).WithValues("kind", l.Kind, "workload", req.NamespacedName)
	key := l.connectionKey(req.Namespace, req.Name)
	obj := l.newObj()
	if err := l.Get(ctx, req.NamespacedName, obj); err != nil {
		if apierrors.IsNotFound(err) {
			l.closeConnection(key)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	if !optedIn(obj) {
		l.closeConnection(key)
		return ctrl.Result{}, nil
	}
	owned, err := l.ownedByMonitor(ctx, obj)
	if err != nil {
		return ctrl.Result{}, err
	}
	if owned != "" {
		l.closeConnection(key)
		log.V(1).Info("workload is observed by a CollectorMonitor; label discovery defers to it", "monitor", owned)
		return ctrl.Result{}, nil
	}
	monitor := syntheticMonitor(obj, l.Kind, l.DefaultServer)
	requeue, condition, err := l.observe(ctx, monitor, discoveryLabel)
	if err != nil {
		l.closeConnection(key)
		log.Error(err, "label-discovered collector could not be observed", "step", condition)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	return ctrl.Result{RequeueAfter: requeue}, nil
}

// ownedByMonitor returns the name of a CollectorMonitor that resolves to this
// workload, if any.
func (l *LabelReconciler) ownedByMonitor(ctx context.Context, obj client.Object) (string, error) {
	var monitors v1.CollectorMonitorList
	if err := l.List(ctx, &monitors, client.InNamespace(obj.GetNamespace())); err != nil {
		return "", err
	}
	for i := range monitors.Items {
		w, _, err := l.discoverWorkload(ctx, &monitors.Items[i])
		if err == nil && w.GetUID() == obj.GetUID() {
			return monitors.Items[i].Name, nil
		}
	}
	return "", nil
}

// syntheticMonitor builds the in-memory monitor for a labelled workload from its
// optional annotations. It is never written to the API server.
func syntheticMonitor(obj client.Object, kind, server string) *v1.CollectorMonitor {
	ann := obj.GetAnnotations()
	get := func(k string) string { return strings.TrimSpace(ann[k]) }
	m := &v1.CollectorMonitor{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:   obj.GetNamespace(),
			Name:        labelPrefix + kind + "/" + obj.GetName(),
			Annotations: map[string]string{},
		},
		Spec: v1.CollectorMonitorSpec{
			WorkloadSelector:   v1.WorkloadSelector{Kind: kind, Name: obj.GetName()},
			CollectorContainer: get(AnnContainer),
			ReportConfig:       strings.EqualFold(get(AnnReportConfig), "true"),
			OpAMPServer:        server,
		},
	}
	if name, key := get(AnnConfigMap), get(AnnConfigKey); name != "" || key != "" {
		m.Spec.ConfigMapSelector = &v1.ConfigMapSelector{Name: name, Key: key}
	}
	if d, err := time.ParseDuration(get(AnnReportInterval)); err == nil {
		m.Spec.HealthCheck.Interval = metav1.Duration{Duration: d}
	}
	return m
}

func (l *LabelReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Pass updates where the label was added or removed, so removal closes the
	// connection, and deletions of previously labelled workloads.
	labelled := predicate.Funcs{
		CreateFunc:  func(e event.CreateEvent) bool { return optedIn(e.Object) },
		UpdateFunc:  func(e event.UpdateEvent) bool { return optedIn(e.ObjectOld) || optedIn(e.ObjectNew) },
		DeleteFunc:  func(e event.DeleteEvent) bool { return optedIn(e.Object) },
		GenericFunc: func(e event.GenericEvent) bool { return optedIn(e.Object) },
	}
	related := handler.EnqueueRequestsFromMapFunc(l.mapNamespace)
	return ctrl.NewControllerManagedBy(mgr).
		Named("observe-"+strings.ToLower(l.Kind)).
		For(l.newObj(), builder.WithPredicates(labelled)).
		Watches(&corev1.ConfigMap{}, related).
		Watches(&corev1.Pod{}, related).
		Watches(&v1.CollectorMonitor{}, related).
		Complete(l)
}

// mapNamespace enqueues the labelled workloads of this kind in the namespace
// of a changed ConfigMap, Pod or CollectorMonitor.
func (l *LabelReconciler) mapNamespace(ctx context.Context, obj client.Object) []reconcile.Request {
	list := l.newList()
	if err := l.List(ctx, list, client.InNamespace(obj.GetNamespace()), client.MatchingLabels{LabelObserve: "true"}); err != nil {
		return nil
	}
	items, err := meta.ExtractList(list)
	if err != nil {
		return nil
	}
	out := make([]reconcile.Request, 0, len(items))
	for _, item := range items {
		if o, ok := item.(client.Object); ok {
			out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: o.GetNamespace(), Name: o.GetName()}})
		}
	}
	return out
}
