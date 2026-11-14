// Package wstunnel carries WireGuard datagrams to a wstunnel v11 server over a
// WebSocket. The server learns where to forward them from a JWT carried in the
// WebSocket Sec-WebSocket-Protocol header, which BuildToken mints.
package wstunnel

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"strconv"
)

// tokenKey signs every token this process mints. The server decodes the token
// without verifying the signature, so any per-process key is accepted; a random
// one keeps our tokens distinct from anybody else's and costs nothing.
var tokenKey = mustRandomKey()

// mustRandomKey returns a fresh 32-byte key. crypto/rand aborts the process on a
// broken entropy source, so the error path here is unreachable in practice.
func mustRandomKey() []byte {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		panic("wstunnel: cannot read random bytes for the JWT key: " + err.Error())
	}
	return key
}

// header is the fixed JOSE header. Field order is load-bearing: the token must
// decode to exactly {"alg":"HS256","typ":"JWT"}.
type header struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
}

// claims is the token payload. The server's serde types are
// LocalProtocol::Udp { timeout: Option<Duration> }, which serialises as
// {"Udp":{"timeout":null}}: the enum wrapper is the "Udp" key and a null
// timeout means "never tear the tunnel down for idleness" (the CLI's
// timeout_sec=0).
type claims struct {
	ID string   `json:"id"`
	P  protocol `json:"p"`
	R  string   `json:"r"`
	RP uint16   `json:"rp"`
}

type protocol struct {
	UDP udp `json:"Udp"`
}

type udp struct {
	Timeout *int `json:"timeout"`
}

// BuildToken returns a three-segment HS256 JWT that asks a wstunnel v11 server
// to forward UDP traffic to target, which must be a "host:port" pair such as
// "127.0.0.1:51820". The host is used verbatim: it is never resolved or
// rewritten here, because the config layer already resolved [Peer] Endpoint.
//
// Every error is prefixed with "wstunnel:" so callers can print one line.
func BuildToken(target string) (string, error) {
	host, port, err := splitTarget(target)
	if err != nil {
		return "", err
	}

	id, err := newUUID()
	if err != nil {
		return "", err
	}

	headerJSON, err := json.Marshal(header{Alg: "HS256", Typ: "JWT"})
	if err != nil {
		return "", fmt.Errorf("wstunnel: marshal JWT header: %w", err)
	}
	claimsJSON, err := json.Marshal(claims{
		ID: id,
		// Timeout is deliberately nil: no idle teardown.
		P:  protocol{UDP: udp{}},
		R:  host,
		RP: port,
	})
	if err != nil {
		return "", fmt.Errorf("wstunnel: marshal JWT claims: %w", err)
	}

	enc := base64.RawURLEncoding
	unsigned := enc.EncodeToString(headerJSON) + "." + enc.EncodeToString(claimsJSON)

	mac := hmac.New(sha256.New, tokenKey)
	mac.Write([]byte(unsigned))

	return unsigned + "." + enc.EncodeToString(mac.Sum(nil)), nil
}

// splitTarget splits a "host:port" pair and validates the port. A host may be
// empty in a net.SplitHostPort sense ("[::1]:51820" yields "::1"), but a target
// without a host cannot name a destination, so it is rejected too.
func splitTarget(target string) (string, uint16, error) {
	host, portStr, err := net.SplitHostPort(target)
	if err != nil {
		return "", 0, fmt.Errorf("wstunnel: bad target %q, want host:port: %w", target, err)
	}
	if host == "" {
		return "", 0, fmt.Errorf("wstunnel: bad target %q, host is empty", target)
	}

	// A 16-bit unsigned parse accepts exactly 1..65535 once zero is ruled out,
	// and rejects signs, spaces and non-digits.
	port, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil {
		return "", 0, fmt.Errorf("wstunnel: bad target %q, port must be a number in 1..65535: %w", target, err)
	}
	if port == 0 {
		return "", 0, fmt.Errorf("wstunnel: bad target %q, port must be a number in 1..65535", target)
	}

	return host, uint16(port), nil
}

// newUUID returns a random (version 4, RFC 4122 variant) UUID in the canonical
// 8-4-4-4-12 form.
func newUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("wstunnel: generate JWT id: %w", err)
	}
	b[6] = b[6]&0x0f | 0x40 // version 4
	b[8] = b[8]&0x3f | 0x80 // RFC 4122 variant

	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
