package control

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	pkcs12 "software.sslmate.com/src/go-pkcs12"
)

// Self-managed CA (DESIGN.md §4): so an operator can run a TLS control channel without
// obtaining certs, core generates a self-signed CA (persisted, stable across restarts so node
// trust survives) and issues its own server cert from it. The CA's public cert travels to nodes
// inside the join bundle, so a node trusts core without anyone copying a cert file by hand.
type CA struct {
	cert    *x509.Certificate
	key     *ecdsa.PrivateKey
	certPEM []byte
}

// EnsureCA loads the CA from dir (ca.crt + ca.key) or generates and persists a new one.
func EnsureCA(dir string) (*CA, error) {
	certPath, keyPath := filepath.Join(dir, "ca.crt"), filepath.Join(dir, "ca.key")
	if cp, err := os.ReadFile(certPath); err == nil {
		if kp, err := os.ReadFile(keyPath); err == nil {
			return loadCA(cp, kp)
		}
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serialNumber(),
		Subject:               pkix.Name{CommonName: "avairy-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		return nil, err
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil { // private key: owner-only
		return nil, err
	}
	return &CA{cert: cert, key: key, certPEM: certPEM}, nil
}

// CertPEM is the CA's public certificate (what nodes trust; carried in the join bundle).
func (ca *CA) CertPEM() []byte { return ca.certPEM }

// ServerTLS issues a fresh server certificate signed by the CA, valid for the given hosts
// (IPs and DNS names), for serving the control channel.
func (ca *CA) ServerTLS(hosts []string) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serialNumber(),
		Subject:      pkix.Name{CommonName: "avairy-core"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(10, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else if h != "" {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		return tls.Certificate{}, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return tls.Certificate{}, err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return tls.X509KeyPair(certPEM, keyPEM)
}

// ClientTLS issues a client certificate carrying nodeID signed by the CA, for mTLS auth. A node
// presenting it is authenticated as nodeID without an enrollment token (DESIGN.md §4). The id
// lives in a URI SAN (avairy:<id>) — the modern place for identity — with CN set for humans.
func (ca *CA) ClientTLS(nodeID string) (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serialNumber(),
		Subject:      pkix.Name{CommonName: nodeID},
		URIs:         []*url.URL{{Scheme: "avairy", Opaque: nodeID}}, // authoritative identity (SAN)
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(10, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, nil
}

// OperatorTLS issues a client cert for a human operator (browser / avairy-tui mTLS, #30). Its
// identity lives in a DISTINCT URI SAN (avairy-operator:<name>) so an operator cert is never
// mistaken for a node cert, nor a node cert for an operator.
func (ca *CA) OperatorTLS(name string) (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serialNumber(),
		Subject:      pkix.Name{CommonName: name},
		URIs:         []*url.URL{{Scheme: "avairy-operator", Opaque: name}},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(10, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, nil
}

// OperatorP12 mints an operator client cert and encodes it (with the CA in the chain) as a
// password-protected PKCS#12 bundle for import into a browser / OS keychain (#30).
func (ca *CA) OperatorP12(name, password string) ([]byte, error) {
	certPEM, keyPEM, err := ca.OperatorTLS(name)
	if err != nil {
		return nil, err
	}
	cb, _ := pem.Decode(certPEM)
	kb, _ := pem.Decode(keyPEM)
	cert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return nil, err
	}
	key, err := x509.ParseECPrivateKey(kb.Bytes)
	if err != nil {
		return nil, err
	}
	return pkcs12.Modern.Encode(key, cert, []*x509.Certificate{ca.cert}, password)
}

// OperatorIDFromCert returns the operator name from a verified client cert's avairy-operator: URI
// SAN, or "" if it isn't an operator cert (e.g. a node cert) — so operator auth accepts only
// operator certs, not any CA-signed cert.
func OperatorIDFromCert(cert *x509.Certificate) string {
	for _, u := range cert.URIs {
		if u.Scheme == "avairy-operator" {
			if u.Opaque != "" {
				return u.Opaque
			}
			return strings.TrimPrefix(u.Path, "/")
		}
	}
	return ""
}

// Pool returns a cert pool containing the CA, for verifying node client certs.
func (ca *CA) Pool() *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(ca.cert)
	return pool
}

// nodeIDFromCert extracts the avairy node id from a (verified) client cert: the avairy: URI
// SAN, falling back to the CN for older certs. Empty if neither is present.
func nodeIDFromCert(cert *x509.Certificate) string {
	for _, u := range cert.URIs {
		if u.Scheme == "avairy" {
			if u.Opaque != "" {
				return u.Opaque
			}
			return strings.TrimPrefix(u.Path, "/")
		}
	}
	return cert.Subject.CommonName
}

func loadCA(certPEM, keyPEM []byte) (*CA, error) {
	certBlock, _ := pem.Decode(certPEM)
	keyBlock, _ := pem.Decode(keyPEM)
	if certBlock == nil || keyBlock == nil {
		return nil, fmt.Errorf("control: malformed CA PEM")
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, err
	}
	key, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, err
	}
	return &CA{cert: cert, key: key, certPEM: certPEM}, nil
}

func serialNumber() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if n == nil {
		n = big.NewInt(1)
	}
	return n
}
