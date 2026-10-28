// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 CollectorCtrl

package controllers

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	v1 "github.com/collectorctrl/collectorctrl/operator/api/v1alpha1"
	"github.com/collectorctrl/collectorctrl/pkg/api"
	"github.com/collectorctrl/collectorctrl/pkg/opamp"
)

// maxCandidates bounds one inventory message.
const maxCandidates = 2000

// collectorImageHints are image repository fragments of OpenTelemetry Collector
// distributions. Matching is on the repository path only, never the tag.
var collectorImageHints = []string{
	"opentelemetry-collector", // otel/opentelemetry-collector{,-contrib,-k8s}
	"otelcol",                 // otelcol-contrib, custom OCB builds
	"otel-collector",          // splunk-otel-collector, coralogix, vendor builds
	"aws-otel-collector",      // AWS ADOT
	"elastic-otel-collector",  // Elastic EDOT
	"dynatrace-otel-collector",
	"nrdot-collector", // New Relic
	"grafana/alloy",   // Grafana Alloy (config is not otelcol YAML)
}

// looksLikeCollector reports whether an image reference names a known
// collector distribution.
func looksLikeCollector(image string) bool {
	repo := strings.ToLower(image)
	if i := strings.Index(repo, "@"); i >= 0 {
		repo = repo[:i]
	}
	if i := strings.LastIndex(repo, ":"); i > strings.LastIndex(repo, "/") {
		repo = repo[:i]
	}
	for _, hint := range collectorImageHints {
		if strings.Contains(repo, hint) {
			return true
		}
	}
	return false
}

// ClusterObserver keeps one extra OpAMP connection per cluster for
// cluster-scoped data. Its first payload lists collector-like workloads, so
// the UI can suggest which ones to opt in. It reads workload metadata only.
type ClusterObserver struct {
	Monitors    *CollectorMonitorReconciler
	ClusterName string
	Interval    time.Duration
	// APIReader reads cluster-scoped objects (namespaces, webhooks) uncached.
	APIReader client.Reader
	// ReadinessEnabled turns on the instrumentation readiness report, sent every
	// ReadinessInterval (default 5 minutes) on the same connection.
	ReadinessEnabled  bool
	ReadinessInterval time.Duration
	ReadinessExclude  []string
	client            *opamp.Client
	lastReadiness     time.Time
}

// NeedLeaderElection keeps a single cluster connection per cluster.
func (o *ClusterObserver) NeedLeaderElection() bool { return true }

