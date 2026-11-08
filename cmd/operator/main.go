package main

import (
	"context"
	"github.com/deepai-cloud/agentregistry-operator/internal/operator"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	image := os.Getenv("GATEWAY_IMAGE")
	if image == "" {
		log.Fatal("GATEWAY_IMAGE is required")
	}
	server := &http.Server{Addr: ":8080", Handler: operator.NewHandler(operator.Config{GatewayImage: image}), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16384}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	listener, err := net.Listen("tcp", server.Addr)
	if err != nil {
		log.Fatal(err)
	}
	if err := serve(ctx, server, listener); err != nil {
		log.Fatal(err)
	}
}

func serve(ctx context.Context, server *http.Server, listener net.Listener) error {
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	select {
	case err := <-served:
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	case <-ctx.Done():
	}
	// Serve returns as soon as Shutdown closes the listener. Keep the process
	// alive until active webhook requests finish or the grace period expires.
	end, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(end); err != nil {
		return err
	}
	if err := <-served; err != http.ErrServerClosed {
		return err
	}
	return nil
}
