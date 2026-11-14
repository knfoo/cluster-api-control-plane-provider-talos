// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// talos-update-extension is the Cluster API Runtime SDK extension that updates Talos
// machines in place. It serves the Discovery, CanUpdateMachine and UpdateMachine hooks;
// the update logic itself lives in pkg/inplaceupdate.
//
// The extension ships dark: the ExtensionConfig in config/extension/extensionconfig.yaml
// is only registered when an operator applies it, and Cluster API calls the hooks only
// when its EXP_IN_PLACE_UPDATES feature gate is enabled (ADR-0003).
package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-logr/logr"
	talosconfig "github.com/siderolabs/talos/pkg/machinery/client/config"
	"github.com/spf13/pflag"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/component-base/logs"
	logsv1 "k8s.io/component-base/logs/api/v1"
	"k8s.io/klog/v2"
	runtimehooksv1 "sigs.k8s.io/cluster-api/api/runtime/hooks/v1alpha1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/siderolabs/cluster-api-control-plane-provider-talos/pkg/inplaceupdate"
)

// The extension binary reads the management cluster only when it is started with
// --kubeconfig: rendering a generated machine configuration needs the Cluster object and
// the cluster's Talos secrets bundle, and rendering a metadataFrom template needs the
// metadata secret the infrastructure provider (Metal3/CAPM3) created. The rules it needs
// are the ones the control plane controller already declares in config/rbac/role.yaml.

// The hook paths are the ones the Runtime SDK catalog derives for the hooks. The
// handler name registered in the ExtensionConfig is a suffix of the path, so these
// prefixes serve every registered handler name.
const (
	discoveryPath        = "/hooks.runtime.cluster.x-k8s.io/v1alpha1/discovery/"
	canUpdateMachinePath = "/hooks.runtime.cluster.x-k8s.io/v1alpha1/canupdatemachine/"
	updateMachinePath    = "/hooks.runtime.cluster.x-k8s.io/v1alpha1/updatemachine/"
)

var (
	listenAddress   string
	certFile        string
	keyFile         string
	talosconfigPath string
	kubeconfigPath  string
	defaultEndpoint string
	requestTimeout  time.Duration
	logOptions      = logs.NewOptions()
)

func main() {
	fs := pflag.NewFlagSet("talos-update-extension", pflag.ExitOnError)
	fs.StringVar(&listenAddress, "listen-address", ":8443", "address the extension listens on")
	fs.StringVar(&certFile, "cert-file", "", "PEM certificate the extension serves; required because Cluster API calls extensions over https")
	fs.StringVar(&keyFile, "key-file", "", "PEM private key of --cert-file")
	fs.StringVar(&talosconfigPath, "talosconfig", os.Getenv("TALOSCONFIG"), "path to the talosconfig used to talk to the machines")
	fs.StringVar(&kubeconfigPath, "kubeconfig", os.Getenv("KUBECONFIG"), "path to a kubeconfig of the management cluster; without it the extension renders only the TalosConfigs that carry their machine configuration in spec.data")
	fs.StringVar(&defaultEndpoint, "endpoint", "", "fallback Talos API address of a machine, used when the request carries none")
	fs.DurationVar(&requestTimeout, "timeout", inplaceupdate.DefaultRequestTimeout, "timeout of a single call to the Talos API")
	logsv1.AddFlags(logOptions, fs)
	fs.Parse(os.Args[1:])

	if err := logsv1.ValidateAndApply(logOptions, nil); err != nil {
		os.Exit(1)
	}

	ctrl.SetLogger(klog.Background())
	log := ctrl.Log.WithName("talos-update-extension")

	// Cluster API calls extension servers only over https (api/runtime/v1alpha1 ClientConfig:
	// "The scheme must be https"), so the extension requires a certificate and its key.
	if certFile == "" || keyFile == "" {
		log.Error(errors.New("the extension serves hooks over https: set --cert-file and --key-file"), "missing the extension certificate")
		os.Exit(1)
	}

	config, err := loadTalosConfig(talosconfigPath)
	if err != nil {
		log.Error(err, "loading the talosconfig")
		os.Exit(1)
	}

	metadata, err := metadataReader(kubeconfigPath)
	if err != nil {
		log.Error(err, "connecting to the management cluster")
		os.Exit(1)
	}

	handler := inplaceupdate.NewHandlerWithMetadata(inplaceupdate.NewTalosApplier(config), log, defaultEndpoint, requestTimeout, metadata)

	server, err := hookServer(listenAddress, certFile, keyFile, handler, log)
	if err != nil {
		log.Error(err, "building the extension server")
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	log.Info("starting the Talos in-place update extension", "address", listenAddress)

	go func() {
		// The certificate and key are already loaded into server.TLSConfig.
		if err := server.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error(err, "the extension server failed")
			os.Exit(1)
		}
	}()

	<-ctx.Done()

	log.Info("shutting down the Talos in-place update extension")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Error(err, "shutting down the extension server")
	}
}

