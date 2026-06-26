package control

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"avairy/internal/journal"
	"avairy/internal/workspace"
)

func parseLeaf(t *testing.T, certPEM []byte) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode(certPEM)
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

// EnsureCA generates a usable CA and persists it — a second call loads the same one (so node
// trust survives a core restart). The server cert it issues is trusted by a client holding the
// CA cert, and a client cert's avairy node id round-trips through the SAN.
func TestCAPersistAndCerts(t *testing.T) {
	dir := t.TempDir()
	ca, err := EnsureCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureCA(dir); err != nil { // reload path
		t.Fatalf("reload CA: %v", err)
	}
	// Persisted to disk.
	for _, f := range []string{"ca.crt", "ca.key"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Fatalf("%s not persisted: %v", f, err)
		}
	}

	// Client cert carries the node id in the SAN.
	certPEM, _, err := ca.ClientTLS("linbot")
	if err != nil {
		t.Fatal(err)
	}
	if id := nodeIDFromCert(parseLeaf(t, certPEM)); id != "linbot" {
		t.Fatalf("node id from cert = %q, want linbot", id)
	}
}

// mTLS enrollment: a node presenting a CA-issued client cert authenticates by its SAN id with
// no token; a mismatched id is rejected.
func TestEnrollByClientCert(t *testing.T) {
	dir := t.TempDir()
	ca, err := EnsureCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	c := NewCore(workspace.NewHub(), journal.NewMemory())

	serverCert, err := ca.ServerTLS([]string{"127.0.0.1", "localhost"})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(c.Handler())
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{serverCert},
		ClientAuth:   tls.VerifyClientCertIfGiven,
		ClientCAs:    ca.Pool(),
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)

	certPEM, keyPEM, err := ca.ClientTLS("linbot")
	if err != nil {
		t.Fatal(err)
	}
	client, err := TLSClientPEM(ca.CertPEM(), false, certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}

	// Correct id (matches SAN) → enrolls with no token.
	n := NewNode(srv.URL, "linbot")
	n.HTTP = client
	if err := n.Enroll("", "linux", nil); err != nil {
		t.Fatalf("mTLS enroll: %v", err)
	}

	// Wrong id (cert SAN says linbot) → rejected.
	bad := NewNode(srv.URL, "imposter")
	bad.HTTP = client
	if err := bad.Enroll("", "linux", nil); err == nil {
		t.Fatal("enroll with id != client-cert SAN should be rejected")
	}
}

func TestJoinEncodeDecode(t *testing.T) {
	in := JoinBundle{Core: "https://core:7700", CA: []byte("CA-PEM"), Token: "tok", NodeID: "linbot"}
	out, err := DecodeJoin(EncodeJoin(in))
	if err != nil {
		t.Fatal(err)
	}
	if out.Core != in.Core || string(out.CA) != "CA-PEM" || out.Token != "tok" || out.NodeID != "linbot" {
		t.Fatalf("round-trip mismatch: %+v", out)
	}
	if _, err := DecodeJoin("!!!not base64!!!"); err == nil {
		t.Fatal("bad join string should error")
	}
}