func (o *ClusterObserver) Start(ctx context.Context) error {
	log := logf.FromContext(ctx).WithName("cluster-observer")
	interval := o.Interval
	if interval <= 0 {
		interval = time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	defer func() {
		if o.client != nil {
			o.client.Stop()
		}
	}()
	for {
		if err := o.report(ctx, interval); err != nil {
			log.Error(err, "cluster inventory not reported")
		}
		if o.ReadinessEnabled && time.Since(o.lastReadiness) >= o.readinessInterval() {
			if err := o.reportReadiness(ctx); err != nil {
				log.Error(err, "instrumentation readiness not reported")
			} else {
				o.lastReadiness = time.Now()
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (o *ClusterObserver) clusterName() string {
	if o.ClusterName != "" {
		return o.ClusterName
	}
	return o.Monitors.ClusterID
}

func (o *ClusterObserver) ensureClient(ctx context.Context) (*opamp.Client, error) {
	if o.client != nil {
		return o.client, nil
	}
	r := o.Monitors
	endpoint, err := opamp.ApprovedEndpoint(r.DefaultServer, "")
	if err != nil {
		return nil, err
	}
	if r.ClusterID == "" {
		return nil, fmt.Errorf("cluster identity is unavailable; set CLUSTER_ID")
	}
	cfg := opamp.ClientConfig{
		Endpoint: endpoint, TLSConfig: r.TLSConfig, CredentialDirectory: r.CredentialDirectory,
		AgentID: fmt.Sprintf("k8s://%s/cluster-observer", r.ClusterID), AgentType: api.AgentTypeKubernetes,
		Labels: map[string]string{
			"k8s.cluster.name": o.clusterName(), "k8s.cluster.id": r.ClusterID,
			"collectorctrl.management.mode": "observe", "collectorctrl.k8s.role": "cluster-observer",
		},
	}
	if r.AuthMode == "legacy" {
		cfg.LegacySecret = r.DefaultSecretKey
	} else {
		cfg.EnrollmentToken = r.EnrollmentToken
	}
	c := opamp.NewClient(cfg)
	if err := c.Start(ctx); err != nil {
		return nil, err
	}
	o.client = c
	return c, nil
}

func (o *ClusterObserver) report(ctx context.Context, interval time.Duration) error {
	inv, err := o.Inventory(ctx)
	if err != nil {
		return err
	}
	inv.ReportIntervalSeconds = int64(interval / time.Second)
	c, err := o.ensureClient(ctx)
	if err != nil {
		return err
	}
	return c.ReportInventory(inv)
}

func (o *ClusterObserver) readinessInterval() time.Duration {
	switch d := o.ReadinessInterval; {
	case d <= 0:
		return 5 * time.Minute
	case d < time.Minute:
		return time.Minute
	case d > time.Hour:
		return time.Hour
	default:
		return d
	}
}

func (o *ClusterObserver) reportReadiness(ctx context.Context) error {
	rep, err := o.Readiness(ctx)
	if err != nil {
		return err
	}
	rep.ReportIntervalSeconds = int64(o.readinessInterval() / time.Second)
	c, err := o.ensureClient(ctx)
	if err != nil {
		return err
	}
	return c.ReportReadiness(rep)
}

// Inventory lists collector-like workloads from cached metadata and marks the
// ones already observed by label or CollectorMonitor.
func (o *ClusterObserver) Inventory(ctx context.Context) (opamp.ClusterInventory, error) {
	r := o.Monitors
	inv := opamp.ClusterInventory{Source: "kubernetes-api", ObservedAt: time.Now().UTC(), ClusterID: r.ClusterID, ClusterName: o.clusterName(), Collectors: []opamp.CollectorCandidate{}}
	monitored := map[types.UID]bool{}
	var monitors v1.CollectorMonitorList
	if err := r.List(ctx, &monitors); err != nil {
		return inv, err
	}
	for i := range monitors.Items {
		if w, _, err := r.discoverWorkload(ctx, &monitors.Items[i]); err == nil {
			monitored[w.GetUID()] = true
		}
	}
	add := func(obj client.Object, kind string, tmpl *corev1.PodTemplateSpec) {
		for _, c := range tmpl.Spec.Containers {
			if !looksLikeCollector(c.Image) {
				continue
			}
			stack, source := stackOf(&v1.CollectorMonitor{}, obj)
			inv.Collectors = append(inv.Collectors, opamp.CollectorCandidate{
				Namespace: obj.GetNamespace(), Kind: kind, Name: obj.GetName(), UID: string(obj.GetUID()),
				Container: c.Name, Image: c.Image, Stack: stack, StackSource: source,
				OptedIn: optedIn(obj), Monitored: monitored[obj.GetUID()],
			})
			return
		}
	}
	var ds appsv1.DaemonSetList
	if err := r.List(ctx, &ds); err != nil {
		return inv, err
	}
	for i := range ds.Items {
		add(&ds.Items[i], "DaemonSet", &ds.Items[i].Spec.Template)
	}
	var deps appsv1.DeploymentList
	if err := r.List(ctx, &deps); err != nil {
		return inv, err
	}
	for i := range deps.Items {
		add(&deps.Items[i], "Deployment", &deps.Items[i].Spec.Template)
	}
	var sts appsv1.StatefulSetList
	if err := r.List(ctx, &sts); err != nil {
		return inv, err
	}
	for i := range sts.Items {
		add(&sts.Items[i], "StatefulSet", &sts.Items[i].Spec.Template)
	}
	sort.Slice(inv.Collectors, func(i, j int) bool {
		a, b := inv.Collectors[i], inv.Collectors[j]
		return a.Namespace+"/"+a.Kind+"/"+a.Name < b.Namespace+"/"+b.Kind+"/"+b.Name
	})
	if len(inv.Collectors) > maxCandidates {
		inv.Collectors, inv.Truncated = inv.Collectors[:maxCandidates], true
	}
	return inv, nil
}
