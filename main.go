// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package main

import (
	"context"
	"crypto/tls"
	"flag"
	"os"

	"github.com/pkg/errors"
	bootstrapv1alpha3 "github.com/siderolabs/cluster-api-bootstrap-provider-talos/api/v1alpha3"
	controlplanev1alpha3 "github.com/siderolabs/cluster-api-control-plane-provider-talos/api/v1alpha3"
	controlplanev1beta1 "github.com/siderolabs/cluster-api-control-plane-provider-talos/api/v1beta1"
	"github.com/siderolabs/cluster-api-control-plane-provider-talos/controllers"
	"github.com/siderolabs/cluster-api-control-plane-provider-talos/pkg/inplaceupdate"
	"github.com/spf13/pflag"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/selection"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	_ "k8s.io/client-go/plugin/pkg/client/auth/gcp"
	"k8s.io/component-base/logs"
	logsv1 "k8s.io/component-base/logs/api/v1"
	"k8s.io/klog/v2"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	runtimecatalog "sigs.k8s.io/cluster-api/api/runtime/catalog"
	runtimehooksv1 "sigs.k8s.io/cluster-api/api/runtime/hooks/v1alpha1"
	runtimev1beta2 "sigs.k8s.io/cluster-api/api/runtime/v1beta2"
	"sigs.k8s.io/cluster-api/controllers/clustercache" // for remote cluster client builder
	"sigs.k8s.io/cluster-api/controllers/remote"
	runtimeserver "sigs.k8s.io/cluster-api/exp/runtime/server"
	"sigs.k8s.io/cluster-api/feature"
	"sigs.k8s.io/cluster-api/util/flags"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

var (
	enableLeaderElection bool
	webhookPort          int
	runtimeExtensionPort int
	webhookCertDir       string
	healthAddr           string
	managerOptions       = flags.ManagerOptions{}
	logOptions           = logs.NewOptions()
)

func init() {
	_ = clientgoscheme.AddToScheme(scheme)
	_ = clusterv1.AddToScheme(scheme)
	_ = bootstrapv1alpha3.AddToScheme(scheme)
	_ = controlplanev1alpha3.AddToScheme(scheme)
	_ = controlplanev1beta1.AddToScheme(scheme)
	_ = runtimev1beta2.AddToScheme(scheme)
	// +kubebuilder:scaffold:scheme
}

// InitFlags initializes the flags.
func InitFlags(fs *pflag.FlagSet) {
	logsv1.AddFlags(logOptions, fs)

	fs.BoolVar(&enableLeaderElection, "enable-leader-election", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	fs.IntVar(&webhookPort, "webhook-port", 9443, "Webhook Server port, disabled by default. When enabled, the manager will only work as webhook server, no reconcilers are installed.")
	fs.StringVar(&webhookCertDir, "webhook-cert-dir", "/tmp/k8s-webhook-server/serving-certs/",
		"Webhook cert dir, only used when webhook-port is specified.")
	fs.IntVar(&runtimeExtensionPort, "runtime-extension-port", 9444,
		"Port the in-place update hooks (CanUpdateMachine, CanUpdateMachineSet, UpdateMachine) are served on. "+
			"They are served only while the InPlaceUpdates feature gate is enabled, and they are served with "+
			"the certificate in --webhook-cert-dir.")
	fs.StringVar(&healthAddr, "health-addr", ":9440",
		"The address the health endpoint binds to.")
	// The manager reads the Cluster API feature gates (InPlaceUpdates among them) from --feature-gates,
	// as KubeadmControlPlane does.
	feature.MutableGates.AddFlag(fs)
	flags.AddManagerOptions(fs, &managerOptions)
}

func main() {
	InitFlags(pflag.CommandLine)
	pflag.CommandLine.AddGoFlagSet(flag.CommandLine)
	pflag.Parse()

	if err := logsv1.ValidateAndApply(logOptions, nil); err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	// klog.Background will automatically use the right logger.
	ctrl.SetLogger(klog.Background())

	tlsOptionOverrides, diagnosticsOpts, err := flags.GetManagerOptions(managerOptions)
	if err != nil {
		setupLog.Error(err, "unable to add TLS settings to the webhook server")
		os.Exit(1)
	}

	req, _ := labels.NewRequirement(clusterv1.ClusterNameLabel, selection.Exists, nil)
	clusterSecretCacheSelector := labels.NewSelector().Add(*req)

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "controller-leader-election-cacppt",
		Metrics:                *diagnosticsOpts,
		HealthProbeBindAddress: healthAddr,
		Cache: cache.Options{
			ByObject: map[client.Object]cache.ByObject{
				// Note: Only Secrets with the cluster name label are cached.
				// The default client of the manager won't use the cache for secrets at all (see Client.Cache.DisableFor).
				// The cached secrets will only be used by the secretCachingClient we create below.
				&corev1.Secret{}: {
					Label: clusterSecretCacheSelector,
				},
			},
		},
		Client: client.Options{
			Cache: &client.CacheOptions{
				DisableFor: []client.Object{
					&corev1.ConfigMap{},
					&corev1.Secret{},
				},
			},
		},
		WebhookServer: webhook.NewServer(
			webhook.Options{
				Port:    webhookPort,
				CertDir: webhookCertDir,
				TLSOpts: tlsOptionOverrides,
			},
		),
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	secretCachingClient, err := client.New(mgr.GetConfig(), client.Options{
		HTTPClient: mgr.GetHTTPClient(),
		Cache: &client.CacheOptions{
			Reader: mgr.GetCache(),
		},
	})
	if err != nil {
		setupLog.Error(err, "unable to create secret caching client")
		os.Exit(1)
	}

	clusterCache, err := clustercache.SetupWithManager(context.Background(), mgr, clustercache.Options{
		SecretClient: secretCachingClient,
		Client: clustercache.ClientOptions{
			UserAgent: remote.DefaultClusterAPIUserAgent("cluster-api-control-plane-provider-talos"),
			Cache: clustercache.ClientCacheOptions{
				DisableFor: []client.Object{
					&corev1.ConfigMap{},
					&corev1.Secret{},
					&corev1.Pod{},
					&appsv1.Deployment{},
					&appsv1.DaemonSet{},
				},
			},
		},
	}, controller.Options{
		MaxConcurrentReconciles: 10,
	})

	if err != nil {
		setupLog.Error(err, "unable to create cluster cache")
		os.Exit(1)
	}
	// The Runtime SDK client behind the in-place update decision. KubeadmControlPlane wires its runtime
	// client into the reconciler the same way; Cluster API's own client lives in internal/ packages a
	// provider cannot import, so controllers.NewRuntimeClient mirrors its extension lookup. It is only
	// consulted while the InPlaceUpdates feature gate is enabled, which stays off by default.
	runtimeClient := controllers.NewRuntimeClient(mgr.GetClient())
	if err = (&controllers.TalosControlPlaneReconciler{
		Client:        mgr.GetClient(),
		APIReader:     mgr.GetAPIReader(),
		Log:           ctrl.Log.WithName("controllers").WithName("TalosControlPlane"),
		Scheme:        mgr.GetScheme(),
		ClusterCache:  clusterCache,
		RuntimeClient: runtimeClient,
	}).SetupWithManager(mgr, controller.Options{MaxConcurrentReconciles: 10}); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "TalosControlPlane")
		os.Exit(1)
	}

	// Resolves the apiVersion of a ContractVersionedObjectReference (apiGroup + kind) when the conversion
	// down-converts to v1alpha3, through the contract labels of the CRD like the controller does.
	controlplanev1alpha3.SetAPIVersionGetter(controllers.ContractAPIVersionGetter(mgr.GetClient(), mgr.GetAPIReader(), ctrl.Log.WithName("conversion")))

	if err = (&controlplanev1beta1.TalosControlPlane{}).SetupWebhookWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create webhook", "webhook", "TalosControlPlane")
		os.Exit(1)
	}

	if err = (&controlplanev1beta1.TalosControlPlaneTemplate{}).SetupWebhookWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create webhook", "webhook", "TalosControlPlaneTemplate")
		os.Exit(1)
	}

	// The in-place update hooks of the Runtime SDK are served by the manager itself, as a second
	// webhook server next to the validating webhook server. Cluster API only calls them while its
	// InPlaceUpdates feature gate is enabled, which is off by default, so no runtime-extension port
	// is served when the gate is off.
	if feature.Gates.Enabled(feature.InPlaceUpdates) {
		if err := setupInPlaceUpdateHooks(mgr, tlsOptionOverrides); err != nil {
			setupLog.Error(err, "unable to serve the in-place update hooks")
			os.Exit(1)
		}
	}

	setupChecks(mgr)
	// +kubebuilder:scaffold:builder

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

