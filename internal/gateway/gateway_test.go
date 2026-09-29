package gateway

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func credentials(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	os.WriteFile(filepath.Join(d, "username"), []byte("alice"), 0600)
	os.WriteFile(filepath.Join(d, "password"), []byte("tenant-password-long-enough"), 0600)
	return d
}
func TestEveryPublicPathRequiresAuthentication(t *testing.T) {
	calls := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "" {
			t.Error("forwarded external credentials")
		}
		io.WriteString(w, "ok")
	}))
	defer up.Close()
	g, err := New(Config{AuthDir: credentials(t), HTTPBackend: up.URL, MCPBackend: up.URL})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/", "/ui/", "/v0/servers", "/mcp", "/v0/health", "/docs", "/assets/app.js", "/readyz"} {
		r := httptest.NewRequest("GET", path, nil)
		w := httptest.NewRecorder()
		g.ServeHTTP(w, r)
		if w.Code != 401 {
			t.Fatalf("%s unprotected (%d)", path, w.Code)
		}
	}
	if calls != 0 {
		t.Fatal("unauthenticated traffic reached backend")
	}
	r := httptest.NewRequest("GET", "/v0/servers", nil)
	r.SetBasicAuth("alice", "tenant-password-long-enough")
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)
	if w.Code != 200 || calls != 1 {
		t.Fatal(w.Code, w.Body.String())
	}
}
func TestRuntimeExecutionDenied(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("runtime call reached registry") }))
	defer up.Close()
	g, _ := New(Config{AuthDir: credentials(t), HTTPBackend: up.URL, MCPBackend: up.URL})
	for _, tc := range []struct{ path, body string }{{"/v0/deployments", "{}"}, {"/v0/runtimes", "{}"}, {"/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"deploy_server"}}`}, {"/mcp", `[{"method":"tools/call","params":{"name":"deploy_agent"}}]`}, {"/v0/../v0/deployments", "{}"}, {"/mcp", `{"method":"tools/call","params":{"name":"remove_deployment"}}`}} {
		r := httptest.NewRequest("POST", tc.path, strings.NewReader(tc.body))
		r.SetBasicAuth("alice", "tenant-password-long-enough")
		w := httptest.NewRecorder()
		g.ServeHTTP(w, r)
		if w.Code < 400 {
			t.Fatalf("runtime accepted: %s %s", tc.path, tc.body)
		}
	}
}
func TestMCPDiscriminatorCaseCannotBypassCatalogFilter(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("ambiguous runtime call reached registry")
	}))
	defer up.Close()
	g, _ := New(Config{AuthDir: credentials(t), HTTPBackend: up.URL, MCPBackend: up.URL})
	for _, body := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"deploy_server"},"Method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"deploy_server","Name":"list_servers"}}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"deploy_server"},"Params":{"name":"list_servers"}}`,
	} {
		r := httptest.NewRequest("POST", "/mcp", strings.NewReader(body))
		r.SetBasicAuth("alice", "tenant-password-long-enough")
		w := httptest.NewRecorder()
		g.ServeHTTP(w, r)
		if w.Code != 403 {
			t.Errorf("mixed-case MCP bypass: status=%d body=%s", w.Code, body)
		}
	}
}
func TestCredentialRemovalFailsClosedAndRotationRecovers(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer up.Close()
	dir := credentials(t)
	g, _ := New(Config{AuthDir: dir, HTTPBackend: up.URL, MCPBackend: up.URL})
	os.Remove(filepath.Join(dir, "password"))
	r := httptest.NewRequest("GET", "/", nil)
	r.SetBasicAuth("alice", "tenant-password-long-enough")
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)
	if w.Code != 503 {
		t.Fatal(w.Code)
	}
	os.WriteFile(filepath.Join(dir, "password"), []byte("new-tenant-password-long-enough"), 0600)
	w = httptest.NewRecorder()
	g.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("old password accepted")
	}
	r.SetBasicAuth("alice", "new-tenant-password-long-enough")
	w = httptest.NewRecorder()
	g.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal("rotation failed")
	}
}
func TestReadinessDetectsDatabaseAndMCPFailures(t *testing.T) {
	dbOK, mcpOK := true, true
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v0/servers" {
			if !dbOK {
				w.WriteHeader(500)
				return
			}
			io.WriteString(w, `{"servers":[]}`)
			return
		}
		if r.URL.Path == "/mcp" {
			if !mcpOK {
				w.WriteHeader(503)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-03-26","capabilities":{},"serverInfo":{"name":"registry","version":"0.3.3"}}}`)
			return
		}
		t.Error("readiness used static health")
	}))
	defer up.Close()
	g, _ := New(Config{AuthDir: credentials(t), HTTPBackend: up.URL, MCPBackend: up.URL})
	probe := func() int {
		w := httptest.NewRecorder()
		g.Ready(w, httptest.NewRequest("GET", "/readyz", nil))
		return w.Code
	}
	if probe() != 200 {
		t.Fatal("healthy not ready")
	}
	dbOK = false
	if probe() != 503 {
		t.Fatal("DB outage ready")
	}
	dbOK = true
	mcpOK = false
	if probe() != 503 {
		t.Fatal("MCP outage ready")
	}
	mcpOK = true
	if probe() != 200 {
		t.Fatal("recovery not ready")
	}
}

