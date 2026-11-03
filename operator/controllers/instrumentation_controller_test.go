// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 CollectorCtrl

package controllers

import (
	"context"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/collectorctrl/collectorctrl/internal/instrument"
	v1 "github.com/collectorctrl/collectorctrl/operator/api/v1alpha1"
)

func instrumentedPod(name, mode string, restarts int32) *corev1.Pod {
	ctrl := true
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "shop", UID: types.UID("uid-" + name),
		Labels:          map[string]string{v1.LabelInstrumented: "java", v1.LabelInstrumentJava: mode, "pod-template-hash": "abc12"},
		Annotations:     map[string]string{v1.AnnotationInstrumentation: "collectorctrl-operator/default", v1.AnnotationInstrumentationMode: mode, v1.AnnotationInstrumentationRevision: "abc12", v1.AnnotationInstrumentationStatus: "injected"},
		OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "orders-abc12", UID: "rs-1", Controller: &ctrl}}},
		Spec:   corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "acme/orders:1"}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{Name: "app", RestartCount: restarts}}}}
}

func profileFixture(t *testing.T, objects ...client.Object) (*InstrumentationReconciler, client.Client) {
	t.Helper()
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	_ = appsv1.AddToScheme(s)
	_ = v1.AddToScheme(s)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objects...).WithStatusSubresource(&v1.Instrumentation{}).Build()
	return &InstrumentationReconciler{Client: c, Now: func() time.Time { return time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC) }}, c
}

func reconcileProfile(t *testing.T, r *InstrumentationReconciler, c client.Client) *v1.Instrumentation {
	t.Helper()
	key := types.NamespacedName{Namespace: "collectorctrl-operator", Name: "default"}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	out := &v1.Instrumentation{}
	if err := c.Get(context.Background(), key, out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestInstrumentationStatusRollsUpWorkloads(t *testing.T) {
	profile := &v1.Instrumentation{ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "collectorctrl-operator", Generation: 2},
		Spec: v1.InstrumentationSpec{Exporter: v1.ExporterSpec{Endpoint: "http://gw:4318"}}}
	other := &v1.Instrumentation{ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "shop"}, Spec: profile.Spec}
	foreign := instrumentedPod("orders-abc12-z", "all", 0)
	foreign.Annotations[v1.AnnotationInstrumentation] = "shop/other"
	r, c := profileFixture(t, profile, other, instrumentedPod("orders-abc12-a", "all", 0), instrumentedPod("orders-abc12-b", "all", 1), foreign)
	out := reconcileProfile(t, r, c)
	if out.Status.PodsInstrumented != 2 || len(out.Status.Workloads) != 1 || out.Status.ObservedGeneration != 2 {
		t.Fatalf("status %+v", out.Status)
	}
	w := out.Status.Workloads[0]
	if w.Kind != "Deployment" || w.Name != "orders" || w.Revision != "abc12" || w.Mode != "all" || w.State != v1.WorkloadStateRolled || w.Pods != 2 {
		t.Fatalf("workload %+v", w)
	}
	if len(out.Status.Conditions) != 1 || out.Status.Conditions[0].Status != metav1.ConditionTrue || !strings.Contains(out.Status.Conditions[0].Message, "2 pods") {
		t.Fatalf("conditions %+v", out.Status.Conditions)
	}
}

func TestFailingCanaryIsRevertedAndStaysReverted(t *testing.T) {
	two := int32(2)
	profile := &v1.Instrumentation{ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "collectorctrl-operator"},
		Spec: v1.InstrumentationSpec{Exporter: v1.ExporterSpec{Endpoint: "http://gw:4318"}, Canary: v1.CanarySpec{MaxRestarts: &two}}}
	canary := instrumentedPod("orders-abc12-c", "canary", 2)
	// An uninstrumented sibling that still asks for the agent keeps the revert alive.
	sibling := instrumentedPod("orders-abc12-s", "canary", 0)
	delete(sibling.Labels, v1.LabelInstrumented)
	sibling.Annotations = map[string]string{v1.AnnotationInstrumentationStatus: "skipped"}
	r, c := profileFixture(t, profile, canary, sibling)

	out := reconcileProfile(t, r, c)
	if len(out.Status.Workloads) != 1 || out.Status.Workloads[0].State != v1.WorkloadStateReverted || !strings.Contains(out.Status.Workloads[0].Reason, "restarted 2 times") {
		t.Fatalf("status %+v", out.Status)
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(canary), &corev1.Pod{}); !apierrors.IsNotFound(err) {
		t.Fatalf("canary pod should have been deleted, got %v", err)
	}
	// The webhook consults the same status.
	if _, ok := out.Reverted("shop", "Deployment", "orders", "abc12"); !ok {
		t.Fatal("Reverted() does not see the record")
	}
	// Next reconcile: the pod is gone but the revision still has a pod asking, so the record stays.
	out = reconcileProfile(t, r, c)
	if len(out.Status.Workloads) != 1 || out.Status.Workloads[0].State != v1.WorkloadStateReverted || out.Status.Workloads[0].Pods != 0 || out.Status.PodsInstrumented != 0 {
		t.Fatalf("status after deletion %+v", out.Status)
	}
	// Once the revision is gone (no ReplicaSet of that hash, no pod) and the
	// grace period has passed, the record is dropped.
	if err := c.Delete(context.Background(), sibling); err != nil {
		t.Fatal(err)
	}
	r.Now = func() time.Time { return time.Date(2026, 11, 3, 13, 0, 0, 0, time.UTC) }
	out = reconcileProfile(t, r, c)
	if len(out.Status.Workloads) != 0 {
		t.Fatalf("stale revert kept: %+v", out.Status.Workloads)
	}
}

