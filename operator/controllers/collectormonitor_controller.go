package controllers

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1 "github.com/collectorctrl/collectorctrl/operator/api/v1alpha1"
	"github.com/collectorctrl/collectorctrl/pkg/api"
	"github.com/collectorctrl/collectorctrl/pkg/opamp"
)

// CollectorMonitorReconciler observes existing workloads. It never writes to them.
type CollectorMonitorReconciler struct {
	client.Client
	Reader           client.Reader
	Scheme           *runtime.Scheme
	DefaultSecretKey string
	DefaultServer    string
	ClusterID        string
	TLSConfig        *tls.Config
	lifetime         context.Context
	mu               sync.Mutex
	connections      map[string]*connection
}

type connection struct {
	client      *opamp.Client
	fingerprint [32]byte
}

// +kubebuilder:rbac:groups=collectorctrl.io,resources=collectormonitors,verbs=get;list;watch
// +kubebuilder:rbac:groups=collectorctrl.io,resources=collectormonitors/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps,resources=daemonsets;deployments;statefulsets;replicasets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=configmaps;pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get

func (r *CollectorMonitorReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	monitor := &v1.CollectorMonitor{}
	if err := r.Get(ctx, req.NamespacedName, monitor); err != nil {
		if apierrors.IsNotFound(err) {
			r.closeConnection(req.String())
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	fail := func(condition string, err error) (ctrl.Result, error) {
		r.closeConnection(req.String())
		monitor.Status.Phase = "Error"
		monitor.Status.AgentCount, monitor.Status.HealthyAgents = 0, 0
		monitor.Status.ConfigMapRef = nil
		r.condition(monitor, condition, metav1.ConditionFalse, "ObservationFailed", err.Error())
		r.condition(monitor, "Active", metav1.ConditionFalse, "ObservationFailed", err.Error())
		r.condition(monitor, "OpAMPConnected", metav1.ConditionFalse, "ObservationFailed", "Observation connection stopped")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, r.Status().Update(ctx, monitor)
	}
	workload, kind, err := r.discoverWorkload(ctx, monitor)
	if err != nil {
		return fail("Discovered", err)
	}
	r.condition(monitor, "Discovered", metav1.ConditionTrue, "WorkloadResolved", kind+"/"+workload.GetName())
	container, err := collectorContainer(monitor, workload)
	if err != nil {
		return fail("ConfigMapResolved", err)
	}
	cm, configKey, err := r.discoverConfig(ctx, monitor, workload, container)
	if err != nil {
		return fail("ConfigMapResolved", err)
	}
	monitor.Status.ConfigMapRef = &v1.ConfigMapReference{Name: cm.Name, Namespace: cm.Namespace, Key: configKey}
	r.condition(monitor, "ConfigMapResolved", metav1.ConditionTrue, "MountedConfigResolved", "Observed mounted ConfigMap; runtime activation is unverified")
	r.condition(monitor, "RuntimeConfigVerified", metav1.ConditionUnknown, "RuntimeEvidenceUnavailable", "ConfigMap contents and pod annotations do not prove the running collector's loaded configuration")
	r.condition(monitor, "TelemetryDeliveryVerified", metav1.ConditionUnknown, "MetricsUnavailable", "Pod readiness does not prove telemetry delivery")
	if monitor.Spec.EmergencyMode.Enabled != nil && *monitor.Spec.EmergencyMode.Enabled {
		r.condition(monitor, "ControlEnabled", metav1.ConditionFalse, "ObservationOnly", "Emergency control is unsupported in this observation release")
	}
	c, err := r.ensureConnection(ctx, monitor, workload, kind, container, cm)
	if err != nil {
		return fail("OpAMPConnected", err)
	}
	pods, err := r.workloadPods(ctx, workload, kind)
	if err != nil {
		return fail("HealthReported", err)
	}
	observation := opamp.Observation{
		Source: "kubernetes-api", RuntimeVerified: false, ObservedAt: time.Now().UTC(),
		Namespace: monitor.Namespace, WorkloadKind: kind, WorkloadName: workload.GetName(),
		WorkloadUID: string(workload.GetUID()), CollectorContainer: container,
		ConfigMapName: cm.Name, ConfigMapKey: configKey, ResourceVersion: cm.ResourceVersion,
		ConfigHash:  fmt.Sprintf("%x", sha256.Sum256([]byte(cm.Data[configKey]))),
		DesiredPods: desiredPods(workload), Pods: make([]opamp.PodHealth, 0, len(pods)),
	}
	if monitor.Spec.ReportConfig {
		observation.ConfigYAML = cm.Data[configKey]
	}
	for _, pod := range pods {
		p := opamp.PodHealth{Name: pod.Name, Node: pod.Spec.NodeName, Phase: string(pod.Status.Phase)}
		for _, status := range pod.Status.ContainerStatuses {
			if status.Name == container {
				p.Ready = status.Ready && pod.DeletionTimestamp == nil
				p.Restarts = status.RestartCount
				p.Image = status.Image
				if status.State.Waiting != nil {
					p.Reason = status.State.Waiting.Reason
				}
				if status.State.Terminated != nil {
					p.Reason = status.State.Terminated.Reason
				}
			}
		}
		if p.Ready {
			observation.ReadyPods++
		}
		observation.Pods = append(observation.Pods, p)
	}
	monitor.Status.AgentCount = int32(len(pods))
	monitor.Status.HealthyAgents = int32(observation.ReadyPods)
	if err := c.Report(observation); err != nil {
		return fail("HealthReported", err)
	}
	r.condition(monitor, "HealthReported", metav1.ConditionTrue, "ReadinessReported", "Collector container readiness reported; telemetry health remains unknown")
	connected := c.Connected()
	monitor.Status.Phase = "Disconnected"
	r.condition(monitor, "OpAMPConnected", metav1.ConditionFalse, "Connecting", "Waiting for authenticated OpAMP connection")
	if connected {
		monitor.Status.Phase = "Active"
		now := metav1.Now()
		monitor.Status.LastHeartbeat = &now
		r.condition(monitor, "OpAMPConnected", metav1.ConditionTrue, "Connected", "OpAMP transport connected; delivery acknowledgement is not tracked")
	}
	r.condition(monitor, "Active", metav1.ConditionFalse, "Disconnected", "Observation transport is disconnected")
	if connected {
		r.condition(monitor, "Active", metav1.ConditionTrue, "Observing", "Observing existing collector workload without deployment control")
	}
	interval := monitor.Spec.HealthCheck.Interval.Duration
	if interval <= 0 {
		interval = 30 * time.Second
	}
	if interval < 5*time.Second {
		interval = 5 * time.Second
	}
	return ctrl.Result{RequeueAfter: interval}, r.Status().Update(ctx, monitor)
}

func (r *CollectorMonitorReconciler) discoverWorkload(ctx context.Context, monitor *v1.CollectorMonitor) (client.Object, string, error) {
	s := monitor.Spec.WorkloadSelector
	if len(s.MatchLabels) == 0 && s.Name == "" {
		return nil, "", fmt.Errorf("provide workload name or non-empty matchLabels")
	}
	var objects []client.Object
	var kinds []string
	add := func(obj client.Object, kind string) {
		if s.Name == "" || obj.GetName() == s.Name {
			objects = append(objects, obj)
			kinds = append(kinds, kind)
		}
	}
	opts := []client.ListOption{client.InNamespace(monitor.Namespace), client.MatchingLabels(s.MatchLabels)}
	if s.Kind == "" || s.Kind == "DaemonSet" {
		var list appsv1.DaemonSetList
		if err := r.List(ctx, &list, opts...); err != nil {
			return nil, "", err
		}
		for i := range list.Items {
			add(&list.Items[i], "DaemonSet")
		}
	}
	if s.Kind == "" || s.Kind == "Deployment" {
		var list appsv1.DeploymentList
		if err := r.List(ctx, &list, opts...); err != nil {
			return nil, "", err
		}
		for i := range list.Items {
			add(&list.Items[i], "Deployment")
		}
	}
	if s.Kind == "" || s.Kind == "StatefulSet" {
		var list appsv1.StatefulSetList
		if err := r.List(ctx, &list, opts...); err != nil {
			return nil, "", err
		}
		for i := range list.Items {
			add(&list.Items[i], "StatefulSet")
		}
	}
	if len(objects) != 1 {
		return nil, "", fmt.Errorf("selector matched %d workloads; use one CollectorMonitor per workload and specify kind/name to resolve ambiguity", len(objects))
	}
	return objects[0], kinds[0], nil
}

func template(w client.Object) *corev1.PodTemplateSpec {
	switch w := w.(type) {
	case *appsv1.DaemonSet:
		return &w.Spec.Template
	case *appsv1.Deployment:
		return &w.Spec.Template
	case *appsv1.StatefulSet:
		return &w.Spec.Template
	}
	return nil
}

func collectorContainer(m *v1.CollectorMonitor, w client.Object) (string, error) {
	t := template(w)
	if t == nil {
		return "", fmt.Errorf("unsupported workload")
	}
	if m.Spec.CollectorContainer != "" {
		for _, c := range t.Spec.Containers {
			if c.Name == m.Spec.CollectorContainer {
				return c.Name, nil
			}
		}
		return "", fmt.Errorf("collectorContainer %q does not exist", m.Spec.CollectorContainer)
	}
	if len(t.Spec.Containers) == 1 {
		return t.Spec.Containers[0].Name, nil
	}
	return "", fmt.Errorf("multi-container workload requires collectorContainer to identify the collector explicitly")
}

func (r *CollectorMonitorReconciler) discoverConfig(ctx context.Context, m *v1.CollectorMonitor, w client.Object, container string) (*corev1.ConfigMap, string, error) {
	t := template(w)
	mounted := map[string]bool{}
	mounts := map[string][]corev1.VolumeMount{}
	for _, c := range t.Spec.Containers {
		if c.Name == container {
			for _, mount := range c.VolumeMounts {
				mounted[mount.Name] = true
				mounts[mount.Name] = append(mounts[mount.Name], mount)
			}
		}
	}
	names := map[string]bool{}
	for _, v := range t.Spec.Volumes {
		if !mounted[v.Name] {
			continue
		}
		if v.ConfigMap != nil {
			names[v.ConfigMap.Name] = true
		}
		if v.Projected != nil {
			for _, src := range v.Projected.Sources {
				if src.ConfigMap != nil {
					names[src.ConfigMap.Name] = true
				}
			}
		}
	}
	var candidates []*corev1.ConfigMap
	for name := range names {
		if m.Spec.ConfigMapSelector != nil && m.Spec.ConfigMapSelector.Name != "" && m.Spec.ConfigMapSelector.Name != name {
			continue
		}
		cm := &corev1.ConfigMap{}
		if err := r.Get(ctx, client.ObjectKey{Namespace: m.Namespace, Name: name}, cm); err != nil {
			return nil, "", err
		}
		match := true
		if m.Spec.ConfigMapSelector != nil {
			for key, value := range m.Spec.ConfigMapSelector.MatchLabels {
				if cm.Labels[key] != value {
					match = false
				}
			}
		}
		if match {
			candidates = append(candidates, cm)
		}
	}
	if len(candidates) != 1 {
		return nil, "", fmt.Errorf("collector container mounts %d matching ConfigMaps; specify configMapSelector.name to resolve ambiguity", len(candidates))
	}
	cm := candidates[0]
	key := ""
	if m.Spec.ConfigMapSelector != nil {
		key = m.Spec.ConfigMapSelector.Key
	}
	if key == "" {
		var keys []string
		for k := range cm.Data {
			if strings.HasSuffix(k, ".yaml") || strings.HasSuffix(k, ".yml") {
				keys = append(keys, k)
			}
		}
		if len(keys) != 1 {
			return nil, "", fmt.Errorf("ConfigMap %s has %d YAML keys; specify configMapSelector.key", cm.Name, len(keys))
		}
		key = keys[0]
	}
	if body, ok := cm.Data[key]; !ok || strings.TrimSpace(body) == "" {
		return nil, "", fmt.Errorf("ConfigMap %s has no non-empty key %q", cm.Name, key)
	}
	// A key filtered out by a volume's items is not mounted.
	keyMounted := false
	containsKey := func(volume string, items []corev1.KeyToPath) bool {
		keyPath := key
		if len(items) > 0 {
			keyPath = ""
			for _, item := range items {
				if item.Key == key {
					keyPath = item.Path
					break
				}
			}
			if keyPath == "" {
				return false
			}
		}
		for _, mount := range mounts[volume] {
			// Expressions need pod environment resolution; do not guess.
			if mount.SubPathExpr != "" {
				continue
			}
			sub := strings.TrimSuffix(mount.SubPath, "/")
			if sub == "" || keyPath == sub || strings.HasPrefix(keyPath, sub+"/") {
				return true
			}
		}
		return false
	}
	for _, v := range t.Spec.Volumes {
		if !mounted[v.Name] {
			continue
		}
		if v.ConfigMap != nil && v.ConfigMap.Name == cm.Name && containsKey(v.Name, v.ConfigMap.Items) {
			keyMounted = true
		}
		if v.Projected != nil {
			for _, src := range v.Projected.Sources {
				if src.ConfigMap != nil && src.ConfigMap.Name == cm.Name && containsKey(v.Name, src.ConfigMap.Items) {
					keyMounted = true
				}
			}
		}
	}
	if !keyMounted {
		return nil, "", fmt.Errorf("ConfigMap key %s/%s is excluded from the collector's mounted volume", cm.Name, key)
	}
	return cm, key, nil
}

func desiredPods(w client.Object) int {
	switch w := w.(type) {
	case *appsv1.DaemonSet:
		return int(w.Status.DesiredNumberScheduled)
	case *appsv1.Deployment:
		if w.Spec.Replicas != nil {
			return int(*w.Spec.Replicas)
		}
	case *appsv1.StatefulSet:
		if w.Spec.Replicas != nil {
			return int(*w.Spec.Replicas)
		}
	}
	return 1
}

// Owner UIDs, including Deployment -> ReplicaSet, keep overlapping labels from
// attributing another workload's pods to this monitor.
func (r *CollectorMonitorReconciler) workloadPods(ctx context.Context, w client.Object, kind string) ([]corev1.Pod, error) {
	owners := map[types.UID]bool{w.GetUID(): true}
	if kind == "Deployment" {
		var replicas appsv1.ReplicaSetList
		if err := r.List(ctx, &replicas, client.InNamespace(w.GetNamespace())); err != nil {
			return nil, err
		}
		for _, rs := range replicas.Items {
			if owner := metav1.GetControllerOf(&rs); owner != nil && owner.UID == w.GetUID() {
				owners[rs.UID] = true
			}
		}
	}
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(w.GetNamespace())); err != nil {
		return nil, err
	}
	result := make([]corev1.Pod, 0)
	for _, p := range pods.Items {
		if owner := metav1.GetControllerOf(&p); owner != nil && owners[owner.UID] {
			result = append(result, p)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result, nil
}

func (r *CollectorMonitorReconciler) ensureConnection(ctx context.Context, m *v1.CollectorMonitor, w client.Object, kind, container string, cm *corev1.ConfigMap) (*opamp.Client, error) {
	endpoint := m.Spec.OpAMPServer
	if endpoint == "" {
		endpoint = r.DefaultServer
	}
	if !strings.HasPrefix(endpoint, "wss://") {
		return nil, fmt.Errorf("opampServer must use wss:// with a trusted server certificate")
	}
	if r.ClusterID == "" {
		return nil, fmt.Errorf("cluster identity is unavailable; set CLUSTER_ID")
	}
	token := r.DefaultSecretKey
	secretVersion := ""
	if ref := m.Spec.Auth.SecretRef; ref != nil {
		namespace := ref.Namespace
		if namespace == "" {
			namespace = m.Namespace
		}
		secret := &corev1.Secret{}
		reader := r.Reader
		if reader == nil {
			reader = r.Client
		}
		if err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: ref.Name}, secret); err != nil {
			return nil, err
		}
		key := ref.Key
		if key == "" {
			key = "secret-key"
		}
		token = string(secret.Data[key])
		secretVersion = secret.ResourceVersion
	}
	if token == "" {
		return nil, fmt.Errorf("OpAMP authentication secret is missing or empty")
	}
	clusterName := m.Labels["k8s.cluster.name"]
	if clusterName == "" {
		clusterName = r.ClusterID
	}
	identity := fmt.Sprintf("k8s://%s/%s/%s", r.ClusterID, w.GetUID(), container)
	spec, _ := json.Marshal(m.Spec)
	// ConfigMap content changes are sent as observations without reconnecting.
	fingerprint := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%s|%s|%s|%s", spec, identity, clusterName, cm.Name, secretVersion, endpoint)))
	key := m.Namespace + "/" + m.Name
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.connections == nil {
		r.connections = map[string]*connection{}
	}
	if current := r.connections[key]; current != nil {
		if current.fingerprint == fingerprint {
			return current.client, nil
		}
		current.client.Stop()
		delete(r.connections, key)
	}
	c := opamp.NewClient(opamp.ClientConfig{
		Endpoint: endpoint, TLSConfig: r.TLSConfig, AgentID: identity, AgentType: api.AgentTypeKubernetes,
		Headers: map[string]string{"Authorization": "Secret-Key " + token},
		Labels: map[string]string{
			"k8s.cluster.name": clusterName, "k8s.cluster.id": r.ClusterID,
			"k8s.namespace": m.Namespace, "k8s.workload.type": kind, "k8s.workload.name": w.GetName(),
			"k8s.workload.uid": string(w.GetUID()), "k8s.container.name": container,
			"k8s.configmap.name": cm.Name, "collectorctrl.management.mode": "observe",
			"collectorctrl.health.source": "container-readiness",
		},
	})
	lifetime := r.lifetime
	if lifetime == nil {
		lifetime = ctx
	}
	if err := c.Start(lifetime); err != nil {
		return nil, err
	}
	r.connections[key] = &connection{client: c, fingerprint: fingerprint}
	return c, nil
}

func (r *CollectorMonitorReconciler) condition(m *v1.CollectorMonitor, kind string, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&m.Status.Conditions, metav1.Condition{
		Type: kind, Status: status, Reason: reason, Message: message, ObservedGeneration: m.Generation,
	})
}