func TestReadinessDoesNotDependOnCatalogPayloadSize(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v0/servers" {
			if r.URL.Query().Get("search") == "//" {
				io.WriteString(w, `{"servers":[]}`)
			} else {
				// Valid remote header descriptions can exceed 1MiB after JSON
				// escaping, despite the upstream 1MiB publish request limit.
				json.NewEncoder(w).Encode(map[string]any{"servers": []any{map[string]any{"server": map[string]any{
					"name": "cloud.deepai/large", "remotes": []any{map[string]any{"type": "streamable-http",
						"url": "https://deepai.cloud/mcp", "headers": []any{map[string]any{"name": "X-Note", "description": strings.Repeat("\u2028", 200000)}}}}}}}})
			}
			return
		}
		io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-03-26","capabilities":{},"serverInfo":{"name":"registry","version":"0.3.3"}}}`)
	}))
	defer up.Close()
	g, _ := New(Config{AuthDir: credentials(t), HTTPBackend: up.URL, MCPBackend: up.URL})
	w := httptest.NewRecorder()
	g.Ready(w, httptest.NewRequest("GET", "/readyz", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("valid catalog payload makes a healthy registry unavailable: %d %s", w.Code, w.Body.String())
	}
}

func TestReadinessConsumesSSEAndDeletesItsSession(t *testing.T) {
	deleted := false
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v0/servers" {
			io.WriteString(w, `{"servers":[]}`)
			return
		}
		if r.Method == "DELETE" {
			if r.Header.Get("Mcp-Session-Id") != "probe-session" {
				t.Error("wrong session deleted")
			}
			deleted = true
			w.WriteHeader(200)
			return
		}
		if r.Header.Get("Accept") != "application/json, text/event-stream" {
			t.Error("missing MCP Accept")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Mcp-Session-Id", "probe-session")
		io.WriteString(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"protocolVersion\":\"2025-06-18\",\"capabilities\":{},\"serverInfo\":{\"name\":\"registry\",\"version\":\"0.3.3\"}}}\n\n")
	}))
	defer up.Close()
	g, _ := New(Config{AuthDir: credentials(t), HTTPBackend: up.URL, MCPBackend: up.URL})
	w := httptest.NewRecorder()
	g.Ready(w, httptest.NewRequest("GET", "/readyz", nil))
	if w.Code != 200 || !deleted {
		t.Fatalf("SSE probe failed or leaked session: %d deleted=%v", w.Code, deleted)
	}
}
func TestMCPProxyRewritesLoopbackHostAndPreservesSession(t *testing.T) {
	var backendHost string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != backendHost {
			t.Errorf("MCP DNS rebinding protection would reject %s", r.Host)
		}
		if r.Header.Get("Mcp-Session-Id") != "session-a" {
			t.Error("session header lost")
		}
		io.WriteString(w, `{"jsonrpc":"2.0","result":{"tools":[]},"id":2}`)
	}))
	defer up.Close()
	backendHost = strings.TrimPrefix(up.URL, "http://")
	g, _ := New(Config{AuthDir: credentials(t), HTTPBackend: up.URL, MCPBackend: up.URL})
	r := httptest.NewRequest("POST", "http://catalog.tenant-a.svc.cluster.local:8080/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`))
	r.SetBasicAuth("alice", "tenant-password-long-enough")
	r.Header.Set("Mcp-Session-Id", "session-a")
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
}
func TestProxySurvivesActualBackendConnectionFailure(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	address := up.URL
	up.Close()
	g, _ := New(Config{AuthDir: credentials(t), HTTPBackend: address, MCPBackend: address})
	r := httptest.NewRequest("GET", "/v0/servers", nil)
	r.SetBasicAuth("alice", "tenant-password-long-enough")
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)
	if w.Code != 502 {
		t.Fatal(w.Code)
	}
	w = httptest.NewRecorder()
	g.Ready(w, r)
	if w.Code != 503 {
		t.Fatal(w.Code)
	}
}
func TestBrowserCrossOriginRequestIsDenied(t *testing.T) {
	g, _ := New(Config{AuthDir: credentials(t)})
	r := httptest.NewRequest("POST", "http://catalog.tenant-a.svc.cluster.local:8080/v0/servers", strings.NewReader(`{}`))
	r.SetBasicAuth("alice", "tenant-password-long-enough")
	r.Header.Set("Origin", "https://other.example")
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal(w.Code)
	}
}

