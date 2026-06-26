package control

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
)

// JoinBundle is the single artifact an operator hands a new node: where core is, how to trust
// it (the self-signed CA's public cert), and how to authenticate — either a one-time enrollment
// Token, or an mTLS client certificate+key (ClientCert/ClientKey) that stands in for the token
// (DESIGN.md §4). It's transported as one opaque base64 string (EncodeJoin), so "the pubcert
// travels with the token" is automatic — no copying cert files by hand.
type JoinBundle struct {
	Core       string `json:"core"`                 // control API URL (https://… when TLS)
	Bus        string `json:"bus,omitempty"`        // MCP bus base URL (supplies -core-mcp, needed by -family)
	CA         []byte `json:"ca,omitempty"`         // PEM of core's CA cert to trust (empty = public/none)
	Token      string `json:"token,omitempty"`      // one-time enrollment token
	NodeID     string `json:"nodeId,omitempty"`     // suggested/required node id (matches a client cert CN)
	ClientCert []byte `json:"clientCert,omitempty"` // mTLS client cert (PEM) — alternative to Token
	ClientKey  []byte `json:"clientKey,omitempty"`  // mTLS client key (PEM)
}

// EncodeJoin renders a bundle as one base64 token string.
func EncodeJoin(b JoinBundle) string {
	raw, _ := json.Marshal(b)
	return base64.RawURLEncoding.EncodeToString(raw)
}

// DecodeJoin parses a join string produced by EncodeJoin.
func DecodeJoin(s string) (JoinBundle, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return JoinBundle{}, fmt.Errorf("control: bad join string: %w", err)
	}
	var b JoinBundle
	if err := json.Unmarshal(raw, &b); err != nil {
		return JoinBundle{}, fmt.Errorf("control: bad join bundle: %w", err)
	}
	if b.Core == "" {
		return JoinBundle{}, fmt.Errorf("control: join bundle missing core URL")
	}
	return b, nil
}
