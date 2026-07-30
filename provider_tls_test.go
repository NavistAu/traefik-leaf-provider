// TLS tests for both hops. CAUTION applies here as it does in
// provider_test.go: these are compiled, and compiled-green is not proof
// the plugin works under Yaegi. test/e2e/run.sh drives the same paths
// through a real traefik against a real mTLS-fronted leaf API.
package traefik_leaf_provider

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/traefik/genconf/dynamic"
)

// testPKI is a throwaway CA plus a server and a client leaf certificate,
// written to disk so the plugin can load them the way an operator would.
type testPKI struct {
	dir                    string
	caFile                 string
	serverCert, serverKey  string
	clientCert, clientKey  string
	caPool                 *x509.CertPool
	serverPair, clientPair tls.Certificate
}

func newTestPKI(t *testing.T) *testPKI {
	t.Helper()
	dir := t.TempDir()

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ca key: %v", err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "leaf-provider-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("ca cert: %v", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatalf("parse ca: %v", err)
	}

	p := &testPKI{
		dir:        dir,
		caFile:     filepath.Join(dir, "ca.pem"),
		serverCert: filepath.Join(dir, "server.crt"),
		serverKey:  filepath.Join(dir, "server.key"),
		clientCert: filepath.Join(dir, "client.crt"),
		clientKey:  filepath.Join(dir, "client.key"),
		caPool:     x509.NewCertPool(),
	}
	p.caPool.AddCert(caCert)
	writePEM(t, p.caFile, "CERTIFICATE", caDER)

	issue := func(cn string, server bool, certPath, keyPath string) tls.Certificate {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatalf("%s key: %v", cn, err)
		}
		tmpl := &x509.Certificate{
			SerialNumber: big.NewInt(time.Now().UnixNano()),
			Subject:      pkix.Name{CommonName: cn},
			NotBefore:    time.Now().Add(-time.Hour),
			NotAfter:     time.Now().Add(24 * time.Hour),
			KeyUsage:     x509.KeyUsageDigitalSignature,
		}
		if server {
			tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
			tmpl.DNSNames = []string{"leaf.example.com"}
			tmpl.IPAddresses = []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}
		} else {
			tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
		if err != nil {
			t.Fatalf("%s cert: %v", cn, err)
		}
		writePEM(t, certPath, "CERTIFICATE", der)
		keyDER, err := x509.MarshalECPrivateKey(key)
		if err != nil {
			t.Fatalf("%s marshal key: %v", cn, err)
		}
		writePEM(t, keyPath, "EC PRIVATE KEY", keyDER)
		pair, err := tls.LoadX509KeyPair(certPath, keyPath)
		if err != nil {
			t.Fatalf("%s loadpair: %v", cn, err)
		}
		return pair
	}

	p.serverPair = issue("leaf.example.com", true, p.serverCert, p.serverKey)
	p.clientPair = issue("main-instance", false, p.clientCert, p.clientKey)
	return p
}

func writePEM(t *testing.T, path, blockType string, der []byte) {
	t.Helper()
	b := pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der})
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// tlsLeaf starts a rawdata server over TLS. requireClientCert turns it
// into an mTLS endpoint that rejects an unauthenticated caller.
func tlsLeaf(t *testing.T, p *testPKI, requireClientCert bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(sampleRawdata))
	}))
	cfg := &tls.Config{
		Certificates: []tls.Certificate{p.serverPair},
		MinVersion:   tls.VersionTLS12,
	}
	if requireClientCert {
		cfg.ClientAuth = tls.RequireAndVerifyClientCert
		cfg.ClientCAs = p.caPool
	}
	srv.TLS = cfg
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

// --- hop 1: the plugin's own poll client ----------------------------

