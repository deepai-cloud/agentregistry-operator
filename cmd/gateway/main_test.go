package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// Run the real main in a subprocess so these tests check the process exit status.
func TestGatewayProcess(t *testing.T) {
	if os.Getenv("AGENTREGISTRY_GATEWAY_TEST_PROCESS") != "1" {
		return
	}
	main()
	os.Exit(0)
}

func TestListenerFailureExitsNonzero(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  []string
	}{
		{name: "public HTTP", env: []string{"LISTEN_ADDRESS=127.0.0.1:invalid-port"}},
		{name: "public HTTPS", env: []string{"LISTEN_ADDRESS=127.0.0.1:invalid-port", "TLS_CERT_FILE=/unused/cert", "TLS_KEY_FILE=/unused/key"}},
		{name: "health", env: []string{"HEALTH_ADDRESS=127.0.0.1:invalid-port"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			command := gatewayProcess(t, ctx)
			command.Env = append(command.Env, tc.env...)
			output, err := command.CombinedOutput()
			if ctx.Err() != nil {
				t.Fatalf("listener failure did not terminate the gateway: %s", output)
			}
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 1 {
				t.Fatalf("want failure exit status 1, got %v: %s", err, output)
			}
			if !strings.Contains(string(output), "gateway listener failed:") {
				t.Fatalf("listener failure was not reported: %s", output)
			}
		})
	}
}

func gatewayProcess(t *testing.T, ctx context.Context) *exec.Cmd {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(ctx, executable, "-test.run=^TestGatewayProcess$")
	command.Env = append(os.Environ(),
		"AGENTREGISTRY_GATEWAY_TEST_PROCESS=1", "AUTH_DIR="+t.TempDir(),
		"LISTEN_ADDRESS=127.0.0.1:0", "HEALTH_ADDRESS=127.0.0.1:0",
		"TLS_CERT_FILE=", "TLS_KEY_FILE=",
	)
	return command
}
