// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package controllers

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"maps"
	"net"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/pkg/errors"
	cabptv1 "github.com/siderolabs/cluster-api-bootstrap-provider-talos/api/v1alpha3"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	utilnet "k8s.io/apimachinery/pkg/util/net"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	runtimecatalog "sigs.k8s.io/cluster-api/api/runtime/catalog"
	runtimehooksv1 "sigs.k8s.io/cluster-api/api/runtime/hooks/v1alpha1"
	runtimev1beta2 "sigs.k8s.io/cluster-api/api/runtime/v1beta2"
	"sigs.k8s.io/cluster-api/util/conditions"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The mirrored extension lookup reads ExtensionConfig objects and the namespaces that scope them, the
// same reads the upstream Runtime SDK client performs (sigs.k8s.io/cluster-api/internal/runtime/client
// and sigs.k8s.io/cluster-api/internal/runtime/registry), so the manager role needs access to them.
// +kubebuilder:rbac:groups=runtime.cluster.x-k8s.io,resources=extensionconfigs,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=namespaces,verbs=get;list;watch

// RuntimeClient is the part of Cluster API's Runtime SDK client that the control-plane provider needs
// to decide whether a Machine can be updated in place. Cluster API keeps its own client and extension
// registry in internal/ packages, which a provider cannot import, so the extension lookup and the hook
// call are mirrored in this file: they follow GetAllExtensions in
// sigs.k8s.io/cluster-api/internal/runtime/client, GetAllExtensionConfigs in
// sigs.k8s.io/cluster-api/internal/runtime/registry, and CallExtension, urlForExtension,
// createHTTPClient and httpCall in sigs.k8s.io/cluster-api/internal/runtime/client.
type RuntimeClient interface {
	// GetAllExtensions returns the canonical names of the extension handlers registered for hook.
	GetAllExtensions(ctx context.Context, hook runtimecatalog.Hook, forObject client.Object) ([]string, error)

	// CallExtension calls the extension handler registered for hook under the canonical handler name
	// handler ("HANDLER_NAME.EXTENSION_NAME") and writes the answer into response. It returns an error
	// when the call fails or when the handler answers with a Failure response, as upstream CallExtension
	// does.
	CallExtension(ctx context.Context, hook runtimecatalog.Hook, forObject client.Object, handler string, request runtimehooksv1.RequestObject, response runtimehooksv1.ResponseObject) error
}

// NewRuntimeClient returns a Runtime SDK client that reads the extensions registered in the cluster.
func NewRuntimeClient(c client.Client) RuntimeClient {
	return &extensionRuntimeClient{client: c}
}

type extensionRuntimeClient struct {
	client client.Client
}

// GetAllExtensions mirrors the upstream client of the same name: the handlers of an extension are the
// ones the Runtime SDK discovered into the status of its ExtensionConfig object.
func (c *extensionRuntimeClient) GetAllExtensions(ctx context.Context, hook runtimecatalog.Hook, forObject client.Object) ([]string, error) {
	extensionConfigs := &runtimev1beta2.ExtensionConfigList{}
	if err := c.client.List(ctx, extensionConfigs); err != nil {
		return nil, errors.Wrap(err, "failed to list ExtensionConfig objects")
	}

	hookName := runtimecatalog.HookName(hook)

	handlers := []string{}
	for _, extensionConfig := range extensionConfigs.Items {
		// Handlers are only known after the extension was discovered by the Runtime SDK.
		if !conditions.IsTrue(&extensionConfig, runtimev1beta2.ExtensionConfigDiscoveredCondition) {
			continue
		}
		if extensionConfig.Spec.NamespaceSelector != nil && !c.namespaceMatches(ctx, extensionConfig.Spec.NamespaceSelector, forObject) {
			continue
		}
		for _, handler := range extensionConfig.Status.Handlers {
			if handler.RequestHook.Hook != hookName {
				continue
			}
			// Canonical handler name "HANDLER_NAME.EXTENSION_NAME", as in upstream NameForHandler.
			handlers = append(handlers, handler.Name+"."+extensionConfig.Name)
		}
	}

	return handlers, nil
}

func (c *extensionRuntimeClient) namespaceMatches(ctx context.Context, selector *metav1.LabelSelector, forObject client.Object) bool {
	namespace := &corev1.Namespace{}
	if err := c.client.Get(ctx, client.ObjectKey{Name: forObject.GetNamespace()}, namespace); err != nil {
		return false
	}

	parsed, err := metav1.LabelSelectorAsSelector(selector)
	if err != nil {
		return false
	}

	return parsed.Matches(labels.Set(namespace.Labels))
}