func TestPollOverTLSWithPrivateCA(t *testing.T) {
	pki := newTestPKI(t)
	srv := tlsLeaf(t, pki, false)

	p := testProvider(t, []Endpoint{{
		Name: "alpha", APIURL: srv.URL, WebURL: "https://leaf.example.com:443",
		TLS: &ClientTLS{RootCAs: []string{pki.caFile}},
	}})

	cfg, err := p.fetchEndpoint(context.Background(), 0)
	if err != nil {
		t.Fatalf("poll over TLS with private CA failed: %v", err)
	}
	if len(cfg.Routers) != 2 {
		t.Fatalf("routers = %d, want 2", len(cfg.Routers))
	}
}

func TestPollOverTLSRejectsUntrustedCA(t *testing.T) {
	pki := newTestPKI(t)
	srv := tlsLeaf(t, pki, false)

	// No rootCAs: the private CA is not in the system pool.
	p := testProvider(t, []Endpoint{{
		Name: "alpha", APIURL: srv.URL, WebURL: "https://leaf.example.com:443",
	}})

	if _, err := p.fetchEndpoint(context.Background(), 0); err == nil {
		t.Fatal("expected verification failure against an untrusted private CA")
	} else if !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("expected a certificate error, got: %v", err)
	}
}

func TestPollOverMutualTLS(t *testing.T) {
	pki := newTestPKI(t)
	srv := tlsLeaf(t, pki, true)

	t.Run("with client certificate", func(t *testing.T) {
		p := testProvider(t, []Endpoint{{
			Name: "alpha", APIURL: srv.URL, WebURL: "https://leaf.example.com:443",
			TLS: &ClientTLS{
				RootCAs:      []string{pki.caFile},
				Certificates: []Certificate{{CertFile: pki.clientCert, KeyFile: pki.clientKey}},
			},
		}})
		if _, err := p.fetchEndpoint(context.Background(), 0); err != nil {
			t.Fatalf("mTLS poll failed: %v", err)
		}
	})

	t.Run("without client certificate", func(t *testing.T) {
		p := testProvider(t, []Endpoint{{
			Name: "alpha", APIURL: srv.URL, WebURL: "https://leaf.example.com:443",
			TLS: &ClientTLS{RootCAs: []string{pki.caFile}},
		}})
		if _, err := p.fetchEndpoint(context.Background(), 0); err == nil {
			t.Fatal("mTLS endpoint accepted a caller presenting no client certificate")
		}
	})
}

func TestServerNameOverride(t *testing.T) {
	pki := newTestPKI(t)
	srv := tlsLeaf(t, pki, false)

	// The cert carries IP SAN 127.0.0.1, so serverName must still verify
	// when it names a SAN the certificate actually has.
	p := testProvider(t, []Endpoint{{
		Name: "alpha", APIURL: srv.URL, WebURL: "https://leaf.example.com:443",
		TLS: &ClientTLS{RootCAs: []string{pki.caFile}, ServerName: "leaf.example.com"},
	}})
	if _, err := p.fetchEndpoint(context.Background(), 0); err != nil {
		t.Fatalf("serverName override failed: %v", err)
	}

	// A serverName the certificate does NOT carry must fail.
	p2 := testProvider(t, []Endpoint{{
		Name: "alpha", APIURL: srv.URL, WebURL: "https://leaf.example.com:443",
		TLS: &ClientTLS{RootCAs: []string{pki.caFile}, ServerName: "wrong.example.com"},
	}})
	if _, err := p2.fetchEndpoint(context.Background(), 0); err == nil {
		t.Fatal("expected hostname verification failure for a serverName not in the cert")
	}
}

func TestInsecureSkipVerifyAcceptsUntrusted(t *testing.T) {
	pki := newTestPKI(t)
	srv := tlsLeaf(t, pki, false)

	p := testProvider(t, []Endpoint{{
		Name: "alpha", APIURL: srv.URL, WebURL: "https://leaf.example.com:443",
		TLS: &ClientTLS{InsecureSkipVerify: true},
	}})
	if _, err := p.fetchEndpoint(context.Background(), 0); err != nil {
		t.Fatalf("insecureSkipVerify should accept the untrusted cert: %v", err)
	}
}

// --- TLS failures must isolate, never abort startup -----------------

