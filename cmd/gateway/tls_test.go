package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestTLSSecretRotationInRunningGateway(t *testing.T) {
	firstCert, firstKey := gatewayTestCertificate(t, 1)
	secondCert, secondKey := gatewayTestCertificate(t, 2)
	roots := x509.NewCertPool()
	for _, cert := range [][]byte{firstCert, secondCert} {
		if !roots.AppendCertsFromPEM(cert) {
			t.Fatal("cannot trust test certificate")
		}
	}
	directory := t.TempDir()
	for _, name := range []string{"tls.crt", "tls.key"} {
		if err := os.Symlink(filepath.Join("..data", name), filepath.Join(directory, name)); err != nil {
			t.Fatal(err)
		}
	}
	// Kubernetes projects each revision into a separate directory, then swaps
	// the ..data symlink. Keep the old revisions so caching a resolved filename
	// or a loaded certificate is caught by subsequent handshakes.
	rotate := func(t *testing.T, revision string, cert, key []byte) {
		t.Helper()
		revisionDir := filepath.Join(directory, revision)
		if err := os.Mkdir(revisionDir, 0700); err != nil {
			t.Fatal(err)
		}
		for name, data := range map[string][]byte{"tls.crt": cert, "tls.key": key} {
			if err := os.WriteFile(filepath.Join(revisionDir, name), data, 0600); err != nil {
				t.Fatal(err)
			}
		}
		pending := filepath.Join(directory, "..data_tmp")
		if err := os.Symlink(revision, pending); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(pending, filepath.Join(directory, "..data")); err != nil {
			t.Fatal(err)
		}
	}
	rotate(t, "..first", firstCert, firstKey)
	// Authentication and TLS are separate Secret volumes in the managed pod.
	authDirectory := t.TempDir()
	for name, value := range map[string]string{"username": "alice", "password": "tenant-password-long-enough"} {
		if err := os.WriteFile(filepath.Join(authDirectory, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}

	// Reserve distinct loopback ports until immediately before starting the
	// subprocess. Only health and unauthenticated HTTPS requests are used, so
	// this test never needs or contacts a registry backend.
	public, health := gatewayTestListener(t), gatewayTestListener(t)
	publicAddress, healthAddress := public.Addr().String(), health.Addr().String()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	command := gatewayProcess(t, ctx)
	command.Env = append(command.Env,
		"AUTH_DIR="+authDirectory,
		"LISTEN_ADDRESS="+publicAddress, "HEALTH_ADDRESS="+healthAddress,
		"TLS_CERT_FILE="+filepath.Join(directory, "tls.crt"),
		"TLS_KEY_FILE="+filepath.Join(directory, "tls.key"),
	)
	var logs bytes.Buffer
	command.Stdout, command.Stderr = &logs, &logs
	public.Close()
	health.Close()
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var processErr error
	go func() {
		processErr = command.Wait()
		close(done)
	}()
	t.Cleanup(func() {
		_ = command.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			cancel()
			<-done
		}
		if processErr != nil {
			t.Errorf("gateway process failed: %v", processErr)
		}
		if t.Failed() {
			t.Logf("gateway output: %s", logs.String())
		}
	})
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: "catalog.example.test", MinVersion: tls.VersionTLS12},
		// Every request must perform a fresh handshake to observe the mount.
		DisableKeepAlives: true,
	}
	t.Cleanup(transport.CloseIdleConnections)
	client := &http.Client{Transport: transport, Timeout: time.Second}
	deadline := time.Now().Add(5 * time.Second)
	for {
		ready := true
		for url, status := range map[string]int{
			"http://" + healthAddress + "/healthz": http.StatusOK,
			"https://" + publicAddress + "/":       http.StatusUnauthorized,
		} {
			response, err := client.Get(url)
			if err != nil {
				ready = false
				continue
			}
			response.Body.Close()
			ready = ready && response.StatusCode == status
		}
		if ready {
			break
		}
		select {
		case <-done:
			t.Fatalf("gateway exited before becoming live: %v", processErr)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("gateway did not become live")
		}
		time.Sleep(10 * time.Millisecond)
	}
	checkCertificate := func(t *testing.T, serial int64) {
		t.Helper()
		response, err := client.Get("https://" + publicAddress + "/")
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusUnauthorized || response.Header.Get("Strict-Transport-Security") == "" {
			t.Fatalf("HTTPS authentication boundary changed: status %d, headers %v", response.StatusCode, response.Header)
		}
		if response.TLS == nil || len(response.TLS.VerifiedChains) == 0 || response.TLS.PeerCertificates[0].SerialNumber.Int64() != serial {
			t.Fatalf("new connection did not use trusted certificate %d", serial)
		}
	}
	checkCertificate(t, 1)
	rotate(t, "..second", secondCert, secondKey)
	checkCertificate(t, 2)
	for i, invalid := range []struct {
		name      string
		cert, key []byte
	}{
		{"mismatched key", secondCert, firstKey},
		{"malformed certificate", []byte("invalid certificate"), secondKey},
	} {
		t.Run(invalid.name, func(t *testing.T) {
			rotate(t, fmt.Sprintf("..invalid-%d", i), invalid.cert, invalid.key)
			response, err := client.Get("https://" + publicAddress + "/")
			if response != nil {
				response.Body.Close()
			}
			if err == nil {
				t.Fatal("new HTTPS connection accepted invalid mounted material")
			}
			// Invalid material is rejected before readiness contacts the backend.
			response, err = client.Get("http://" + healthAddress + "/readyz")
			if err != nil {
				t.Fatal(err)
			}
			body, readErr := io.ReadAll(response.Body)
			response.Body.Close()
			if readErr != nil || response.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(body), "TLS certificate invalid") {
				t.Fatalf("invalid certificate was not reported by readiness: %d %s (%v)", response.StatusCode, body, readErr)
			}
			rotate(t, fmt.Sprintf("..repaired-%d", i), secondCert, secondKey)
			checkCertificate(t, 2)
		})
	}
}

func gatewayTestListener(t *testing.T) net.Listener {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	return listener
}

func gatewayTestCertificate(t *testing.T, serial int64) ([]byte, []byte) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	certificate := &x509.Certificate{
		SerialNumber: big.NewInt(serial), DNSNames: []string{"catalog.example.test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, certificate, certificate, pub, priv)
	if err != nil {
		t.Fatal(err)
	}
	key, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key})
}
