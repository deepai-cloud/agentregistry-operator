// Package gateway protects the unmodified upstream registry, including its UI and MCP.
package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"github.com/deepai-cloud/agentregistry-operator/internal/certificates"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

type Config struct{ AuthDir, HTTPBackend, MCPBackend string }
type Gateway struct {
	config              Config
	httpProxy, mcpProxy *httputil.ReverseProxy
	client              *http.Client
}

func New(c Config) (*Gateway, error) {
	if c.AuthDir == "" {
		return nil, errors.New("AUTH_DIR is required")
	}
	if c.HTTPBackend == "" {
		c.HTTPBackend = "http://127.0.0.1:8080"
	}
	if c.MCPBackend == "" {
		c.MCPBackend = "http://127.0.0.1:31313"
	}
	httpProxy, err := proxy(c.HTTPBackend)
	if err != nil {
		return nil, err
	}
	mcpProxy, err := proxy(c.MCPBackend)
	if err != nil {
		return nil, err
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &Gateway{config: c, httpProxy: httpProxy, mcpProxy: mcpProxy, client: &http.Client{Timeout: 3 * time.Second, Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}
func proxy(target string) (*httputil.ReverseProxy, error) {
	u, err := url.Parse(target)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.User != nil {
		return nil, errors.New("backend must be loopback HTTP")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.ResponseHeaderTimeout = 15 * time.Second
	return &httputil.ReverseProxy{Transport: transport, FlushInterval: -1, Rewrite: func(p *httputil.ProxyRequest) {
		p.SetURL(u)
		p.Out.Host = u.Host
		p.Out.Header.Del("Authorization")
		p.Out.Header.Del("Proxy-Authorization")
		p.Out.Header.Del("Cookie")
		p.Out.Header.Del("Origin")
		p.Out.Header.Del("Forwarded")
		p.Out.Header.Del("X-Forwarded-For")
		p.Out.Header.Del("X-Forwarded-Host")
		p.Out.Header.Del("X-Forwarded-Proto")
	}, ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
		http.Error(w, "registry backend unavailable", http.StatusBadGateway)
	}}, nil
}
func (g *Gateway) credentials() (string, string, error) {
	username, err := os.ReadFile(filepath.Join(g.config.AuthDir, "username"))
	if err != nil {
		return "", "", errors.New("credentials unavailable")
	}
	password, err := os.ReadFile(filepath.Join(g.config.AuthDir, "password"))
	if err != nil || len(password) < 16 || len(username) == 0 || strings.ContainsAny(string(username), ":\r\n") {
		return "", "", errors.New("credentials invalid")
	}
	return string(username), string(password), nil
}
func equal(a, b string) int {
	x, y := sha256.Sum256([]byte(a)), sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(x[:], y[:])
}
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if r.TLS != nil {
		w.Header().Set("Strict-Transport-Security", "max-age=31536000")
	}
	user, password, err := g.credentials()
	if err != nil {
		http.Error(w, "authentication dependency unavailable", 503)
		return
	}
	suppliedUser, suppliedPassword, ok := r.BasicAuth()
	match := equal(user, suppliedUser) & equal(password, suppliedPassword)
	if !ok || match != 1 {
		w.Header().Set("WWW-Authenticate", `Basic realm="AgentRegistry", charset="UTF-8"`)
		http.Error(w, "authentication required", 401)
		return
	}
	// Browser credentials must not authorize requests from another origin.
	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		if err != nil || u.Host != r.Host || u.Scheme != scheme || u.User != nil {
			http.Error(w, "cross-origin request denied", 403)
			return
		}
	}
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		http.Error(w, "cross-site request denied", 403)
		return
	}
	cleaned := path.Clean(r.URL.Path)
	if cleaned != strings.TrimSuffix(r.URL.Path, "/") && !(r.URL.Path == "/" && cleaned == "/") {
		http.Error(w, "noncanonical path denied", 400)
		return
	}
	if r.URL.Path == "/mcp" || r.URL.Path == "/mcp/" {
		if !allowMCP(w, r) {
			return
		}
		g.mcpProxy.ServeHTTP(w, r)
		return
	}
	if strings.HasPrefix(cleaned, "/v0/") {
		resource := strings.Split(strings.TrimPrefix(cleaned, "/v0/"), "/")[0]
		switch resource {
		case "servers", "agents", "skills", "prompts", "health", "version":
		default:
			http.Error(w, "endpoint outside registry catalog scope", 403)
			return
		}
	}
	if !strings.HasPrefix(cleaned, "/v0/") && r.Method != "GET" && r.Method != "HEAD" {
		http.Error(w, "method outside registry catalog scope", 405)
		return
	}
	g.httpProxy.ServeHTTP(w, r)
}

var catalogTools = map[string]bool{"list_agents": true, "get_agent": true, "list_servers": true, "get_server": true, "get_server_readme": true, "list_skills": true, "get_skill": true, "registry_health": true, "registry_version": true}