func TestUnreadableCertIsIsolatedNotFatal(t *testing.T) {
	pki := newTestPKI(t)
	good := tlsLeaf(t, pki, false)

	// New() must NOT read the files, so a bogus path is accepted here and
	// only surfaces as a per-endpoint failure at poll time.
	p := testProvider(t, []Endpoint{
		{Name: "good", APIURL: good.URL, WebURL: "https://leaf.example.com:443",
			TLS: &ClientTLS{RootCAs: []string{pki.caFile}}},
		{Name: "broken", APIURL: good.URL, WebURL: "https://leaf.example.com:443",
			TLS: &ClientTLS{RootCAs: []string{filepath.Join(pki.dir, "does-not-exist.pem")}}},
	})

	if _, err := p.fetchEndpoint(context.Background(), 1); err == nil {
		t.Fatal("expected an error for the unreadable rootCA")
	}

	out := make(chan json.Marshaler, 1)
	p.refresh(context.Background(), out)
	payload := (<-out).(dynamic.JSONPayload)
	if _, ok := payload.Configuration.HTTP.Routers["grafana-good"]; !ok {
		t.Fatalf("healthy endpoint blanked by a sibling's TLS failure: %v",
			keys(payload.Configuration.HTTP.Routers))
	}
}

func TestNewRejectsHalfSpecifiedKeypair(t *testing.T) {
	for name, certs := range map[string][]Certificate{
		"missing key":  {{CertFile: "/x.crt"}},
		"missing cert": {{KeyFile: "/x.key"}},
		"both empty":   {{}},
	} {
		cfg := CreateConfig()
		cfg.Endpoints = []Endpoint{{
			Name: "alpha", APIURL: "http://x", WebURL: "http://x",
			TLS: &ClientTLS{Certificates: certs},
		}}
		if _, err := New(context.Background(), cfg, "test"); err == nil {
			t.Fatalf("%s: expected New to reject a half-specified keypair", name)
		}
	}
}

func TestNewAcceptsUnreadableCertPaths(t *testing.T) {
	cfg := CreateConfig()
	cfg.Endpoints = []Endpoint{{
		Name: "alpha", APIURL: "http://x", WebURL: "http://x",
		TLS: &ClientTLS{
			RootCAs:      []string{"/not/mounted/yet.pem"},
			Certificates: []Certificate{{CertFile: "/not/mounted/yet.crt", KeyFile: "/not/mounted/yet.key"}},
		},
	}}
	// A secret still mounting during a restart must not abort provider init.
	if _, err := New(context.Background(), cfg, "test"); err != nil {
		t.Fatalf("New must not read cert files, but failed: %v", err)
	}
}