// hookServer builds the server that serves the hooks. Cluster API only talks to extension
// servers over https, so the certificate given with --cert-file and --key-file is loaded into
// the server's TLS configuration and ListenAndServeTLS is called without file names.
func hookServer(address, certFile, keyFile string, handler *inplaceupdate.Handler, log logr.Logger) (*http.Server, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("reading the extension certificate %q and key %q: %w", certFile, keyFile, err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc(discoveryPath, discovery(handler, log))
	mux.HandleFunc(canUpdateMachinePath, canUpdateMachine(handler, log))
	mux.HandleFunc(updateMachinePath, updateMachine(handler, log))

	return &http.Server{
		Addr:              address,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		TLSConfig:         &tls.Config{Certificates: []tls.Certificate{cert}},
	}, nil
}

func loadTalosConfig(path string) (*talosconfig.Config, error) {
	if path == "" {
		return nil, errors.New("no talosconfig given: set --talosconfig or the TALOSCONFIG environment variable")
	}

	config, err := talosconfig.Open(path)
	if err != nil {
		return nil, fmt.Errorf("reading the talosconfig at %q: %w", path, err)
	}

	return config, nil
}

func discovery(handler *inplaceupdate.Handler, log logr.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		request := &runtimehooksv1.DiscoveryRequest{}
		response := &runtimehooksv1.DiscoveryResponse{}

		if err := decode(r, request); err != nil {
			fail(w, err)
			return
		}

		handler.Discovery(request, response)
		write(w, log, response)
	}
}

func canUpdateMachine(handler *inplaceupdate.Handler, log logr.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		request := &runtimehooksv1.CanUpdateMachineRequest{}
		response := &runtimehooksv1.CanUpdateMachineResponse{}

		if err := decode(r, request); err != nil {
			fail(w, err)
			return
		}

		handler.CanUpdateMachine(request, response)
		write(w, log, response)
	}
}

func updateMachine(handler *inplaceupdate.Handler, log logr.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		request := &runtimehooksv1.UpdateMachineRequest{}
		response := &runtimehooksv1.UpdateMachineResponse{}

		if err := decode(r, request); err != nil {
			fail(w, err)
			return
		}

		handler.UpdateMachine(request, response)
		write(w, log, response)
	}
}

func decode(r *http.Request, object any) error {
	if r.Body == nil {
		return errors.New("the hook request has no body")
	}

	if err := json.NewDecoder(r.Body).Decode(object); err != nil {
		return fmt.Errorf("decoding the hook request: %w", err)
	}

	return nil
}

func write(w http.ResponseWriter, log logr.Logger, object any) {
	w.Header().Set("Content-Type", "application/json")

	if err := json.NewEncoder(w).Encode(object); err != nil {
		log.Error(err, "encoding the hook response")
	}
}

func fail(w http.ResponseWriter, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)

	response := runtimehooksv1.CommonResponse{
		Status:  runtimehooksv1.ResponseStatusFailure,
		Message: err.Error(),
	}

	_ = json.NewEncoder(w).Encode(response)
}

// metadataReader reads the management cluster objects the extension needs to render a
// machine configuration. Without --kubeconfig it returns nil, and the extension answers
// "no patch" for every TalosConfig it cannot render, so Cluster API rolls the machine
// out instead of applying a configuration it guessed.
func metadataReader(kubeconfigPath string) (inplaceupdate.MetadataReader, error) {
	if kubeconfigPath == "" {
		return nil, nil
	}

	kubeConfig, err := clientcmd.LoadFromFile(kubeconfigPath)
	if err != nil {
		return nil, fmt.Errorf("reading the kubeconfig at %q: %w", kubeconfigPath, err)
	}

	restConfig, err := clientcmd.NewDefaultClientConfig(*kubeConfig, nil).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("building the kubeconfig at %q: %w", kubeconfigPath, err)
	}

	kubeClient, err := client.New(restConfig, client.Options{})
	if err != nil {
		return nil, fmt.Errorf("connecting to the management cluster: %w", err)
	}

	return inplaceupdate.NewKubeMetadataReader(kubeClient), nil
}
