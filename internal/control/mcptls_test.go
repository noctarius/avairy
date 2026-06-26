package control

import (
	"crypto/tls"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The node's MCP proxy reaches an https bus by trusting the CA (its TLS client transport), and
// still stamps the agent identity — so inter-agent traffic to a remote core can be encrypted.
func TestMCPProxyOverTLS(t *testing.T) {
	ca, err := EnsureCA(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	serverCert, err := ca.ServerTLS([]string{"127.0.0.1", "localhost"})
	if err != nil {
		t.Fatal(err)
	}

	var gotAgent string
	backend := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAgent = r.Header.Get("X-Avairy-Agent")
		_, _ = io.WriteString(w, "bus-ok")
	}))
	backend.TLS = &tls.Config{Certificates: []tls.Certificate{serverCert}}
	backend.StartTLS()
	t.Cleanup(backend.Close)

	n := NewNode("http://unused", "linbot")
	client, err := TLSClientPEM(ca.CertPEM(), false, nil, nil) // trusts the CA
	if err != nil {
		t.Fatal(err)
	}
	n.HTTP = client

	h, err := n.MCPProxy(backend.URL, "alice")
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(h)
	t.Cleanup(front.Close)

	resp, err := http.Get(front.URL + "/mcp")
	if err != nil {
		t.Fatalf("request through proxy to https bus: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != "bus-ok" {
		t.Fatalf("proxy did not reach TLS bus: %s %q", resp.Status, body)
	}
	if gotAgent != "alice" {
		t.Fatalf("identity not stamped: %q", gotAgent)
	}
}
