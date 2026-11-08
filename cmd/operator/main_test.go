package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestShutdownWaitsForActiveRequest(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	entered, release, shuttingDown := make(chan struct{}), make(chan struct{}), make(chan struct{})
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		_, _ = io.WriteString(w, "completed")
	})}
	defer server.Close()
	defer close(release)
	server.RegisterOnShutdown(func() { close(shuttingDown) })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	go func() { finished <- serve(ctx, server, listener) }()

	response := make(chan string, 1)
	requestError := make(chan error, 1)
	go func() {
		client := &http.Client{Timeout: 5 * time.Second}
		resp, err := client.Get("http://" + listener.Addr().String())
		if err != nil {
			requestError <- err
			return
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			requestError <- err
			return
		}
		response <- string(body)
	}()
	select {
	case <-entered:
	case err := <-requestError:
		t.Fatal(err)
	case <-time.After(5 * time.Second):
		t.Fatal("request did not enter the handler")
	}
	cancel()
	select {
	case <-shuttingDown:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not start")
	}
	select {
	case err := <-finished:
		t.Fatalf("shutdown abandoned an active request: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	release <- struct{}{}
	select {
	case body := <-response:
		if body != "completed" {
			t.Fatalf("incomplete response: %q", body)
		}
	case err := <-requestError:
		t.Fatal(err)
	case <-time.After(5 * time.Second):
		t.Fatal("active request did not complete")
	}
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not finish after the request completed")
	}
}

func TestServeReturnsListenerFailure(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listener.Close()
	finished := make(chan error, 1)
	go func() { finished <- serve(context.Background(), &http.Server{}, listener) }()
	select {
	case err := <-finished:
		if err == nil {
			t.Fatal("listener failure was ignored")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("listener failure waited for a shutdown signal")
	}
}
