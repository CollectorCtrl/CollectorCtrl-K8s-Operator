package controllers

import (
	"context"
	"testing"

	v1 "github.com/collectorctrl/collectorctrl/operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func fixture(t *testing.T, objects ...client.Object) *CollectorMonitorReconciler {
	t.Helper()
	s := runtime.NewScheme()
	_ = appsv1.AddToScheme(s)
	_ = corev1.AddToScheme(s)
	_ = v1.AddToScheme(s)
	return &CollectorMonitorReconciler{Client: fake.NewClientBuilder().WithScheme(s).WithObjects(objects...).Build(), Scheme: s}
}
func workload() *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "gateway", Namespace: "obs", UID: "workload", Labels: map[string]string{"app": "otel"}},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "collector", VolumeMounts: []corev1.VolumeMount{{Name: "config", MountPath: "/etc/otel"}}}},
			Volumes:    []corev1.Volume{{Name: "config", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "config"}}}}},
		}}},
	}
}
func monitor() *v1.CollectorMonitor {
	return &v1.CollectorMonitor{ObjectMeta: metav1.ObjectMeta{Namespace: "obs"}, Spec: v1.CollectorMonitorSpec{WorkloadSelector: v1.WorkloadSelector{MatchLabels: map[string]string{"app": "otel"}}}}
}
func TestAmbiguousWorkloadIsRejected(t *testing.T) {
	a, b := workload(), workload()
	b.Name = "second"
	b.UID = "second"
	r := fixture(t, a, b)
	if _, _, err := r.discoverWorkload(context.Background(), monitor()); err == nil {
		t.Fatal("arbitrary first workload selected")
	}
	m := monitor()
	m.Spec.WorkloadSelector.Name = "gateway"
	if w, _, err := r.discoverWorkload(context.Background(), m); err != nil || w.GetName() != "gateway" {
		t.Fatalf("explicit workload failed: %v", err)
	}
}
func TestMountedConfigDiscovery(t *testing.T) {
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "config", Namespace: "obs"}, Data: map[string]string{"relay.yaml": "receivers: {}"}}
	r := fixture(t, cm)
	got, key, err := r.discoverConfig(context.Background(), monitor(), workload(), "collector")
	if err != nil || got.Name != "config" || key != "relay.yaml" {
		t.Fatalf("mounted configuration unresolved: %v", err)
	}
	w := workload()
	w.Spec.Template.Spec.Volumes[0].ConfigMap.Items = []corev1.KeyToPath{{Key: "other", Path: "other"}}
	if _, _, err := r.discoverConfig(context.Background(), monitor(), w, "collector"); err == nil {
		t.Fatal("excluded key accepted as mounted")
	}
}
func TestProjectedConfigAndAmbiguousKeys(t *testing.T) {
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "config", Namespace: "obs"}, Data: map[string]string{"a.yaml": "a", "b.yaml": "b"}}
	r := fixture(t, cm)
	w := workload()
	w.Spec.Template.Spec.Volumes[0].VolumeSource = corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{Sources: []corev1.VolumeProjection{{ConfigMap: &corev1.ConfigMapProjection{LocalObjectReference: corev1.LocalObjectReference{Name: "config"}}}}}}
	m := monitor()
	if _, _, err := r.discoverConfig(context.Background(), m, w, "collector"); err == nil {
		t.Fatal("ambiguous keys guessed")
	}
	m.Spec.ConfigMapSelector = &v1.ConfigMapSelector{Key: "b.yaml"}
	if _, key, err := r.discoverConfig(context.Background(), m, w, "collector"); err != nil || key != "b.yaml" {
		t.Fatal(err)
	}
}
func TestSidecarRequiresContainerSelection(t *testing.T) {
	w := workload()
	w.Spec.Template.Spec.Containers = append(w.Spec.Template.Spec.Containers, corev1.Container{Name: "application"})
	m := monitor()
	if _, err := collectorContainer(m, w); err == nil {
		t.Fatal("collector container guessed in application pod")
	}
	m.Spec.CollectorContainer = "collector"
	if got, err := collectorContainer(m, w); err != nil || got != "collector" {
		t.Fatal(err)
	}
}
func TestPodsResolvedByOwnerUID(t *testing.T) {
	controller := true
	owner := func(uid types.UID) []metav1.OwnerReference {
		return []metav1.OwnerReference{{UID: uid, Controller: &controller}}
	}
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "rs", Namespace: "obs", UID: "rs", OwnerReferences: owner("workload")}}
	owned := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "owned", Namespace: "obs", OwnerReferences: owner("rs")}}
	foreign := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "foreign", Namespace: "obs", Labels: map[string]string{"app": "otel"}, OwnerReferences: owner("foreign-rs")}}
	r := fixture(t, rs, owned, foreign)
	pods, err := r.workloadPods(context.Background(), workload(), "Deployment")
	if err != nil || len(pods) != 1 || pods[0].Name != "owned" {
		t.Fatalf("incorrect ownership attribution: %+v %v", pods, err)
	}
}
func TestDeepCopyDoesNotMutateInformerCache(t *testing.T) {
	m := monitor()
	enabled := true
	m.Spec.EmergencyMode.Enabled = &enabled
	m.Status.ConfigMapRef = &v1.ConfigMapReference{Name: "original"}
	m.Status.Conditions = []metav1.Condition{{Type: "Active", Message: "original"}}
	copy := m.DeepCopy()
	copy.Spec.WorkloadSelector.MatchLabels["app"] = "changed"
	*copy.Spec.EmergencyMode.Enabled = false
	copy.Status.ConfigMapRef.Name = "changed"
	copy.Status.Conditions[0].Message = "changed"
	if m.Spec.WorkloadSelector.MatchLabels["app"] != "otel" || !*m.Spec.EmergencyMode.Enabled || m.Status.ConfigMapRef.Name != "original" || m.Status.Conditions[0].Message != "original" {
		t.Fatal("deep copy mutated shared informer objects")
	}
}

