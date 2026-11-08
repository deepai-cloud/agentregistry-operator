package gateway

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMCPDiscoveryMatchesAllowedCalls(t *testing.T) {
	for _, tc := range []struct {
		method, field string
		allowed       map[string]bool
		blocked       []string
	}{
		{"tools/list", "tools", catalogTools, []string{"list_deployments", "get_deployment", "deploy_server", "deploy_agent", "remove_deployment", "future_tool"}},
		{"prompts/list", "prompts", catalogPrompts, []string{"deploy_mcp_server", "future_prompt"}},
	} {
		t.Run(tc.method, func(t *testing.T) {
			entries := []map[string]any{}
			for name := range tc.allowed {
				entries = append(entries, map[string]any{"name": name, "description": "Keep this description", "inputSchema": map[string]any{"type": "object"}, "_meta": map[string]any{"version": 7}})
			}
			for _, name := range tc.blocked {
				entries = append(entries, map[string]any{"name": name})
			}
			result, _ := json.Marshal(map[string]any{tc.field: entries, "nextCursor": "next-page", "_meta": map[string]any{"revision": 1}})
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Accept-Encoding") != "identity" {
					t.Error("discovery allowed a compressed upstream response")
				}
				if r.Header.Get("Mcp-Session-Id") != "session-a" || r.Header.Get("MCP-Protocol-Version") != "2025-06-18" {
					t.Error("session or protocol header lost")
				}
				var request map[string]json.RawMessage
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
				}
				if string(request["params"]) != `{"cursor":"previous-page"}` {
					t.Errorf("pagination request changed: %s", request["params"])
				}
				body := fmt.Sprintf(`{"jsonrpc":"2.0","id":9007199254740993,"result":%s}`, result)
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				w.Header().Set("Content-Length", fmt.Sprint(len(body)))
				w.Header().Set("Mcp-Session-Id", "session-a")
				w.Header().Set("ETag", "upstream-content")
				w.Header().Set("Content-Digest", "upstream-digest")
				io.WriteString(w, body)
			}))
			defer up.Close()
			g, err := New(Config{AuthDir: credentials(t), MCPBackend: up.URL})
			if err != nil {
				t.Fatal(err)
			}
			front := httptest.NewServer(g)
			defer front.Close()
			r, _ := http.NewRequest("POST", front.URL+"/mcp", strings.NewReader(fmt.Sprintf(`{"jsonrpc":"2.0","id":9007199254740993,"method":%q,"params":{"cursor":"previous-page"}}`, tc.method)))
			r.SetBasicAuth("alice", "tenant-password-long-enough")
			r.Header.Set("Accept-Encoding", "gzip")
			r.Header.Set("Mcp-Session-Id", "session-a")
			r.Header.Set("MCP-Protocol-Version", "2025-06-18")
			resp, err := front.Client().Do(r)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			data, err := io.ReadAll(resp.Body)
			if err != nil || resp.StatusCode != 200 || resp.ContentLength != int64(len(data)) {
				t.Fatalf("invalid response framing: status=%d length=%d read=%d error=%v", resp.StatusCode, resp.ContentLength, len(data), err)
			}
			if resp.Header.Get("ETag") != "" || resp.Header.Get("Content-Digest") != "" || resp.Header.Get("Mcp-Session-Id") != "session-a" {
				t.Fatalf("incorrect response headers: %v", resp.Header)
			}
			var rpc struct {
				ID     json.RawMessage            `json:"id"`
				Result map[string]json.RawMessage `json:"result"`
			}
			if err := json.Unmarshal(data, &rpc); err != nil {
				t.Fatal(err)
			}
			if string(rpc.ID) != "9007199254740993" || string(rpc.Result["nextCursor"]) != `"next-page"` || string(rpc.Result["_meta"]) != `{"revision":1}` {
				t.Fatalf("response identity or pagination changed: %s", data)
			}
			want, _ := json.Marshal(entries[:len(tc.allowed)])
			if string(rpc.Result[tc.field]) != string(want) {
				t.Fatalf("discovery does not match allowed calls or lost schemas: %s", data)
			}
		})
	}
}