func TestBadPEMIsReportedClearly(t *testing.T) {
	dir := t.TempDir()
	junk := filepath.Join(dir, "junk.pem")
	if err := os.WriteFile(junk, []byte("this is not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := buildTLSConfig(&ClientTLS{RootCAs: []string{junk}})
	if err == nil || !strings.Contains(err.Error(), "no PEM certificates found") {
		t.Fatalf("want a clear no-PEM error, got: %v", err)
	}
}

func TestPollClientIsCachedAndScoped(t *testing.T) {
	pki := newTestPKI(t)
	p := testProvider(t, []Endpoint{
		{Name: "plain", APIURL: "http://x", WebURL: "http://x"},
		{Name: "tls", APIURL: "https://x", WebURL: "https://x",
			TLS: &ClientTLS{RootCAs: []string{pki.caFile}}},
	})

	plain, err := p.clientFor(0)
	if err != nil {
		t.Fatal(err)
	}
	if plain != p.client {
		t.Fatal("endpoint without TLS should reuse the shared client")
	}

	first, err := p.clientFor(1)
	if err != nil {
		t.Fatal(err)
	}
	if first == p.client {
		t.Fatal("endpoint with TLS must get its own client")
	}
	second, err := p.clientFor(1)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatal("TLS client should be built once and cached")
	}
	if first.Timeout != p.client.Timeout {
		t.Fatalf("TLS client lost the request timeout: %v vs %v", first.Timeout, p.client.Timeout)
	}
}

// --- hop 2: the emitted serversTransport ----------------------------

func TestEmitsServersTransportFromTLSBlock(t *testing.T) {
	pki := newTestPKI(t)
	srv := tlsLeaf(t, pki, false)

	p := testProvider(t, []Endpoint{{
		Name: "alpha", APIURL: srv.URL, WebURL: "https://leaf.example.com:443",
		TLS: &ClientTLS{
			RootCAs:      []string{pki.caFile},
			Certificates: []Certificate{{CertFile: pki.clientCert, KeyFile: pki.clientKey}},
			ServerName:   "leaf.example.com",
		},
	}})

	cfg, err := p.fetchEndpoint(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}

	svc := cfg.Services["alpha-leaf"]
	if svc.LoadBalancer.ServersTransport != "alpha-transport" {
		t.Fatalf("service should reference the generated transport, got %q",
			svc.LoadBalancer.ServersTransport)
	}
	st, ok := cfg.ServersTransports["alpha-transport"]
	if !ok {
		t.Fatalf("generated transport missing: %v", cfg.ServersTransports)
	}
	if st.ServerName != "leaf.example.com" {
		t.Fatalf("serverName not mirrored: %q", st.ServerName)
	}
	if len(st.RootCAs) != 1 || st.RootCAs[0] != pki.caFile {
		t.Fatalf("rootCAs not mirrored: %v", st.RootCAs)
	}
	if len(st.Certificates) != 1 || st.Certificates[0].CertFile != pki.clientCert {
		t.Fatalf("client certificate not mirrored: %+v", st.Certificates)
	}
}

func TestExplicitServersTransportWins(t *testing.T) {
	pki := newTestPKI(t)
	srv := tlsLeaf(t, pki, false)

	p := testProvider(t, []Endpoint{{
		Name: "alpha", APIURL: srv.URL, WebURL: "https://leaf.example.com:443",
		TLS:              &ClientTLS{RootCAs: []string{pki.caFile}},
		ServersTransport: "spiffe-mesh@file",
	}})

	cfg, err := p.fetchEndpoint(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Services["alpha-leaf"].LoadBalancer.ServersTransport; got != "spiffe-mesh@file" {
		t.Fatalf("explicit serversTransport ignored, got %q", got)
	}
	if len(cfg.ServersTransports) != 0 {
		t.Fatalf("should not generate a transport when one is named: %v", cfg.ServersTransports)
	}
}

func TestNoTLSLeavesServersTransportUnset(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(sampleRawdata))
	}))
	defer srv.Close()

	p := testProvider(t, []Endpoint{{Name: "alpha", APIURL: srv.URL, WebURL: "http://192.0.2.10:80"}})
	cfg, err := p.fetchEndpoint(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	// v1 behaviour: empty means traefik's global default transport.
	if got := cfg.Services["alpha-leaf"].LoadBalancer.ServersTransport; got != "" {
		t.Fatalf("serversTransport should stay empty without TLS config, got %q", got)
	}
	if len(cfg.ServersTransports) != 0 {
		t.Fatalf("no transports should be emitted, got %v", cfg.ServersTransports)
	}
}

func TestServersTransportsMergeAcrossEndpoints(t *testing.T) {
	pki := newTestPKI(t)
	srv := tlsLeaf(t, pki, false)

	p := testProvider(t, []Endpoint{
		{Name: "alpha", APIURL: srv.URL, WebURL: "https://a:443",
			TLS: &ClientTLS{RootCAs: []string{pki.caFile}}},
		{Name: "beta", APIURL: srv.URL, WebURL: "https://b:443",
			TLS: &ClientTLS{RootCAs: []string{pki.caFile}}},
	})

	out := make(chan json.Marshaler, 1)
	p.refresh(context.Background(), out)
	got := (<-out).(dynamic.JSONPayload).Configuration.HTTP

	for _, name := range []string{"alpha-transport", "beta-transport"} {
		if _, ok := got.ServersTransports[name]; !ok {
			t.Fatalf("merged config missing %s: %v", name, got.ServersTransports)
		}
	}
}