// canUpdateMachine and the CanUpdateMachine hook call live in inplace_canupdatemachine.go.

// CallExtension calls one registered extension handler. It mirrors CallExtension, urlForExtension,
// createHTTPClient, httpCall and validateResponseStatus of
// sigs.k8s.io/cluster-api/internal/runtime/client, which a provider cannot import: the handler's
// ExtensionConfig is read from the management cluster, the hook URL is computed from its clientConfig
// and the call is made over https, validating the extension certificate against the caBundle of the
// ExtensionConfig.
func (c *extensionRuntimeClient) CallExtension(ctx context.Context, hook runtimecatalog.Hook, forObject client.Object, handler string, request runtimehooksv1.RequestObject, response runtimehooksv1.ResponseObject) error {
	log := ctrl.LoggerFrom(ctx).WithValues("extensionHandler", handler, "hook", runtimecatalog.HookName(hook))
	ctx = ctrl.LoggerInto(ctx, log)

	extensionName, err := extensionNameFromHandlerName(handler)
	if err != nil {
		return errors.Wrapf(err, "failed to call extension handler %q", handler)
	}

	extensionConfig := &runtimev1beta2.ExtensionConfig{}
	if err := c.client.Get(ctx, client.ObjectKey{Name: extensionName}, extensionConfig); err != nil {
		return errors.Wrapf(err, "failed to call extension handler %q: failed to get ExtensionConfig %q", handler, extensionName)
	}

	// The handlers of an ExtensionConfig are only known once the Runtime SDK discovered them.
	if !conditions.IsTrue(extensionConfig, runtimev1beta2.ExtensionConfigDiscoveredCondition) {
		return errors.Errorf("failed to call extension handler %q: extension %q has not been discovered", handler, extensionName)
	}

	registration := runtimev1beta2.ExtensionHandler{}
	found := false

	for _, handlerRegistration := range extensionConfig.Status.Handlers {
		// Canonical handler name "HANDLER_NAME.EXTENSION_NAME", as in upstream NameForHandler.
		if handlerRegistration.Name+"."+extensionConfig.Name == handler {
			registration = handlerRegistration
			found = true

			break
		}
	}

	if !found {
		return errors.Errorf("failed to call extension handler %q: extension %q does not provide it", handler, extensionName)
	}

	// The provider builds the request with the hook types of api/runtime/hooks/v1alpha1 and does not
	// convert between hook versions, as the upstream catalog does.
	if registration.RequestHook.APIVersion != runtimehooksv1.GroupVersion.String() {
		return errors.Errorf("failed to call extension handler %q: extension %q serves hook %q at APIVersion %s, the provider only calls %s",
			handler, extensionName, registration.RequestHook.Hook, registration.RequestHook.APIVersion, runtimehooksv1.GroupVersion.String())
	}

	if !c.namespaceMatches(ctx, extensionConfig.Spec.NamespaceSelector, forObject) {
		return errors.Errorf("failed to call extension handler %q: namespaceSelector of ExtensionConfig %q does not match %s/%s",
			handler, extensionConfig.Name, forObject.GetNamespace(), forObject.GetName())
	}

	// The settings of the registration are merged into the request; the settings already in the
	// request win (upstream cloneAndAddSettings).
	settings := maps.Clone(extensionConfig.Spec.Settings)
	if settings == nil {
		settings = map[string]string{}
	}

	for key, value := range request.GetSettings() {
		settings[key] = value
	}

	request.SetSettings(settings)

	timeout := time.Duration(runtimehooksv1.DefaultHandlersTimeoutSeconds) * time.Second
	if registration.TimeoutSeconds != 0 {
		timeout = time.Duration(registration.TimeoutSeconds) * time.Second
	}

	extensionURL, err := urlForExtension(extensionConfig.Spec.ClientConfig, registration.RequestHook, strings.TrimSuffix(handler, "."+extensionName))
	if err != nil {
		return errors.Wrapf(err, "failed to call extension handler %q", handler)
	}

	httpClient, err := extensionHTTPClient(extensionConfig.Spec.ClientConfig.CABundle, extensionURL.Host)
	if err != nil {
		return errors.Wrapf(err, "failed to call extension handler %q: failed to create the http client", handler)
	}

	if timeout > 0 {
		query := extensionURL.Query()
		query.Add("timeout", strconv.Itoa(int(timeout.Seconds())))
		extensionURL.RawQuery = query.Encode()

		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	body, err := json.Marshal(request)
	if err != nil {
		return errors.Wrapf(err, "failed to call extension handler %q: failed to marshal the request", handler)
	}

	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, extensionURL.String(), bytes.NewBuffer(body))
	if err != nil {
		return errors.Wrapf(err, "failed to call extension handler %q: failed to create the http request", handler)
	}

	httpRequest.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(httpRequest)
	if err != nil {
		return extensionCallError(registration, response, errors.Wrapf(err, "http call to extension handler %q failed", handler))
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 512))
		if readErr != nil {
			return extensionCallError(registration, response, errors.Errorf("http call to extension handler %q failed: got response with status code %d != 200: failed to read response body", handler, resp.StatusCode))
		}

		return extensionCallError(registration, response, errors.Errorf("http call to extension handler %q failed: got response with status code %d != 200: response %q", handler, resp.StatusCode, string(respBody)))
	}

	if err := json.NewDecoder(resp.Body).Decode(response); err != nil {
		return extensionCallError(registration, response, errors.Wrapf(err, "http call to extension handler %q failed: failed to decode the response", handler))
	}

	// A handler that answers with Failure is an error, not a rollout decision
	// (upstream validateResponseStatus).
	return validateResponseStatus(response, "call extension handler", handler)
}

