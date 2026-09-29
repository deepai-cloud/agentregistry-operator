package main

import (
	"context"
	"github.com/deepai-cloud/agentregistry-operator/internal/operator"
	"log"
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
	go func() {
		<-ctx.Done()
		end, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(end)
	}()
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
