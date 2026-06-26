package control

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
)

// TLSClient builds an HTTP client for a node dialing a TLS control endpoint (DESIGN.md §4: the
// channel is HTTP in dev, TLS in production). caFile, if set, is a PEM bundle the node trusts
// (hand core's cert/CA to the node the way the enroll token is handed over) — needed for the
// self-signed / internal-CA certs typical of a self-hosted core; with a publicly-trusted cert
// the default system roots suffice and no client override is required. insecure skips
// verification entirely (dev only — it exposes the channel to MITM, defeating the point).
func TLSClient(caFile string, insecure bool) (*http.Client, error) {
	var caPEM []byte
	if caFile != "" {
		b, err := os.ReadFile(caFile)
		if err != nil {
			return nil, err
		}
		caPEM = b
	}
	return TLSClientPEM(caPEM, insecure, nil, nil)
}

// TLSClientPEM builds a node's HTTP client from in-memory PEM (as carried in a join bundle):
// caPEM is the CA to trust (empty → system roots); clientCert/clientKey, if set, are presented
// for mTLS so a valid client cert authenticates the node in place of an enrollment token.
func TLSClientPEM(caPEM []byte, insecure bool, clientCert, clientKey []byte) (*http.Client, error) {
	tc := &tls.Config{InsecureSkipVerify: insecure} //nolint:gosec // insecure is an explicit opt-in
	if len(caPEM) > 0 {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caPEM) {
			return nil, fmt.Errorf("control: no certificates found in CA PEM")
		}
		tc.RootCAs = pool
	}
	if len(clientCert) > 0 && len(clientKey) > 0 {
		cert, err := tls.X509KeyPair(clientCert, clientKey)
		if err != nil {
			return nil, fmt.Errorf("control: bad client cert: %w", err)
		}
		tc.Certificates = []tls.Certificate{cert}
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: tc}}, nil
}