func TestConfigSubPathSelection(t *testing.T) {
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "config", Namespace: "obs"}, Data: map[string]string{"relay.yaml": "receivers: {}"}}
	r := fixture(t, cm)
	w := workload()
	mount := &w.Spec.Template.Spec.Containers[0].VolumeMounts[0]
	mount.SubPath = "other.yaml"
	if _, _, err := r.discoverConfig(context.Background(), monitor(), w, "collector"); err == nil {
		t.Fatal("unmounted subPath key accepted")
	}
	mount.SubPath = "collector.yaml"
	w.Spec.Template.Spec.Volumes[0].ConfigMap.Items = []corev1.KeyToPath{{Key: "relay.yaml", Path: "collector.yaml"}}
	if _, _, err := r.discoverConfig(context.Background(), monitor(), w, "collector"); err != nil {
		t.Fatal(err)
	}
	mount.SubPath = ""
	mount.SubPathExpr = "$(CONFIG_PATH)"
	if _, _, err := r.discoverConfig(context.Background(), monitor(), w, "collector"); err == nil {
		t.Fatal("dynamic mount path guessed")
	}
}

func TestObservePodLifecycle(t *testing.T) {
	pod := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "collector-0", UID: "old"}, Status: corev1.PodStatus{
		Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{Name: "collector", Ready: true, RestartCount: 3, Image: "otel:1"}},
	}}
	p := observePod(pod, "collector")
	if !p.Ready || p.UID != "old" || p.Restarts != 3 {
		t.Fatalf("lost current identity/status: %+v", p)
	}
	now := metav1.Now()
	pod.DeletionTimestamp = &now
	p = observePod(pod, "collector")
	if p.Ready || !p.Terminating || p.Phase != "Running" {
		t.Fatalf("lost deletion intent: %+v", p)
	}
	pod.DeletionTimestamp = nil
	for _, phase := range []corev1.PodPhase{corev1.PodSucceeded, corev1.PodFailed} {
		pod.Status.Phase = phase
		if observePod(pod, "collector").Ready {
			t.Fatalf("completed %s pod counted ready", phase)
		}
	}
	pod.UID = "replacement"
	if observePod(pod, "collector").UID != "replacement" {
		t.Fatal("same-name replacement lost UID")
	}
	if observePod(pod, "absent").Ready {
		t.Fatal("missing collector reported ready")
	}
}

func TestDeletedPodLeavesNextSnapshot(t *testing.T) {
	controller := true
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "old", Namespace: "obs", UID: "old", OwnerReferences: []metav1.OwnerReference{{UID: "workload", Controller: &controller}}}}
	r := fixture(t, pod)
	ctx := context.Background()
	pods, err := r.workloadPods(ctx, workload(), "StatefulSet")
	if err != nil || len(pods) != 1 {
		t.Fatalf("initial pods: %v %v", pods, err)
	}
	if err := r.Delete(ctx, pod); err != nil {
		t.Fatal(err)
	}
	replacement := pod.DeepCopy()
	replacement.ResourceVersion = ""
	replacement.UID = "replacement"
	if err := r.Create(ctx, replacement); err != nil {
		t.Fatal(err)
	}
	pods, err = r.workloadPods(ctx, workload(), "StatefulSet")
	if err != nil || len(pods) != 1 || pods[0].UID != "replacement" {
		t.Fatalf("old pod retained: %v %v", pods, err)
	}
}
