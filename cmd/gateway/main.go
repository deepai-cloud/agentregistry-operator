package main

import (
	"context"
	"crypto/tls"
	"github.com/deepai-cloud/agentregistry-operator/internal/gateway"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
func main() {
	g, err := gateway.New(gateway.Config{AuthDir: env("AUTH_DIR", "/auth")})
	if err != nil {
		log.Fatal(err)
	}
	healthMux := http.NewServeMux()
	healthMux.HandleFunc("GET /readyz", g.Ready)
	healthMux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	public := &http.Server{Addr: env("LISTEN_ADDRESS", ":8443"), Handler: g, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16384}
	health := &http.Server{Addr: env("HEALTH_ADDRESS", ":9090"), Handler: healthMux, ReadHeaderTimeout: time.Second, ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second}
	cert, key := os.Getenv("TLS_CERT_FILE"), os.Getenv("TLS_KEY_FILE")
	if (cert == "") != (key == "") {
		log.Fatal("both TLS_CERT_FILE and TLS_KEY_FILE must be set")
	}
	if cert != "" {
		public.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			pair, err := tls.LoadX509KeyPair(cert, key)
			return &pair, err
		}}
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	failed := make(chan error, 2)
	go func() { failed <- health.ListenAndServe() }()
	go func() {
		if cert != "" {
			failed <- public.ListenAndServeTLS("", "")
		} else {
			failed <- public.ListenAndServe()
		}
	}()
	select {
	case <-ctx.Done():
	case err := <-failed:
		if err != nil && err != http.ErrServerClosed {
			log.Printf("gateway listener failed: %v", err)
		}
	}
	end, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = public.Shutdown(end)
	_ = health.Shutdown(end)
}
