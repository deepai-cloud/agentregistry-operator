package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const initializeReply = `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18","capabilities":{},"serverInfo":{"name":"registry","version":"0.3.3"}}}`

func TestReadinessMCPResponseContentType(t *testing.T) {
	const stream = "data: " + initializeReply + "\n\n"
	for _, tc := range []struct {
		name         string
		contentTypes []string
		body         string
		wantStatus   int
	}{
		{"parameterized JSON", []string{"Application/JSON; charset=utf-8"}, initializeReply, http.StatusOK},
		{"mixed case event stream", []string{"Text/Event-Stream; Charset=utf-8"}, stream, http.StatusOK},
		{"missing type", nil, initializeReply, http.StatusServiceUnavailable},
		{"plain text JSON", []string{"text/plain"}, initializeReply, http.StatusServiceUnavailable},
		{"event stream prefix", []string{"text/event-stream-extra"}, stream, http.StatusServiceUnavailable},
		{"malformed parameters", []string{"application/json; charset"}, initializeReply, http.StatusServiceUnavailable},
		{"conflicting types", []string{"application/json", "text/event-stream"}, initializeReply, http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var deleted atomic.Bool
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v0/servers" {
					io.WriteString(w, `{"servers":[]}`)
					return
				}
				if r.Method == http.MethodDelete {
					deleted.Store(r.Header.Get("Mcp-Session-Id") == "probe-session")
					return
				}
				// An explicitly nil header suppresses net/http's Content-Type sniffing.
				w.Header()["Content-Type"] = tc.contentTypes
				w.Header().Set("Mcp-Session-Id", "probe-session")
				io.WriteString(w, tc.body)
			}))
			defer up.Close()
			g, err := New(Config{AuthDir: credentials(t), HTTPBackend: up.URL, MCPBackend: up.URL})
			if err != nil {
				t.Fatal(err)
			}
			w := httptest.NewRecorder()
			g.Ready(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
			if w.Code != tc.wantStatus || !deleted.Load() {
				t.Fatalf("readiness HTTP %d, want %d; session deleted=%t", w.Code, tc.wantStatus, deleted.Load())
			}
		})
	}
}

func TestMCPReadinessAcceptsStreamedInitialization(t *testing.T) {
	for _, tc := range []struct {
		name, stream string
		keepOpen     bool
	}{
		{"open-stream", "data: " + initializeReply + "\n\n", true},
		{"empty-event-before-reply", "id: start\ndata:\n\ndata: " + initializeReply + "\n\n", true},
		{"keepalive-before-reply", "event: keepalive\ndata: ping\n\ndata: " + initializeReply + "\n\n", true},
		{"event-name-without-data", "event: keepalive\n\ndata: " + initializeReply + "\n\n", false},
		{"notification-before-reply", "data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/message\",\"params\":{\"level\":\"info\",\"data\":\"ready\"}}\n\n" + "data: " + initializeReply + "\n\n", false},
		{"unrelated-reply", "data: {\"jsonrpc\":\"2.0\",\"id\":2,\"result\":{}}\n\ndata: " + initializeReply + "\n\n", false},
		{"multiline-crlf", ": keepalive\r\n\r\nevent: message\r\nid: event-1\r\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\r\ndata: \"result\":{\"protocolVersion\":\"2025-06-18\",\"serverInfo\":{\"name\":\"registry\"}}}\r\n\r\n", false},
		{"bare-cr", "data: " + initializeReply + "\r\r", true},
		{"utf8-bom", "\xef\xbb\xbfdata: " + initializeReply + "\n\n", false},
		{"negotiated-version", "data: " + strings.Replace(initializeReply, "2025-06-18", "2025-03-26", 1) + "\n\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var deleted atomic.Bool
			streamClosed := make(chan struct{})
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodDelete {
					if r.Header.Get("Mcp-Session-Id") != "probe-session" {
						t.Error("cleanup used the wrong session")
					}
					version := "2025-06-18"
					if tc.name == "negotiated-version" {
						version = "2025-03-26"
					}
					if r.Header.Get("MCP-Protocol-Version") != version {
						t.Error("cleanup must use the negotiated protocol version")
					}
					if tc.keepOpen {
						select {
						case <-streamClosed:
						case <-r.Context().Done():
							t.Error("cleanup waited on a stream that the probe left open")
							return
						}
					}
					deleted.Store(true)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
				w.Header().Set("Mcp-Session-Id", "probe-session")
				io.WriteString(w, tc.stream)
				w.(http.Flusher).Flush()
				if tc.keepOpen {
					<-r.Context().Done()
					close(streamClosed)
				}
			}))
			defer up.Close()
			g, err := New(Config{AuthDir: credentials(t), MCPBackend: up.URL})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := g.mcpReady(ctx); err != nil {
				t.Fatalf("valid streamed initialization was not ready: %v", err)
			}
			if ctx.Err() != nil {
				t.Fatal("readiness waited for the initialization stream to close")
			}
			if !deleted.Load() {
				t.Fatal("readiness leaked its MCP session")
			}
		})
	}
}