// extensionNameFromHandlerName mirrors ExtensionNameFromHandlerName of
// sigs.k8s.io/cluster-api/internal/runtime/client.
func extensionNameFromHandlerName(registeredHandlerName string) (string, error) {
	parts := strings.Split(registeredHandlerName, ".")
	if len(parts) != 2 {
		return "", errors.Errorf("registered handler name %s was not in the expected format (`HANDLER_NAME.EXTENSION_NAME`)", registeredHandlerName)
	}

	return parts[1], nil
}

// urlForExtension mirrors urlForExtension of sigs.k8s.io/cluster-api/internal/runtime/client.
func urlForExtension(config runtimev1beta2.ClientConfig, requestHook runtimev1beta2.GroupVersionHook, handlerName string) (*url.URL, error) {
	var extensionURL *url.URL

	if config.Service.Name != "" {
		host := config.Service.Name + "." + config.Service.Namespace + ".svc"
		if config.Service.Port != nil {
			host = net.JoinHostPort(host, strconv.Itoa(int(*config.Service.Port)))
		}

		extensionURL = &url.URL{Scheme: "https", Host: host}
		if config.Service.Path != "" {
			extensionURL.Path = config.Service.Path
		}
	} else {
		if config.URL == "" {
			return nil, errors.New("failed to compute URL: at least one of service and url must be defined in the clientConfig")
		}

		parsed, err := url.Parse(config.URL)
		if err != nil {
			return nil, errors.Wrap(err, "failed to compute URL: failed to parse url from clientConfig")
		}

		if parsed.Scheme != "https" {
			return nil, errors.Errorf("failed to compute URL: expected https scheme, got %s", parsed.Scheme)
		}

		extensionURL = parsed
	}

	group, version, err := splitAPIVersion(requestHook.APIVersion)
	if err != nil {
		return nil, err
	}

	// Append the hook path, e.g. /hooks.runtime.cluster.x-k8s.io/v1alpha1/canupdatemachine/<handler>.
	extensionURL.Path = path.Join(extensionURL.Path, runtimecatalog.GVHToPath(runtimecatalog.GroupVersionHook{
		Group:   group,
		Version: version,
		Hook:    requestHook.Hook,
	}, handlerName))

	return extensionURL, nil
}

func splitAPIVersion(apiVersion string) (string, string, error) {
	parts := strings.Split(apiVersion, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", errors.Errorf("APIVersion %q is not in the expected format (`GROUP/VERSION`)", apiVersion)
	}

	return parts[0], parts[1], nil
}

// extensionHTTPClient mirrors createHTTPClient of sigs.k8s.io/cluster-api/internal/runtime/client:
// the extension is called over https, validating its certificate with the caBundle of the
// ExtensionConfig, and redirects are not followed so the pinned server name cannot be bypassed.
func extensionHTTPClient(caBundle []byte, host string) (*http.Client, error) {
	tlsConfig := &tls.Config{ServerName: host}

	if len(caBundle) > 0 {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caBundle) {
			return nil, errors.New("the caBundle of the ExtensionConfig contains no valid PEM certificate")
		}

		tlsConfig.RootCAs = pool
	}

	return &http.Client{
		Transport: utilnet.SetTransportDefaults(&http.Transport{TLSClientConfig: tlsConfig}),
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}, nil
}