func TestAutoRevertOffKeepsPod(t *testing.T) {
	off := false
	profile := &v1.Instrumentation{ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "collectorctrl-operator"},
		Spec: v1.InstrumentationSpec{Exporter: v1.ExporterSpec{Endpoint: "http://gw:4318"}, Canary: v1.CanarySpec{AutoRevert: &off}}}
	canary := instrumentedPod("orders-abc12-c", "canary", 0)
	canary.Status.ContainerStatuses[0].RestartCount = 1
	canary.Status.ContainerStatuses[0].State.Waiting = &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}
	r, c := profileFixture(t, profile, canary)
	out := reconcileProfile(t, r, c)
	if out.Status.Workloads[0].State != v1.WorkloadStateReverted || !strings.Contains(out.Status.Workloads[0].Reason, "crash-looping") {
		t.Fatalf("status %+v", out.Status)
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(canary), &corev1.Pod{}); err != nil {
		t.Fatalf("pod should be kept with autoRevert=false: %v", err)
	}
}

func TestHealthyCanaryIsNotReverted(t *testing.T) {
	profile := &v1.Instrumentation{ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "collectorctrl-operator"},
		Spec: v1.InstrumentationSpec{Exporter: v1.ExporterSpec{Endpoint: "http://gw:4318"}}}
	r, c := profileFixture(t, profile, instrumentedPod("orders-abc12-c", "canary", 1))
	out := reconcileProfile(t, r, c)
	if w := out.Status.Workloads[0]; w.State != v1.WorkloadStateCanary || w.Pods != 1 || w.Mode != "canary" {
		t.Fatalf("%+v", w)
	}
	if instrument.OwnerOf(instrumentedPod("x", "canary", 0)).Name != "orders" {
		t.Fatal("owner derivation")
	}
}

func TestFailingAgentInitContainerRevertsCanary(t *testing.T) {
	profile := &v1.Instrumentation{ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "collectorctrl-operator"},
		Spec: v1.InstrumentationSpec{Exporter: v1.ExporterSpec{Endpoint: "http://gw:4318"}}}
	canary := instrumentedPod("orders-abc12-c", "canary", 0)
	canary.Status.Phase = corev1.PodPending
	canary.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: instrument.InitContainerName, RestartCount: 3,
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}}}
	r, c := profileFixture(t, profile, canary)
	out := reconcileProfile(t, r, c)
	if len(out.Status.Workloads) != 1 || out.Status.Workloads[0].State != v1.WorkloadStateReverted {
		t.Fatalf("init container failure not treated as a failing canary: %+v", out.Status)
	}
}

