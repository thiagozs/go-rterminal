package tlsutil

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
)

// ClientConfig builds the TLS configuration shared by the agent and client.
func ClientConfig(caPath string, insecure bool) (*tls.Config, error) {
	config := &tls.Config{InsecureSkipVerify: insecure} //nolint:gosec // Explicit CLI option.
	if caPath == "" {
		return config, nil
	}

	pem, err := os.ReadFile(caPath)
	if err != nil {
		return nil, fmt.Errorf("ler CA %q: %w", caPath, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("carregar CA %q: nenhum certificado PEM válido", caPath)
	}
	config.RootCAs = pool
	return config, nil
}
