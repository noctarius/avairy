package control

import (
	"encoding/pem"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"avairy/internal/journal"
	"avairy/internal/workspace"
)

// A node enrolls and syncs over a TLS control channel, trusting core's cert (the production
// shape from DESIGN.md §4). Uses httptest's self-signed server + its trusting client.
func TestEnrollOverTLS(t *testing.T) {
	c := NewCore(workspace.NewHub(), journal.NewMemory())
	srv := httptest.NewTLSServer(c.Handler())
	t.Cleanup(srv.Close)

	n := NewNode(srv.URL, "linbot")
	n.HTTP = srv.Client() // trusts the server's self-signed cert

	if err := n.Enroll(c.CurrentToken(), "linux", nil); err != nil {
		t.Fatalf("enroll over TLS: %v", err)
	}
	if err := n.Heartbeat(); err != nil {
		t.Fatalf("heartbeat over TLS: %v", err)
	}

	// A plain (non-trusting) client must be rejected — TLS is actually verifying.
	plain := NewNode(srv.URL, "other")
	if err := plain.Enroll(c.CurrentToken(), "linux", nil); err == nil {
		t.Fatal("expected TLS verification failure for an untrusting client")
	}
}

// TLSClient with a CA file trusts a server whose cert is in that file; insecure trusts anything.
func TestTLSClientWithCAFile(t *testing.T) {
	c := NewCore(workspace.NewHub(), journal.NewMemory())
	srv := httptest.NewTLSServer(c.Handler())
	t.Cleanup(srv.Close)

	// Write the server's cert to a PEM file and trust it via TLSClient.
	caPath := filepath.Join(t.TempDir(), "core.pem")
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := os.WriteFile(caPath, certPEM, 0o644); err != nil {
		t.Fatal(err)
	}

	client, err := TLSClient(caPath, false)
	if err != nil {
		t.Fatal(err)
	}
	n := NewNode(srv.URL, "linbot")
	n.HTTP = client
	if err := n.Enroll(c.CurrentToken(), "linux", nil); err != nil {
		t.Fatalf("enroll with CA-file trust: %v", err)
	}

	// A bogus CA file is an error; a wrong-but-valid CA fails verification.
	if _, err := TLSClient(filepath.Join(t.TempDir(), "missing.pem"), false); err == nil {
		t.Fatal("missing CA file should error")
	}
	bad := filepath.Join(t.TempDir(), "bad.pem")
	_ = os.WriteFile(bad, []byte("not a cert"), 0o644)
	if _, err := TLSClient(bad, false); err == nil {
		t.Fatal("CA file with no certs should error")
	}

	insecureClient, err := TLSClient("", true)
	if err != nil {
		t.Fatal(err)
	}
	n2 := NewNode(srv.URL, "dev")
	n2.HTTP = insecureClient
	if err := n2.Enroll(c.CurrentToken(), "linux", nil); err != nil {
		t.Fatalf("insecure client should connect: %v", err)
	}
}