func allowMCP(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == "GET" || r.Method == "DELETE" {
		return true
	}
	if r.Method != "POST" {
		http.Error(w, "MCP method denied", 405)
		return false
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		http.Error(w, "MCP body too large", 413)
		return false
	}
	// Upstream's MCP SDK decodes field names case-sensitively. Struct decoding
	// here would accept aliases such as Method/Name and authorize a different
	// operation from the one the SDK executes. Exact map lookups and canonical
	// serialization also ensure duplicate JSON members cannot differ downstream.
	var rpc map[string]any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if !json.Valid(body) || decoder.Decode(&rpc) != nil || rpc["jsonrpc"] != "2.0" {
		http.Error(w, "invalid MCP JSON-RPC object", 400)
		return false
	}
	method, _ := rpc["method"].(string)
	params, _ := rpc["params"].(map[string]any)
	name, _ := params["name"].(string)
	allowed := false
	switch method {
	case "initialize", "ping", "notifications/initialized", "notifications/cancelled", "tools/list", "prompts/list":
		allowed = true
	case "tools/call":
		allowed = catalogTools[name]
	case "prompts/get":
		allowed = name == "search_registry" || name == "registry_overview"
	}
	if !allowed {
		http.Error(w, "MCP operation outside registry catalog scope", 403)
		return false
	}
	body, err = json.Marshal(rpc)
	if err != nil {
		http.Error(w, "invalid MCP JSON-RPC object", 400)
		return false
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	return true
}

// Ready is served only on the pod health port, never through the public Service.
// An upstream TCP socket or /v0/health cannot reveal database outages.
func (g *Gateway) Ready(w http.ResponseWriter, r *http.Request) {
	if err := servingCertificateReady(); err != nil {
		http.Error(w, "TLS certificate invalid, unsuitable for server authentication, expired or not yet valid", http.StatusServiceUnavailable)
		return
	}
	if _, _, err := g.credentials(); err != nil {
		http.Error(w, "credentials unavailable", 503)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	// In pinned upstream 0.3.2/0.3.3, search performs a PostgreSQL ILIKE on
	// server_name. Accepted names contain exactly one slash, so "//" returns
	// an empty page while still detecting database failures. Loading real
	// records here makes readiness depend on unbounded catalog payload sizes.
	req, _ := http.NewRequestWithContext(ctx, "GET", g.config.HTTPBackend+"/v0/servers?limit=1&search=%2F%2F", nil)
	resp, err := g.client.Do(req)
	if err != nil {
		http.Error(w, "registry database unavailable", 503)
		return
	}
	var catalog struct {
		Servers json.RawMessage `json:"servers"`
	}
	err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&catalog)
	resp.Body.Close()
	if resp.StatusCode != 200 || err != nil || len(catalog.Servers) == 0 || catalog.Servers[0] != '[' {
		http.Error(w, "registry database query failed", 503)
		return
	}
	if err := g.mcpReady(ctx); err != nil {
		http.Error(w, "registry MCP unavailable", 503)
		return
	}
	w.WriteHeader(200)
}
func (g *Gateway) mcpReady(ctx context.Context) error {
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"operator-readiness","version":"1"}}}`
	req, _ := http.NewRequestWithContext(ctx, "POST", g.config.MCPBackend+"/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := g.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if id := resp.Header.Get("Mcp-Session-Id"); id != "" {
		defer func() {
			cleanup, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			del, _ := http.NewRequestWithContext(cleanup, "DELETE", g.config.MCPBackend+"/mcp", nil)
			del.Header.Set("Mcp-Session-Id", id)
			del.Header.Set("MCP-Protocol-Version", "2025-06-18")
			if result, e := g.client.Do(del); e == nil {
				result.Body.Close()
			}
		}()
	}
	if resp.StatusCode != 200 {
		return errors.New("MCP initialize failed")
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 65536))
	if err != nil {
		return err
	}
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		var frames []byte
		for _, line := range bytes.Split(data, []byte("\n")) {
			if bytes.HasPrefix(line, []byte("data:")) {
				frames = append(frames, bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))...)
				frames = append(frames, '\n')
			}
		}
		data = frames
	}
	var reply struct {
		Result struct {
			ProtocolVersion string         `json:"protocolVersion"`
			ServerInfo      map[string]any `json:"serverInfo"`
		} `json:"result"`
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(data, &reply) != nil || reply.Result.ProtocolVersion == "" || len(reply.Result.ServerInfo) == 0 || len(reply.Error) > 0 {
		return errors.New("invalid MCP initialization response")
	}
	return nil
}

// Only the health process consults time. Reconciliation remains deterministic.
func servingCertificateReady() error {
	certFile, keyFile := os.Getenv("TLS_CERT_FILE"), os.Getenv("TLS_KEY_FILE")
	if certFile == "" && keyFile == "" {
		return nil
	}
	pair, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return err
	}
	cert, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return err
	}
	if err := certificates.ValidateLeaf(cert); err != nil {
		return err
	}
	now := time.Now()
	if now.Before(cert.NotBefore) || !now.Before(cert.NotAfter) {
		return errors.New("certificate outside validity period")
	}
	return nil
}