func setupInPlaceUpdateHooks(mgr ctrl.Manager, tlsOpts []func(*tls.Config)) error {
	catalog := runtimecatalog.New()
	if err := runtimehooksv1.AddToCatalog(catalog); err != nil {
		return errors.Wrap(err, "building the runtime hook catalog")
	}

	runtimeExtensionServer, err := runtimeserver.New(runtimeserver.Options{
		Port:    runtimeExtensionPort,
		CertDir: webhookCertDir,
		TLSOpts: tlsOpts,
		Catalog: catalog,
	})
	if err != nil {
		return errors.Wrap(err, "creating the runtime extension server")
	}

	// The hooks read the Machine, its status addresses and the talosconfig secret of its cluster
	// with the manager's client, the same client the controllers use.
	handler := inplaceupdate.NewHandler(mgr.GetClient(), ctrl.Log.WithName("controllers").WithName("InPlaceUpdates"))

	for _, extensionHandler := range []runtimeserver.ExtensionHandler{
		{Hook: runtimehooksv1.CanUpdateMachine, Name: "talos-can-update-machine", HandlerFunc: handler.CanUpdateMachine},
		{Hook: runtimehooksv1.CanUpdateMachineSet, Name: "talos-can-update-machine-set", HandlerFunc: handler.CanUpdateMachineSet},
		{Hook: runtimehooksv1.UpdateMachine, Name: "talos-update-machine", HandlerFunc: handler.UpdateMachine},
	} {
		if err := runtimeExtensionServer.AddExtensionHandler(extensionHandler); err != nil {
			return errors.Wrapf(err, "adding the handler %q", extensionHandler.Name)
		}
	}

	// The server is a controller-runtime Runnable, so the manager starts and stops it with the
	// controllers. Discovery is answered by the server itself from the handlers registered above.
	if err := mgr.Add(runtimeExtensionServer); err != nil {
		return errors.Wrap(err, "adding the runtime extension server to the manager")
	}

	setupLog.Info("serving the in-place update hooks", "port", runtimeExtensionPort, "certDir", webhookCertDir)

	return nil
}

func setupChecks(mgr ctrl.Manager) {
	if err := mgr.AddReadyzCheck("webhook", mgr.GetWebhookServer().StartedChecker()); err != nil {
		setupLog.Error(err, "unable to create ready check")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("webhook", mgr.GetWebhookServer().StartedChecker()); err != nil {
		setupLog.Error(err, "unable to create health check")
		os.Exit(1)
	}
}
