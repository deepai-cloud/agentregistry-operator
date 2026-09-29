// Package certificates contains deterministic checks shared by sync and health.
package certificates

import (
	"crypto/x509"
	"errors"
)

// ValidateLeaf checks critical extensions and permitted TLS server purpose.
// An absent EKU extension is unrestricted; an unknown-only EKU is restricted.
// Hostname, validity time and issuer trust are separate concerns.
func ValidateLeaf(cert *x509.Certificate) error {
	if len(cert.UnhandledCriticalExtensions) > 0 {
		return errors.New("TLS certificate has unsupported critical extensions")
	}
	if len(cert.ExtKeyUsage) == 0 && len(cert.UnknownExtKeyUsage) == 0 {
		return nil
	}
	for _, usage := range cert.ExtKeyUsage {
		if usage == x509.ExtKeyUsageServerAuth || usage == x509.ExtKeyUsageAny {
			return nil
		}
	}
	return errors.New("TLS certificate must permit server authentication")
}