func (r *CollectorMonitorReconciler) closeConnection(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c := r.connections[key]; c != nil {
		c.client.Stop()
		delete(r.connections, key)
	}
}

func (r *CollectorMonitorReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.lifetime = context.Background()
	if err := mgr.Add(&connectionLifetime{reconciler: r}); err != nil {
		return err
	}
	mapper := handler.EnqueueRequestsFromMapFunc(r.mapNamespace)
	return ctrl.NewControllerManagedBy(mgr).For(&v1.CollectorMonitor{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&corev1.ConfigMap{}, mapper).Watches(&corev1.Pod{}, mapper).
		Watches(&appsv1.Deployment{}, mapper).Watches(&appsv1.DaemonSet{}, mapper).
		Watches(&appsv1.StatefulSet{}, mapper).Watches(&appsv1.ReplicaSet{}, mapper).Complete(r)
}

type connectionLifetime struct{ reconciler *CollectorMonitorReconciler }

func (l *connectionLifetime) NeedLeaderElection() bool { return true }
func (l *connectionLifetime) Start(ctx context.Context) error {
	<-ctx.Done()
	l.reconciler.mu.Lock()
	defer l.reconciler.mu.Unlock()
	for _, c := range l.reconciler.connections {
		c.client.Stop()
	}
	return nil
}
func (r *CollectorMonitorReconciler) mapNamespace(ctx context.Context, obj client.Object) []reconcile.Request {
	var list v1.CollectorMonitorList
	if err := r.List(ctx, &list, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	result := make([]reconcile.Request, 0, len(list.Items))
	for _, m := range list.Items {
		result = append(result, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: m.Namespace, Name: m.Name}})
	}
	return result
}
