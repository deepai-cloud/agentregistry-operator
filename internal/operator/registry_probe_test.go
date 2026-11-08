package operator

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func registryContainer(t *testing.T, version string) Object {
	t.Helper()
	req := fixture()
	req.Parent.Spec.Version = version
	result, err := Sync(req, config())
	if err != nil {
		t.Fatal(err)
	}
	deployment := asMap(t, find(t, result, "Deployment"))
	pod := object(object(object(deployment["spec"])["template"])["spec"])
	for _, container := range pod["containers"].([]any) {
		if object(container)["name"] == "registry" {
			return object(container)
		}
	}
	t.Fatal("missing registry container")
	return nil
}

func TestRegistryProbesUseLocalHealthWithStartupGrace(t *testing.T) {
	for _, version := range []string{"0.3.2", "0.3.3"} {
		t.Run(version, func(t *testing.T) {
			container := registryContainer(t, version)
			startup, liveness := object(container["startupProbe"]), object(container["livenessProbe"])
			if startup == nil || liveness == nil {
				t.Fatal("registry requires startup and liveness probes to recover from a stalled HTTP server")
			}
			if startup["httpGet"] != nil || liveness["httpGet"] != nil {
				t.Fatal("a node HTTP probe cannot reach the registry's pod-loopback listener")
			}
			if !reflect.DeepEqual(startup["exec"], liveness["exec"]) {
				t.Fatal("startup must test the same application endpoint as liveness")
			}
			if number(startup["periodSeconds"])*number(startup["failureThreshold"]) != 600 || number(startup["timeoutSeconds"]) != 3 {
				t.Fatal("startup must allow ten minutes for initial database setup and migrations")
			}
			if number(liveness["periodSeconds"]) != 10 || number(liveness["failureThreshold"]) != 3 || number(liveness["timeoutSeconds"]) != 3 {
				t.Fatal("liveness must tolerate three failed ten-second checks, each bounded to three seconds")
			}
			command := object(liveness["exec"])["command"].([]any)
			if command[0] != "/usr/bin/curl" || command[len(command)-1] != "http://127.0.0.1:8080/v0/health" {
				t.Fatal("liveness must use the pinned image's curl and dependency-independent loopback health endpoint")
			}
		})
	}
}

func TestRegistryProbeCommandHandlesHealthFailures(t *testing.T) {
	container := registryContainer(t, "0.3.3")
	command, ok := object(object(container["livenessProbe"])["exec"])["command"].([]any)
	if !ok {
		t.Fatal("registry liveness command missing")
	}
	if _, err := os.Stat(command[0].(string)); err != nil {
		t.Skip("curl is required to exercise the pinned image's probe command")
	}
	config := t.TempDir()
	if err := os.WriteFile(filepath.Join(config, ".curlrc"), []byte("url = http://127.0.0.1:1/config-must-not-be-loaded\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"healthy", "http-failure", "stalled"} {
		t.Run(state, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/v0/health" {
					t.Error("liveness must not query the catalog or MCP dependencies")
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				switch state {
				case "stalled":
					<-r.Context().Done()
				case "http-failure":
					w.WriteHeader(http.StatusServiceUnavailable)
				default:
					// The pinned health endpoint is static even when database queries fail.
					io.WriteString(w, `{"status":"ok"}`)
				}
			}))
			defer up.Close()
			args := make([]string, len(command)-1)
			for i, arg := range command[1:] {
				args[i] = strings.Replace(arg.(string), "http://127.0.0.1:8080", up.URL, 1)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			probe := exec.CommandContext(ctx, command[0].(string), args...)
			probe.Env = append(os.Environ(), "CURL_HOME="+config, "http_proxy=http://127.0.0.1:1", "no_proxy=")
			output, err := probe.CombinedOutput()
			wantHealthy := state == "healthy"
			if (err == nil) != wantHealthy || ctx.Err() != nil {
				t.Fatalf("probe health=%v, expected=%v; deadline=%v; output=%s", err == nil, wantHealthy, ctx.Err(), output)
			}
		})
	}
}