func TestMCPDiscoveryStreamsAndReplaysWithoutWaitingForEOF(t *testing.T) {
	for _, requestMethod := range []string{"POST", "GET"} {
		for _, newline := range []string{"\n", "\r\n", "\r"} {
			t.Run(fmt.Sprintf("%s/%q", requestMethod, newline), func(t *testing.T) {
				closed := make(chan struct{})
				up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					defer close(closed)
					if requestMethod == "GET" && r.Header.Get("Last-Event-ID") != "before-list" {
						t.Error("replay cursor lost")
					}
					w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
					stream := "\xef\xbb\xbf: keepalive\n\n" +
						"event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/tools/list_changed\"}\n\n" +
						"id: after-list\nretry: 500\nevent: message\n" +
						"data: {\"jsonrpc\":\"2.0\",\"id\":\"list-request\",\"result\":{\n" +
						"data: \"tools\":[{\"name\":\"deploy_server\"},{\"name\":\"list_servers\"}],\"nextCursor\":\"page-two\"}}\n\n"
					io.WriteString(w, strings.ReplaceAll(stream, "\n", newline))
					w.(http.Flusher).Flush()
					<-r.Context().Done()
				}))
				defer up.Close()
				g, err := New(Config{AuthDir: credentials(t), MCPBackend: up.URL})
				if err != nil {
					t.Fatal(err)
				}
				front := httptest.NewServer(g)
				defer front.Close()
				r, _ := http.NewRequest(requestMethod, front.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":"list-request","method":"tools/list"}`))
				r.SetBasicAuth("alice", "tenant-password-long-enough")
				r.Header.Set("Last-Event-ID", "before-list")
				client := &http.Client{Timeout: 3 * time.Second}
				resp, err := client.Do(r)
				if err != nil {
					t.Fatal(err)
				}
				defer resp.Body.Close()
				reader := bufio.NewReader(resp.Body)
				var got strings.Builder
				for events := 0; events < 3; {
					line, err := reader.ReadString('\n')
					if err != nil {
						t.Fatalf("stream did not flush complete events: %v", err)
					}
					got.WriteString(line)
					if line == "\n" {
						events++
					}
				}
				body := got.String()
				for _, kept := range []string{": keepalive", "notifications/tools/list_changed", "id: after-list", "retry: 500", "event: message", `"name":"list_servers"`, `"nextCursor":"page-two"`, `"id":"list-request"`} {
					if !strings.Contains(body, kept) {
						t.Errorf("stream lost %q: %s", kept, body)
					}
				}
				if strings.Contains(body, "deploy_server") {
					t.Fatalf("blocked tool advertised: %s", body)
				}
				resp.Body.Close()
				select {
				case <-closed:
				case <-time.After(time.Second):
					t.Fatal("disconnect leaked upstream stream")
				}
			})
		}
	}
}

func TestMCPCatalogMessagePreservesErrorsAndOtherContent(t *testing.T) {
	for _, message := range []string{
		`{"jsonrpc":"2.0","id":1,"error":{"code":-32602,"message":"bad cursor"}}`,
		`{"jsonrpc":"2.0","method":"notifications/progress","params":{"progress":1}}`,
		`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"deploy_server documentation"}]}}`,
		`{"jsonrpc":"2.0","id":1,"result":{"messages":[{"role":"user","content":{"type":"text","text":"Search for list_deployments examples"}}]}}`,
	} {
		got, err := filterCatalogMessage([]byte(message))
		if err != nil || string(got) != message {
			t.Errorf("unrelated message changed: %s, %v", got, err)
		}
	}
	got, err := filterCatalogMessage([]byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"deploy_server"}],"nextCursor":"more"}}`))
	if err != nil || !strings.Contains(string(got), `"tools":[]`) || !strings.Contains(string(got), `"nextCursor":"more"`) {
		t.Fatalf("filtered empty page must remain an array with its cursor: %s %v", got, err)
	}
}

func TestMCPRegistryOverviewStaysWithinCatalogScope(t *testing.T) {
	body := `{"jsonrpc":"2.0","id":1,"result":{"description":"Overview of registry contents","messages":[{"role":"user","content":{"type":"text","text":"Give me an overview of what's available in the agent registry. Use list_servers, list_agents, and list_skills to see what's published. Also check list_deployments to see what's currently deployed. Summarize the results in a clear table format showing name, description, and latest version for each resource type."}}]}}`
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, body)
	}))
	defer up.Close()
	g, err := New(Config{AuthDir: credentials(t), MCPBackend: up.URL})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"prompts/get","params":{"name":"registry_overview"}}`))
	r.SetBasicAuth("alice", "tenant-password-long-enough")
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)
	if w.Code != 200 || strings.Contains(w.Body.String(), "list_deployments") || !strings.Contains(w.Body.String(), "list_servers, list_agents, and list_skills") {
		t.Fatalf("overview recommends blocked operations: %d %s", w.Code, w.Body.String())
	}
}