func TestExpiredTLSMaterialIsNotReady(t *testing.T) {
	dir := credentials(t)
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "catalog.example.test"}, DNSNames: []string{"catalog.example.test"}, NotBefore: time.Now().Add(-48 * time.Hour), NotAfter: time.Now().Add(-24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature}, &x509.Certificate{SerialNumber: big.NewInt(1)}, pub, priv)
	if err != nil {
		t.Fatal(err)
	}
	key, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	certFile, keyFile := filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600)
	os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}), 0600)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v0/servers" {
			io.WriteString(w, `{"servers":[]}`)
			return
		}
		io.WriteString(w, `{"result":{"protocolVersion":"2025-06-18","serverInfo":{"name":"registry"}}}`)
	}))
	defer up.Close()
	g, _ := New(Config{AuthDir: dir, HTTPBackend: up.URL, MCPBackend: up.URL})
	// The live process obtains TLS paths from its environment; readiness must inspect them.
	t.Setenv("TLS_CERT_FILE", certFile)
	t.Setenv("TLS_KEY_FILE", keyFile)
	w := httptest.NewRecorder()
	g.Ready(w, httptest.NewRequest("GET", "/readyz", nil))
	if w.Code != 503 {
		t.Fatalf("expired TLS certificate reports Ready: %d", w.Code)
	}
}

