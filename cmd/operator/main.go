package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	// Import all Kubernetes client auth plugins (GKE, EKS, AKS, etc.)
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"github.com/collectorctrl/collectorctrl/pkg/opamp"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/server"

	collectorctrlv1alpha1 "github.com/collectorctrl/collectorctrl/operator/api/v1alpha1"
	"github.com/collectorctrl/collectorctrl/operator/controllers"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(collectorctrlv1alpha1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

func main() {
	var metricsAddr string
	var enableLeaderElection bool
	var probeAddr string
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	tlsConfig, err := opamp.TLSConfig(os.Getenv("OPAMP_CA_FILE"))
	if err != nil {
		setupLog.Error(err, "invalid OpAMP TLS configuration")
		os.Exit(1)
	}
	namespace := os.Getenv("WATCH_NAMESPACE")
	cacheOptions := cache.Options{}
	if namespace != "" {
		cacheOptions.DefaultNamespaces = map[string]cache.Config{namespace: {}}
	}
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Cache:                  cacheOptions,
		Metrics:                server.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "collectorctrl.io",
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	clusterID := os.Getenv("CLUSTER_ID")
	if clusterID == "" {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		ns := &corev1.Namespace{}
		err = mgr.GetAPIReader().Get(ctx, client.ObjectKey{Name: "kube-system"}, ns)
		cancel()
		if err != nil || ns.UID == "" {
			setupLog.Error(fmt.Errorf("set CLUSTER_ID or grant get access to namespace kube-system: %v", err), "cluster identity unavailable")
			os.Exit(1)
		}
		clusterID = string(ns.UID)
	}

	if err = (&controllers.CollectorMonitorReconciler{
		Client:           mgr.GetClient(),
		Scheme:           mgr.GetScheme(),
		Reader:           mgr.GetAPIReader(),
		DefaultServer:    os.Getenv("OPAMP_SERVER"),
		TLSConfig:        tlsConfig,
		ClusterID:        clusterID,
		DefaultSecretKey: os.Getenv("OPAMP_SECRET_KEY"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "CollectorMonitor")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