func TestMCPNonDiscoveryResponsesPassThrough(t *testing.T) {
	for _, tc := range []struct {
		method, rpc string
		status      int
	}{
		{"POST", `{"jsonrpc":"2.0","id":1,"method":"initialize"}`, 200},
		{"POST", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_server_readme"}}`, 200},
		{"POST", `{"jsonrpc":"2.0","id":1,"method":"prompts/get","params":{"name":"search_registry","arguments":{"query":"deploy_server"}}}`, 200},
		{"POST", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, 404},
		{"GET", "", 405},
		{"DELETE", "", 200},
	} {
		t.Run(tc.method+"/"+tc.rpc, func(t *testing.T) {
			body := strings.Repeat("x", catalogResponseLimit+1)
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				io.WriteString(w, body)
			}))
			defer up.Close()
			g, err := New(Config{AuthDir: credentials(t), MCPBackend: up.URL})
			if err != nil {
				t.Fatal(err)
			}
			r := httptest.NewRequest(tc.method, "/mcp", strings.NewReader(tc.rpc))
			r.SetBasicAuth("alice", "tenant-password-long-enough")
			w := httptest.NewRecorder()
			g.ServeHTTP(w, r)
			if w.Code != tc.status || w.Body.String() != body {
				t.Fatalf("non-discovery response changed: status=%d bytes=%d", w.Code, w.Body.Len())
			}
		})
	}
}

func TestMCPDiscoveryRejectsInvalidResponses(t *testing.T) {
	for _, tc := range []struct{ name, contentType, encoding, body string }{
		{"malformed JSON", "application/json", "", `{`},
		{"non-object JSON", "application/json", "", `null`},
		{"invalid result", "application/json", "", `{"result":[]}`},
		{"invalid list", "application/json", "", `{"result":{"tools":null}}`},
		{"invalid entry", "application/json", "", `{"result":{"tools":[1]}}`},
		{"oversized JSON", "application/json", "", strings.Repeat(" ", catalogResponseLimit+1)},
		{"unexpected encoding", "application/json", "gzip", "compressed"},
		{"unexpected content", "text/html", "", "backend unavailable"},
		{"invalid media type", "invalid;", "", "backend unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tc.contentType)
				w.Header().Set("Content-Encoding", tc.encoding)
				io.WriteString(w, tc.body)
			}))
			defer up.Close()
			g, err := New(Config{AuthDir: credentials(t), MCPBackend: up.URL})
			if err != nil {
				t.Fatal(err)
			}
			r := httptest.NewRequest("POST", "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
			r.SetBasicAuth("alice", "tenant-password-long-enough")
			w := httptest.NewRecorder()
			g.ServeHTTP(w, r)
			if w.Code != http.StatusBadGateway {
				t.Fatalf("invalid discovery leaked: %d %s", w.Code, w.Body.String())
			}
		})
	}
	for _, stream := range []string{"data: {\n\n", "data: {}", ": " + strings.Repeat("x", catalogResponseLimit)} {
		r := io.NopCloser(strings.NewReader(stream))
		s := &catalogEventStream{ReadCloser: r, reader: bufio.NewReader(r), firstLine: true}
		if got, err := io.ReadAll(s); err == nil || len(got) != 0 {
			t.Errorf("invalid/oversized event was forwarded: bytes=%d, error=%v", len(got), err)
		}
	}
}

func TestMCPDiscoveryStreamKeepsEmptyEventsAndDropsStaleTrailers(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Trailer", "Content-Digest")
		io.WriteString(w, "id: start\ndata:\n\nevent: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"prompts\":[{\"name\":\"deploy_mcp_server\"}]}}\n\n")
		w.Header().Set("Content-Digest", "upstream-bytes")
	}))
	defer up.Close()
	g, err := New(Config{AuthDir: credentials(t), MCPBackend: up.URL})
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(g)
	defer front.Close()
	r, _ := http.NewRequest("POST", front.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"prompts/list"}`))
	r.SetBasicAuth("alice", "tenant-password-long-enough")
	resp, err := front.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != 200 || !strings.Contains(string(body), "id: start\ndata:\n\n") || !strings.Contains(string(body), `"prompts":[]`) {
		t.Fatalf("stream lost empty event or leaked blocked prompt: %d %s %v", resp.StatusCode, body, err)
	}
	if resp.Trailer.Get("Content-Digest") != "" {
		t.Fatal("modified stream carries the upstream content digest")
	}
}