func TestRevertIsWrittenBeforePodDeletionAndStragglersAreReplaced(t *testing.T) {
	two := int32(2)
	profile := &v1.Instrumentation{ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "collectorctrl-operator"},
		Spec: v1.InstrumentationSpec{Exporter: v1.ExporterSpec{Endpoint: "http://gw:4318"}, Canary: v1.CanarySpec{MaxRestarts: &two}}}
	failing := instrumentedPod("flaky-abc12-a", "canary", 2)
	failing.CreationTimestamp = metav1.NewTime(time.Date(2026, 11, 3, 11, 0, 0, 0, time.UTC))
	r, c := profileFixture(t, profile, failing)
	out := reconcileProfile(t, r, c)
	if out.Status.Workloads[0].State != v1.WorkloadStateReverted {
		t.Fatalf("%+v", out.Status)
	}
	// The deleted canary can be seen once more before the deletion reaches the
	// cache: it was created before the revert, so it is not reverted again.
	ghost := instrumentedPod("flaky-abc12-a", "canary", 2)
	ghost.CreationTimestamp = failing.CreationTimestamp
	if err := c.Create(context.Background(), ghost); err != nil {
		t.Fatal(err)
	}
	if _, reverts := r.rollUp(out, []corev1.Pod{*ghost}); len(reverts) != 0 {
		t.Fatalf("reverted canary flagged again: %+v", reverts)
	}
	if err := c.Delete(context.Background(), ghost); err != nil {
		t.Fatal(err)
	}
	// A replacement that still got the agent (webhook raced the revert) is deleted
	// on the next reconcile even though it has not failed yet.
	straggler := instrumentedPod("flaky-abc12-b", "canary", 0)
	straggler.CreationTimestamp = metav1.NewTime(time.Date(2026, 11, 3, 12, 0, 1, 0, time.UTC))
	if err := c.Create(context.Background(), straggler); err != nil {
		t.Fatal(err)
	}
	out = reconcileProfile(t, r, c)
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(straggler), &corev1.Pod{}); !apierrors.IsNotFound(err) {
		t.Fatalf("straggler should be deleted, got %v", err)
	}
	if out.Status.Workloads[0].State != v1.WorkloadStateReverted || !strings.Contains(out.Status.Workloads[0].Reason, "restarted 2 times") {
		t.Fatalf("revert record changed: %+v", out.Status.Workloads[0])
	}
	// A pod of the revision without the agent (the webhook skipped it) is left alone and keeps the record alive.
	clean := instrumentedPod("flaky-abc12-c", "canary", 0)
	delete(clean.Labels, v1.LabelInstrumented)
	clean.Annotations = map[string]string{v1.AnnotationInstrumentationStatus: "skipped"}
	if err := c.Create(context.Background(), clean); err != nil {
		t.Fatal(err)
	}
	out = reconcileProfile(t, r, c)
	if len(out.Status.Workloads) != 1 || out.Status.Workloads[0].State != v1.WorkloadStateReverted {
		t.Fatalf("record lost: %+v", out.Status)
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(clean), &corev1.Pod{}); err != nil {
		t.Fatalf("clean pod must not be deleted: %v", err)
	}
}

// The sequence seen on a real cluster: the failing canary is deleted, the
// replacement is not in the cache yet, and a reconcile runs in between. The
// record must survive because the ReplicaSet of that revision still asks.
func TestRevertSurvivesTheGapBetweenPodDeletionAndReplacement(t *testing.T) {
	two := int32(2)
	ctrl := true
	profile := &v1.Instrumentation{ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "collectorctrl-operator"},
		Spec: v1.InstrumentationSpec{Exporter: v1.ExporterSpec{Endpoint: "http://gw:4318"}, Canary: v1.CanarySpec{MaxRestarts: &two}}}
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "orders-abc12", Namespace: "shop", Labels: map[string]string{"pod-template-hash": "abc12"},
		OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: "orders", UID: "d-1", Controller: &ctrl}}}}
	rs.Spec.Template.Labels = map[string]string{"app": "orders", v1.LabelInstrumentJava: "canary", "pod-template-hash": "abc12"}
	failing := instrumentedPod("orders-abc12-a", "canary", 2)
	r, c := profileFixture(t, profile, rs, failing)
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	r.Now = func() time.Time { return now }

	out := reconcileProfile(t, r, c) // reverts and deletes the pod
	if out.Status.Workloads[0].State != v1.WorkloadStateReverted {
		t.Fatalf("%+v", out.Status)
	}
	// Gap: no pod at all. Within the grace period the record stays regardless.
	out = reconcileProfile(t, r, c)
	if len(out.Status.Workloads) != 1 {
		t.Fatalf("record dropped in the gap: %+v", out.Status)
	}
	// Long after the grace period the ReplicaSet still exists and asks: keep.
	now = now.Add(time.Hour)
	out = reconcileProfile(t, r, c)
	if len(out.Status.Workloads) != 1 || out.Status.Workloads[0].State != v1.WorkloadStateReverted {
		t.Fatalf("record dropped although the ReplicaSet still asks: %+v", out.Status)
	}
	// The user rolls out a new revision (old ReplicaSet scaled away and removed): drop.
	if err := c.Delete(context.Background(), rs); err != nil {
		t.Fatal(err)
	}
	out = reconcileProfile(t, r, c)
	if len(out.Status.Workloads) != 0 {
		t.Fatalf("stale record kept: %+v", out.Status)
	}
}
