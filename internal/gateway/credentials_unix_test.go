//go:build unix

package gateway

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestAuthenticationSecretRotationDoesNotMixRevisions(t *testing.T) {
	for _, removeOld := range []bool{false, true} {
		name := "old revision retained"
		if removeOld {
			name = "old revision removed"
		}
		t.Run(name, func(t *testing.T) {
			directory := t.TempDir()
			old := filepath.Join(directory, "..old")
			current := filepath.Join(directory, "..current")
			for _, revision := range []string{old, current} {
				if err := os.Mkdir(revision, 0700); err != nil {
					t.Fatal(err)
				}
			}
			write := func(path, value string) {
				t.Helper()
				if err := os.WriteFile(path, []byte(value), 0600); err != nil {
					t.Fatal(err)
				}
			}
			const oldPassword = "old-password-long-enough"
			const newPassword = "new-password-long-enough"
			write(filepath.Join(old, "password"), oldPassword)
			write(filepath.Join(current, "username"), "bob")
			write(filepath.Join(current, "password"), newPassword)
			for link, target := range map[string]string{
				"username": "..data/username", "password": "..data/password", "..data": "..old",
			} {
				if err := os.Symlink(target, filepath.Join(directory, link)); err != nil {
					t.Fatal(err)
				}
			}
			// A FIFO pauses the real username read after opening the old revision.
			// No production hooks or probabilistic concurrent-rotation loop needed.
			fifo := filepath.Join(old, "username")
			if err := syscall.Mkfifo(fifo, 0600); err != nil {
				t.Fatal(err)
			}
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			}))
			defer up.Close()
			g, err := New(Config{AuthDir: directory, HTTPBackend: up.URL, MCPBackend: up.URL})
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest("GET", "/", nil)
			request.SetBasicAuth("alice", newPassword) // This pair has never been configured.
			response := httptest.NewRecorder()
			done := make(chan struct{})
			go func() {
				g.ServeHTTP(response, request)
				close(done)
			}()
			var writer *os.File
			deadline := time.Now().Add(5 * time.Second)
			for {
				writer, err = os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0600)
				if err == nil {
					break
				}
				if !errors.Is(err, syscall.ENXIO) || time.Now().After(deadline) {
					t.Fatalf("waiting for username reader: %v", err)
				}
				select {
				case <-done:
					t.Fatal("authentication returned before reading the username")
				case <-time.After(time.Millisecond):
				}
			}
			defer writer.Close()
			if err := os.Symlink("..current", filepath.Join(directory, "..data_tmp")); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(filepath.Join(directory, "..data_tmp"), filepath.Join(directory, "..data")); err != nil {
				t.Fatal(err)
			}
			if removeOld {
				if err := os.RemoveAll(old); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := io.WriteString(writer, "alice"); err != nil {
				t.Fatal(err)
			}
			writer.Close()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("authentication did not complete")
			}
			want := http.StatusUnauthorized
			if removeOld {
				want = http.StatusServiceUnavailable
			}
			if response.Code != want {
				t.Fatalf("mixed credentials returned HTTP %d, want %d", response.Code, want)
			}
			// The next request must observe the new revision without restarting.
			for _, credentials := range []struct {
				user, password string
				status         int
			}{{"bob", newPassword, http.StatusNoContent}, {"alice", oldPassword, http.StatusUnauthorized}, {"alice", newPassword, http.StatusUnauthorized}} {
				r := httptest.NewRequest("GET", "/", nil)
				r.SetBasicAuth(credentials.user, credentials.password)
				w := httptest.NewRecorder()
				g.ServeHTTP(w, r)
				if w.Code != credentials.status {
					t.Errorf("authentication after rotation: got %d, want %d", w.Code, credentials.status)
				}
			}
		})
	}
}