func TestMCPReadinessRejectsIncompleteStreams(t *testing.T) {
	for name, stream := range map[string]string{
		"unterminated-event":    "data: " + initializeReply + "\n", // No blank line dispatches this event.
		"notification-only":     "data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/message\"}\n\n",
		"reply-in-custom-event": "data: " + initializeReply + "\nevent: status\n\n",
		"rpc-error":             "data: {\"jsonrpc\":\"2.0\",\"id\":1,\"error\":{\"code\":-32603,\"message\":\"failed\"}}\n\n",
		"malformed-json":        "data: not-json\n\n",
		"wrong-id":              "data: " + strings.Replace(initializeReply, `"id":1`, `"id":2`, 1) + "\n\n",
		"too-large":             strings.Repeat(": keepalive\n\n", 6000) + "data: " + initializeReply + "\n\n",
	} {
		t.Run(name, func(t *testing.T) {
			var deleted atomic.Bool
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodDelete {
					deleted.Store(true)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("Mcp-Session-Id", "failed-probe-session")
				io.WriteString(w, stream)
			}))
			defer up.Close()
			g, err := New(Config{AuthDir: credentials(t), MCPBackend: up.URL})
			if err != nil {
				t.Fatal(err)
			}
			if err := g.mcpReady(context.Background()); err == nil {
				t.Error("incomplete, invalid, mismatched or oversized stream reported ready")
			}
			if !deleted.Load() {
				t.Error("failed readiness probe leaked its MCP session")
			}
		})
	}
}

func TestMCPReadinessValidatesJSONResponse(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		valid      bool
	}{
		{"valid", initializeReply, true},
		{"size-limit", initializeReply + strings.Repeat(" ", (64<<10)-len(initializeReply)), true},
		{"too-large", initializeReply + strings.Repeat(" ", (64<<10)-len(initializeReply)+1), false},
		{"wrong-id", strings.Replace(initializeReply, `"id":1`, `"id":2`, 1), false},
		{"wrong-jsonrpc", strings.Replace(initializeReply, `"jsonrpc":"2.0"`, `"jsonrpc":"1.0"`, 1), false},
		{"notification", strings.Replace(initializeReply, `"id":1`, `"method":"notifications/message"`, 1), false},
		{"trailing-json", initializeReply + `{}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, tc.body)
			}))
			defer up.Close()
			g, err := New(Config{AuthDir: credentials(t), MCPBackend: up.URL})
			if err != nil {
				t.Fatal(err)
			}
			if err := g.mcpReady(context.Background()); (err == nil) != tc.valid {
				t.Fatalf("unexpected readiness result: valid=%v, error=%v", tc.valid, err)
			}
		})
	}
}
