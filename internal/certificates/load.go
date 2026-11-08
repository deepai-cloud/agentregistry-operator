package certificates

import (
	"crypto/tls"
	"os"
	"path/filepath"
)

// LoadKeyPair reads both files from one Kubernetes Secret revision when they
// share a projected volume. Ordinary files and other symlink layouts retain
// tls.LoadX509KeyPair semantics. No certificate or resolved path is cached.
func LoadKeyPair(certFile, keyFile string) (tls.Certificate, error) {
	directory := filepath.Dir(certFile)
	if directory != filepath.Dir(keyFile) || !projectedFile(certFile) || !projectedFile(keyFile) {
		return tls.LoadX509KeyPair(certFile, keyFile)
	}
	link := filepath.Join(directory, "..data")
	revision, err := os.Readlink(link)
	if err != nil {
		return tls.Certificate{}, err
	}
	load := func(revision string) (tls.Certificate, error) {
		if !filepath.IsAbs(revision) {
			revision = filepath.Join(directory, revision)
		}
		return tls.LoadX509KeyPair(filepath.Join(revision, filepath.Base(certFile)), filepath.Join(revision, filepath.Base(keyFile)))
	}
	pair, err := load(revision)
	if err == nil {
		return pair, nil
	}
	// Kubelet can remove the old revision before both reads finish. Retry once
	// only if the projection changed; invalid current material still fails.
	if current, readErr := os.Readlink(link); readErr == nil && current != revision {
		return load(current)
	}
	return pair, err
}

func projectedFile(path string) bool {
	target, err := os.Readlink(path)
	return err == nil && target == filepath.Join("..data", filepath.Base(path))
}
