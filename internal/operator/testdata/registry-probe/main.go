// This test helper renders the real registry probes or serves controlled health
// responses inside a pinned registry image. It is not part of either binary.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"

	"github.com/deepai-cloud/agentregistry-operator/internal/operator"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "render" {
		var checks []map[string]any
		for _, version := range []string{"0.3.2", "0.3.3"} {
			parent := operator.Parent{
				APIVersion: "registry.deepai.cloud/v1alpha1", Kind: "AgentRegistry",
				Metadata: operator.Metadata{Name: "catalog", Namespace: "tenant-a", UID: "probe-test", Generation: 1},
				Spec: operator.Spec{Version: version, AuthSecretName: "auth", Database: operator.Database{
					Host: "postgres.tenant-a.svc.cluster.local", SecretName: "database",
				}},
			}
			result, err := operator.Sync(operator.Request{Parent: parent}, operator.Config{GatewayImage: "probe:test"})
			if err != nil {
				panic(err)
			}
			for _, child := range result.Children {
				if child["kind"] != "Deployment" {
					continue
				}
				pod := child["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
				for _, container := range pod["containers"].([]operator.Object) {
					if container["name"] == "registry" {
						probe := container["livenessProbe"].(map[string]any)["exec"].(map[string]any)
						checks = append(checks, map[string]any{"version": version, "image": container["image"], "command": probe["command"]})
					}
				}
			}
		}
		if len(checks) != 2 {
			panic("expected probes for both supported registry versions")
		}
		if err := json.NewEncoder(os.Stdout).Encode(checks); err != nil {
			panic(err)
		}
		return
	}
	server := &http.Server{Addr: "127.0.0.1:8080", Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/v0/health" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		state, err := os.ReadFile("/probe/state")
		if err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		switch string(state) {
		case "healthy":
			fmt.Fprint(w, `{"status":"ok"}`)
		case "stalled":
			<-r.Context().Done()
		default:
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	})}
	if err := server.ListenAndServe(); err != nil {
		panic(err)
	}
}
