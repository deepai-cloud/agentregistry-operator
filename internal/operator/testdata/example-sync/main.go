// Reconcile example manifests offline, without starting a server or contacting
// Kubernetes. This helper is excluded from the production binaries and image.
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/deepai-cloud/agentregistry-operator/internal/operator"
)

func main() {
	var request operator.Request
	if err := json.NewDecoder(os.Stdin).Decode(&request); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	response, err := operator.Sync(request, operator.Config{GatewayImage: "example-check:test"})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := json.NewEncoder(os.Stdout).Encode(response); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