func TestTLSReadinessRequiresServerCertificatePurpose(t *testing.T) {
	for _, tc := range []struct {
		name      string
		usage     []x509.ExtKeyUsage
		unknown   []asn1.ObjectIdentifier
		valid     bool
		extension *pkix.Extension
	}{
		{"client-only", []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, nil, false, nil},
		{"code-signing", []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning}, nil, false, nil},
		{"unknown-only", nil, []asn1.ObjectIdentifier{{1, 2, 3, 4}}, false, nil},
		{"server", []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, nil, true, nil},
		{"server-and-client", []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}, nil, true, nil},
		{"any", []x509.ExtKeyUsage{x509.ExtKeyUsageAny}, nil, true, nil},
		{"unrestricted", nil, nil, true, nil},
		{"unknown-critical", []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, nil, false,
			&pkix.Extension{Id: asn1.ObjectIdentifier{1, 2, 3, 4}, Critical: true, Value: []byte{5, 0}}},
		{"unknown-noncritical", []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, nil, true,
			&pkix.Extension{Id: asn1.ObjectIdentifier{1, 2, 3, 4}, Value: []byte{5, 0}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pub, priv, err := ed25519.GenerateKey(rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			template := &x509.Certificate{SerialNumber: big.NewInt(1), DNSNames: []string{"catalog.example.test"},
				NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
				KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: tc.usage, UnknownExtKeyUsage: tc.unknown}
			if tc.extension != nil {
				template.ExtraExtensions = []pkix.Extension{*tc.extension}
			}
			der, err := x509.CreateCertificate(rand.Reader, template, template, pub, priv)
			if err != nil {
				t.Fatal(err)
			}
			key, err := x509.MarshalPKCS8PrivateKey(priv)
			if err != nil {
				t.Fatal(err)
			}
			dir := credentials(t)
			certFile, keyFile := filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
			if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}), 0600); err != nil {
				t.Fatal(err)
			}
			pair, err := tls.LoadX509KeyPair(certFile, keyFile)
			if err != nil {
				t.Fatal(err)
			}
			cert, err := x509.ParseCertificate(der)
			if err != nil {
				t.Fatal(err)
			}
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v0/servers" {
					io.WriteString(w, `{"servers":[]}`)
					return
				}
				io.WriteString(w, `{"result":{"protocolVersion":"2025-06-18","serverInfo":{"name":"registry"}}}`)
			}))
			defer up.Close()
			g, err := New(Config{AuthDir: dir, HTTPBackend: up.URL, MCPBackend: up.URL})
			if err != nil {
				t.Fatal(err)
			}
			server := httptest.NewUnstartedServer(g)
			server.TLS = &tls.Config{Certificates: []tls.Certificate{pair}}
			server.StartTLS()
			defer server.Close()
			roots := x509.NewCertPool()
			roots.AddCert(cert)
			transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: "catalog.example.test"}}
			defer transport.CloseIdleConnections()
			client := &http.Client{Transport: transport, Timeout: time.Second}
			req, _ := http.NewRequest("GET", server.URL+"/v0/servers", nil)
			req.SetBasicAuth("alice", "tenant-password-long-enough")
			resp, err := client.Do(req)
			if tc.valid {
				if err != nil {
					t.Fatal(err)
				}
				resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					t.Fatal(resp.StatusCode)
				}
			} else {
				if resp != nil {
					resp.Body.Close()
				}
				if tc.extension != nil && tc.extension.Critical {
					var unhandled x509.UnhandledCriticalExtension
					if !errors.As(err, &unhandled) {
						t.Fatalf("expected actual HTTPS rejection for critical extension, got %v", err)
					}
				} else {
					var invalid x509.CertificateInvalidError
					if !errors.As(err, &invalid) || invalid.Reason != x509.IncompatibleUsage {
						t.Fatalf("expected actual HTTPS rejection for certificate purpose, got %v", err)
					}
				}
				t.Logf("Trusted matching-host HTTPS connection rejected certificate: %v", err)
			}
			t.Setenv("TLS_CERT_FILE", certFile)
			t.Setenv("TLS_KEY_FILE", keyFile)
			w := httptest.NewRecorder()
			g.Ready(w, httptest.NewRequest("GET", "/readyz", nil))
			want := http.StatusServiceUnavailable
			if tc.valid {
				want = http.StatusOK
			}
			if w.Code != want {
				t.Fatalf("Ready disagrees with real HTTPS verification: got %d, want %d", w.Code, want)
			}
		})
	}
}
