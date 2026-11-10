// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/go-logr/logr"
	. "github.com/onsi/gomega"
	runtimecatalog "sigs.k8s.io/cluster-api/api/runtime/catalog"
	runtimehooksv1 "sigs.k8s.io/cluster-api/api/runtime/hooks/v1alpha1"

	"github.com/siderolabs/cluster-api-control-plane-provider-talos/pkg/inplaceupdate"
)

// selfSignedCertFiles writes a throwaway certificate and key, the kind an operator puts behind the
// --cert-file and --key-file flags of the extension binary.
func selfSignedCertFiles(t *testing.T) (string, string) {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating the test key: %v", err)
	}

	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "talos-in-place-update"},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}

	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, key.Public(), key)
	if err != nil {
		t.Fatalf("creating the test certificate: %v", err)
	}

	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("encoding the test key: %v", err)
	}

	dir := t.TempDir()
	certPath := dir + "/cert.pem"
	keyPath := dir + "/key.pem"

	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER}), 0600); err != nil {
		t.Fatalf("writing the test certificate: %v", err)
	}

	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0600); err != nil {
		t.Fatalf("writing the test key: %v", err)
	}

	return certPath, keyPath
}

// TestHookPathsMatchTheRuntimeCatalog checks the paths the binary serves against the paths the
// Runtime SDK catalog derives for the hooks (catalog.GVHToPath), including the handler name the
// ExtensionConfig registers.
func TestHookPathsMatchTheRuntimeCatalog(t *testing.T) {
	g := NewWithT(t)

	for _, test := range []struct {
		hook  string
		serve string
	}{
		{hook: "Discovery", serve: discoveryPath},
		{hook: "CanUpdateMachine", serve: canUpdateMachinePath},
		{hook: "UpdateMachine", serve: updateMachinePath},
	} {
		path := runtimecatalog.GVHToPath(runtimecatalog.GroupVersionHook{
			Group:   runtimehooksv1.GroupVersion.Group,
			Version: runtimehooksv1.GroupVersion.Version,
			Hook:    test.hook,
		}, "")

		g.Expect(test.serve).To(Equal(path+"/"), "the extension does not serve the %s hook", test.hook)
		g.Expect(test.serve).To(HavePrefix(path+"/"), "the extension does not serve the %s hook of a registered handler", test.hook)
	}
}

// TestExtensionServesHooksOverHTTPS checks that the hooks are reachable over TLS with the certificate
// from --cert-file and are not reachable over plain http: Cluster API only calls an extension whose
// clientConfig.url is https.
func TestExtensionServesHooksOverHTTPS(t *testing.T) {
	g := NewWithT(t)

	certPath, keyPath := selfSignedCertFiles(t)

	handler := inplaceupdate.NewHandler(nil, logr.Discard(), "", 0)

	server, err := hookServer("127.0.0.1:0", certPath, keyPath, handler, logr.Discard())
	g.Expect(err).NotTo(HaveOccurred())

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	g.Expect(err).NotTo(HaveOccurred())
	port := listener.Addr().(*net.TCPAddr).Port

	go func() {
		_ = server.Serve(tls.NewListener(listener, server.TLSConfig))
	}()

	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		_ = server.Shutdown(shutdownCtx)
	}()

	pair, err := tls.LoadX509KeyPair(certPath, keyPath)
	g.Expect(err).NotTo(HaveOccurred())

	// tls.X509Certificate is the DER encoding of the certificate, not PEM.
	cert, err := x509.ParseCertificate(pair.Certificate[0])
	g.Expect(err).NotTo(HaveOccurred())

	pool := x509.NewCertPool()
	pool.AddCert(cert)

	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	response, err := postJSON(ctx, client, fmt.Sprintf("https://127.0.0.1:%d%s", port, discoveryPath))
	g.Expect(err).NotTo(HaveOccurred())

	defer response.Body.Close()

	g.Expect(response.StatusCode).To(Equal(http.StatusOK))

	discoveryResponse := &runtimehooksv1.DiscoveryResponse{}
	g.Expect(json.NewDecoder(response.Body).Decode(discoveryResponse)).To(Succeed())
	g.Expect(discoveryResponse.Status).To(Equal(runtimehooksv1.ResponseStatusSuccess))

	hooks := []string{}
	for _, extensionHandler := range discoveryResponse.Handlers {
		hooks = append(hooks, extensionHandler.RequestHook.Hook)
	}

	g.Expect(hooks).To(ConsistOf("CanUpdateMachine", "UpdateMachine"))

	// The same port does not serve the hooks over plain http: the connection fails, or Go's TLS listener
	// answers the plaintext request with 400 "Client sent an HTTP request to an HTTPS server".
	plainTextResponse, err := postJSON(ctx, &http.Client{}, fmt.Sprintf("http://127.0.0.1:%d%s", port, discoveryPath))
	if err == nil {
		defer plainTextResponse.Body.Close()
		g.Expect(plainTextResponse.StatusCode).NotTo(Equal(http.StatusOK), "the hooks are reachable over plain http")
	}
}

// postJSON sends an empty JSON body with POST, the way the Runtime SDK client calls a hook.
func postJSON(ctx context.Context, client *http.Client, url string) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader([]byte("{}")))
	if err != nil {
		return nil, err
	}

	request.Header.Set("Content-Type", "application/json")

	return client.Do(request)
}

// TestExtensionNeedsACertificate checks that the binary refuses to start without the certificate the
// Runtime SDK requires.
func TestExtensionNeedsACertificate(t *testing.T) {
	g := NewWithT(t)

	server, err := hookServer("127.0.0.1:8443", "", "", inplaceupdate.NewHandler(nil, logr.Discard(), "", 0), logr.Discard())
	g.Expect(server).To(BeNil())
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("certificate"))
}
