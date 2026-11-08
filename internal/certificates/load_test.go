package certificates

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testKeyPair(t *testing.T, serial int64) ([]byte, []byte) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(serial), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, leaf, leaf, public, private)
	if err != nil {
		t.Fatal(err)
	}
	key, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key})
}

func writeMaterial(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestLoadKeyPairSupportsOrdinaryFilesAndSymlinks(t *testing.T) {
	cert, key := testKeyPair(t, 1)
	for _, layout := range []string{"same directory", "different directories", "custom symlinks"} {
		t.Run(layout, func(t *testing.T) {
			directory := t.TempDir()
			certFile, keyFile := filepath.Join(directory, "cert.pem"), filepath.Join(directory, "key.pem")
			if layout == "different directories" {
				keyFile = filepath.Join(t.TempDir(), "key.pem")
			}
			writeMaterial(t, certFile, cert)
			writeMaterial(t, keyFile, key)
			// An unrelated ..data marker must not override ordinary files or
			// arbitrary symlinks that do not use Kubernetes' projection layout.
			if err := os.Symlink("missing-revision", filepath.Join(directory, "..data")); err != nil {
				t.Fatal(err)
			}
			if layout == "custom symlinks" {
				for _, file := range []string{certFile, keyFile} {
					if err := os.Symlink(file, file+".link"); err != nil {
						t.Fatal(err)
					}
				}
				certFile, keyFile = certFile+".link", keyFile+".link"
			}
			pair, err := LoadKeyPair(certFile, keyFile)
			if err != nil {
				t.Fatal(err)
			}
			leaf, err := x509.ParseCertificate(pair.Certificate[0])
			if err != nil || leaf.SerialNumber.Int64() != 1 {
				t.Fatalf("unexpected certificate: %v", err)
			}
		})
	}
}
