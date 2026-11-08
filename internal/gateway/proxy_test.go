package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestProxyKeepsClientCredentialsAtGateway(t *testing.T) {
	for _, useTLS := range []bool{false, true} {
		scheme := "http"
		if useTLS {
			scheme = "https"
		}
		t.Run(scheme, func(t *testing.T) {
			type observation struct {
				header                  http.Header
				method, uri, host, body string
			}
			observed := make(chan observation, 1)
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				select {
				case observed <- observation{r.Header.Clone(), r.Method, r.RequestURI, r.Host, string(body)}:
				default:
					t.Error("backend received an unexpected additional request")
				}
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Mcp-Session-Id", "backend-session")
				io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
			}))
			defer up.Close()
			g, err := New(Config{AuthDir: credentials(t), HTTPBackend: up.URL, MCPBackend: up.URL})
			if err != nil {
				t.Fatal(err)
			}
			front := httptest.NewUnstartedServer(g)
			if useTLS {
				front.StartTLS()
			} else {
				front.Start()
			}
			defer front.Close()
			client := front.Client()
			client.Timeout = 3 * time.Second
			for _, tc := range []struct{ method, path, body string }{
				{"GET", "/ui/", ""},
				{"POST", "/v0/servers?version=1", `{"name":"example/catalog"}`},
				{"POST", "/mcp", `{"id":1,"jsonrpc":"2.0","method":"ping"}`},
				{"DELETE", "/mcp", ""},
			} {
				t.Run(tc.method+tc.path, func(t *testing.T) {
					r, err := http.NewRequest(tc.method, front.URL+tc.path, strings.NewReader(tc.body))
					if err != nil {
						t.Fatal(err)
					}
					r.Host = "catalog.example.test"
					r.SetBasicAuth("alice", "tenant-password-long-enough")
					privateHeaders := []string{"Proxy-Authorization", "Cookie", "Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto"}
					for _, name := range privateHeaders {
						r.Header.Add(name, "private-client-value")
						r.Header.Add(name, "another-private-client-value")
					}
					r.Header.Set("Origin", scheme+"://"+r.Host)
					r.Header.Set("Sec-Fetch-Site", "same-origin")
					protocolHeaders := map[string]string{
						"Content-Type": "application/json", "Accept": "application/json, text/event-stream",
						"Mcp-Session-Id": "client-session", "MCP-Protocol-Version": "2025-06-18", "Last-Event-ID": "event-7",
					}
					for name, value := range protocolHeaders {
						r.Header.Set(name, value)
					}
					resp, err := client.Do(r)
					if err != nil {
						t.Fatal(err)
					}
					body, err := io.ReadAll(resp.Body)
					resp.Body.Close()
					if err != nil || resp.StatusCode != http.StatusOK || string(body) != `{"jsonrpc":"2.0","id":1,"result":{}}` || resp.Header.Get("Mcp-Session-Id") != "backend-session" {
						t.Fatalf("authenticated proxy request failed: status=%d body=%s error=%v", resp.StatusCode, body, err)
					}
					select {
					case got := <-observed:
						for _, name := range append(privateHeaders, "Authorization", "Origin") {
							if len(got.header.Values(name)) != 0 {
								t.Errorf("backend received private client header %s", name)
							}
						}
						for name, value := range protocolHeaders {
							if got.header.Get(name) != value {
								t.Errorf("proxy lost protocol header %s", name)
							}
						}
						if got.host != strings.TrimPrefix(up.URL, "http://") || got.method != tc.method || got.uri != tc.path || got.body != tc.body {
							t.Errorf("proxy changed routing or request content: %+v", got)
						}
					default:
						t.Fatal("successful response did not come from the backend")
					}
				})
			}
		})
	}
}

func TestBrowserOriginBoundaryBeforeProxying(t *testing.T) {
	for _, useTLS := range []bool{false, true} {
		scheme, otherScheme := "http", "https"
		if useTLS {
			scheme, otherScheme = otherScheme, scheme
		}
		t.Run(scheme, func(t *testing.T) {
			var calls atomic.Int32
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(http.StatusNoContent)
			}))
			defer up.Close()
			g, err := New(Config{AuthDir: credentials(t), HTTPBackend: up.URL, MCPBackend: up.URL})
			if err != nil {
				t.Fatal(err)
			}
			front := httptest.NewUnstartedServer(g)
			if useTLS {
				front.StartTLS()
			} else {
				front.Start()
			}
			defer front.Close()
			client := front.Client()
			client.Timeout = 3 * time.Second
			origin := scheme + "://catalog.example.test"
			for _, endpoint := range []struct{ path, body string }{
				{"/v0/servers", `{"name":"example/catalog"}`},
				{"/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_servers"}}`},
			} {
				for _, tc := range []struct {
					name, origin, site string
					allowed            bool
				}{
					{"non-browser", "", "", true},
					{"same-origin", origin, "same-origin", true},
					{"other-host", scheme + "://other.example.test", "same-site", false},
					{"other-port", origin + ":9443", "same-site", false},
					{"other-scheme", otherScheme + "://catalog.example.test", "same-site", false},
					{"opaque-origin", "null", "", false},
					{"userinfo", scheme + "://alice@catalog.example.test", "", false},
					{"cross-site-without-origin", "", "cross-site", false},
					{"cross-site-with-matching-origin", origin, "cross-site", false},
				} {
					t.Run(endpoint.path+"/"+tc.name, func(t *testing.T) {
						r, err := http.NewRequest("POST", front.URL+endpoint.path, strings.NewReader(endpoint.body))
						if err != nil {
							t.Fatal(err)
						}
						r.Host = "catalog.example.test"
						r.SetBasicAuth("alice", "tenant-password-long-enough")
						r.Header.Set("Origin", tc.origin)
						r.Header.Set("Sec-Fetch-Site", tc.site)
						before := calls.Load()
						resp, err := client.Do(r)
						if err != nil {
							t.Fatal(err)
						}
						resp.Body.Close()
						wantStatus, wantCalls := http.StatusForbidden, int32(0)
						if tc.allowed {
							wantStatus, wantCalls = http.StatusNoContent, 1
						}
						if resp.StatusCode != wantStatus || calls.Load()-before != wantCalls {
							t.Fatalf("origin boundary: status=%d backend calls=%d, want status=%d calls=%d", resp.StatusCode, calls.Load()-before, wantStatus, wantCalls)
						}
					})
				}
			}
		})
	}
}
