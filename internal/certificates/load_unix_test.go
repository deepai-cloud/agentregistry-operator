//go:build unix

package certificates

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestLoadKeyPairDuringSecretRotation(t *testing.T) {
	oldCert, oldKey := testKeyPair(t, 1)
	newCert, newKey := testKeyPair(t, 2)
	for _, tc := range []struct {
		name      string
		removeOld bool
		invalid   bool
		serial    int64
	}{
		{"old revision retained", false, false, 1},
		{"old revision removed", true, false, 2},
		{"invalid replacement", true, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			directory := t.TempDir()
			old, current := filepath.Join(directory, "..old"), filepath.Join(directory, "..current")
			for _, revision := range []string{old, current} {
				if err := os.Mkdir(revision, 0700); err != nil {
					t.Fatal(err)
				}
			}
			writeMaterial(t, filepath.Join(old, "tls.key"), oldKey)
			writeMaterial(t, filepath.Join(current, "tls.crt"), newCert)
			key := newKey
			if tc.invalid {
				key = oldKey
			}
			writeMaterial(t, filepath.Join(current, "tls.key"), key)
			for link, target := range map[string]string{"tls.crt": "..data/tls.crt", "tls.key": "..data/tls.key", "..data": "..old"} {
				if err := os.Symlink(target, filepath.Join(directory, link)); err != nil {
					t.Fatal(err)
				}
			}
			// Hold the actual certificate read open until after ..data swaps.
			// This deterministically exercises rotation between the two reads.
			fifo := filepath.Join(old, "tls.crt")
			if err := syscall.Mkfifo(fifo, 0600); err != nil {
				t.Fatal(err)
			}
			type result struct {
				pair tls.Certificate
				err  error
			}
			done := make(chan result, 1)
			certFile, keyFile := filepath.Join(directory, "tls.crt"), filepath.Join(directory, "tls.key")
			go func() {
				pair, err := LoadKeyPair(certFile, keyFile)
				done <- result{pair, err}
			}()
			var writer *os.File
			var err error
			deadline := time.Now().Add(5 * time.Second)
			for {
				writer, err = os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0600)
				if err == nil {
					break
				}
				if !errors.Is(err, syscall.ENXIO) || time.Now().After(deadline) {
					t.Fatalf("waiting for certificate reader: %v", err)
				}
				select {
				case result := <-done:
					t.Fatalf("load returned before reading certificate: %v", result.err)
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
			if tc.removeOld {
				if err := os.RemoveAll(old); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := writer.Write(oldCert); err != nil {
				t.Fatal(err)
			}
			writer.Close()
			select {
			case result := <-done:
				if tc.invalid {
					if result.err == nil {
						t.Fatal("invalid replacement accepted during rotation")
					}
				} else {
					if result.err != nil {
						t.Fatalf("rotation interrupted loading a valid pair: %v", result.err)
					}
					leaf, err := x509.ParseCertificate(result.pair.Certificate[0])
					if err != nil || leaf.SerialNumber.Int64() != tc.serial {
						t.Fatalf("unexpected certificate revision: %v", err)
					}
				}
			case <-time.After(5 * time.Second):
				t.Fatal("certificate load did not complete")
			}
			pair, err := LoadKeyPair(certFile, keyFile)
			if tc.invalid {
				if err == nil {
					t.Fatal("invalid current revision used a cached certificate")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				leaf, err := x509.ParseCertificate(pair.Certificate[0])
				if err != nil || leaf.SerialNumber.Int64() != 2 {
					t.Fatalf("subsequent load did not use the current revision: %v", err)
				}
			}
		})
	}
}