// extensionCallError applies the FailurePolicy of the handler registration to a transport level
// error, as upstream CallExtension does with errCallingExtensionHandler.
func extensionCallError(registration runtimev1beta2.ExtensionHandler, response runtimehooksv1.ResponseObject, err error) error {
	if registration.FailurePolicy == runtimev1beta2.FailurePolicyIgnore {
		response.SetStatus(runtimehooksv1.ResponseStatusSuccess)
		response.SetMessage("")

		return nil
	}

	return err
}

// validateResponseStatus mirrors validateResponseStatus of
// sigs.k8s.io/cluster-api/internal/runtime/client.
func validateResponseStatus(response runtimehooksv1.ResponseObject, operationName, targetName string) error {
	if response.GetStatus() != runtimehooksv1.ResponseStatusSuccess {
		return errors.Errorf("failed to %s %q: got response with status %q and message %q", operationName, targetName, response.GetStatus(), response.GetMessage())
	}

	return nil
}

// UpToDateResult carries the current and desired objects of one control plane Machine that is not up
// to date, the fork's counterpart of the UpToDateResult KubeadmControlPlane builds before it decides
// between an in-place update and a rollout (inplace.go:44-53, inplace_trigger.go:81-84).
//
// A nil DesiredInfraMachine or DesiredTalosConfig means the objects the in-place update would write
// could not be computed, which makes the Machine roll out like today.
type UpToDateResult struct {
	DesiredMachine      *clusterv1.Machine
	CurrentInfraMachine *unstructured.Unstructured
	DesiredInfraMachine *unstructured.Unstructured
	CurrentTalosConfig  *cabptv1.TalosConfig
	DesiredTalosConfig  *cabptv1.TalosConfig
}

// Test seams for the in-place update path. KubeadmControlPlane keeps the equivalent fields on its
// Reconciler (inplace.go:39-41), used by its tests (inplace_test.go:40-66); the reconciler struct of
// this fork belongs to stage L2, so the seams are package level here. They are nil unless a test sets
// them, so the production path always runs canUpdateMachine and triggerInPlaceUpdate.
var (
	overrideTryInPlaceUpdate     func(ctx context.Context, controlPlane *ControlPlane, machine *clusterv1.Machine, machineUpToDateResult UpToDateResult) (bool, error)
	overrideCanUpdateMachine     func(ctx context.Context, machine *clusterv1.Machine) (bool, error)
	overrideTriggerInPlaceUpdate func(ctx context.Context, machine *clusterv1.Machine, machineUpToDateResult UpToDateResult) error
)

// tryInPlaceUpdate decides between an in-place update and the scale down that rolls a Machine out,
// following tryInPlaceUpdate in inplace.go:44-53.
//
// It returns fallbackToScaleDown, which tells the caller to delete the Machine as today. Once the
// UpdateInProgressAnnotation is written the in-place update is in progress and the Machine is never
// rolled out for that change: canUpdateMachine is only called again if writing the annotation or the
// scale down itself fails.
func (r *TalosControlPlaneReconciler) tryInPlaceUpdate(
	ctx context.Context,
	controlPlane *ControlPlane,
	machineToInPlaceUpdate *clusterv1.Machine,
	machineUpToDateResult UpToDateResult,
) (bool, error) {
	if overrideTryInPlaceUpdate != nil {
		return overrideTryInPlaceUpdate(ctx, controlPlane, machineToInPlaceUpdate, machineUpToDateResult)
	}

	// A Machine cannot be updated in-place when one of the objects the update would write is missing,
	// e.g. when the InfraMachine or the TalosConfig was deleted (inplace_canupdatemachine.go:58-63).
	if machineUpToDateResult.DesiredMachine == nil ||
		machineUpToDateResult.CurrentInfraMachine == nil ||
		machineUpToDateResult.DesiredInfraMachine == nil ||
		machineUpToDateResult.CurrentTalosConfig == nil ||
		machineUpToDateResult.DesiredTalosConfig == nil {
		return true, nil
	}

	canUpdate, err := r.canUpdateMachine(ctx, machineToInPlaceUpdate, machineUpToDateResult)
	if err != nil {
		return false, errors.Wrapf(err, "failed to determine if Machine %s can be updated in-place", machineToInPlaceUpdate.Name)
	}

	if !canUpdate {
		return true, nil
	}

	return false, r.triggerInPlaceUpdate(ctx, controlPlane, machineToInPlaceUpdate, machineUpToDateResult)
}
