package wstunnel

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

// uuidV4Pattern is the canonical 8-4-4-4-12 form with version 4 and the RFC
// 4122 variant bits set.
var uuidV4Pattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// segment decodes one token segment as unpadded base64url.
func segment(t *testing.T, name, seg string) []byte {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		t.Fatalf("%s segment is not unpadded base64url: %v", name, err)
	}
	return raw
}

func TestBuildToken_HasThreeHS256Segments(t *testing.T) {
	tok, err := BuildToken("127.0.0.1:51820")
	if err != nil {
		t.Fatalf("BuildToken: %v", err)
	}

	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d segments, want 3: %q", len(parts), tok)
	}

	const wantHeader = `{"alg":"HS256","typ":"JWT"}`
	if got := string(segment(t, "header", parts[0])); got != wantHeader {
		t.Errorf("header = %s, want %s", got, wantHeader)
	}
}

func TestBuildToken_SignatureIsHS256OverHeaderAndPayload(t *testing.T) {
	tok, err := BuildToken("127.0.0.1:51820")
	if err != nil {
		t.Fatalf("BuildToken: %v", err)
	}

	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d segments, want 3: %q", len(parts), tok)
	}

	mac := hmac.New(sha256.New, tokenKey)
	mac.Write([]byte(parts[0] + "." + parts[1]))
	want := mac.Sum(nil)
	if got := segment(t, "signature", parts[2]); !hmac.Equal(got, want) {
		t.Errorf("signature = %x, want %x", got, want)
	}
}

func TestBuildToken_ClaimShapeMatchesServerSerde(t *testing.T) {
	tok, err := BuildToken("127.0.0.1:51820")
	if err != nil {
		t.Fatalf("BuildToken: %v", err)
	}

	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d segments, want 3: %q", len(parts), tok)
	}

	var claim map[string]json.RawMessage
	if err := json.Unmarshal(segment(t, "payload", parts[1]), &claim); err != nil {
		t.Fatalf("payload is not a JSON object: %v", err)
	}

	if len(claim) != 4 {
		t.Errorf("claim has %d keys (%v), want exactly id, p, r, rp", len(claim), claim)
	}

	// The server's serde type is LocalProtocol::Udp { timeout: Option<Duration> },
	// so "p" must carry the externally tagged enum wrapper and a null timeout.
	const wantProtocol = `{"Udp":{"timeout":null}}`
	if got := string(claim["p"]); got != wantProtocol {
		t.Errorf("p = %s, want %s", got, wantProtocol)
	}
	if got := string(claim["r"]); got != `"127.0.0.1"` {
		t.Errorf("r = %s, want %q", got, "127.0.0.1")
	}
	if got := string(claim["rp"]); got != `51820` {
		t.Errorf("rp = %s, want 51820", got)
	}

	var id string
	if err := json.Unmarshal(claim["id"], &id); err != nil {
		t.Fatalf("id is not a JSON string: %v", err)
	}
	if !uuidV4Pattern.MatchString(id) {
		t.Errorf("id = %q, want a random UUID v4", id)
	}
}

func TestBuildToken_FreshIDPerCall(t *testing.T) {
	first, err := BuildToken("127.0.0.1:51820")
	if err != nil {
		t.Fatalf("BuildToken: %v", err)
	}
	second, err := BuildToken("127.0.0.1:51820")
	if err != nil {
		t.Fatalf("BuildToken: %v", err)
	}

	id := func(tok string) string {
		var claim struct {
			ID string `json:"id"`
		}
		parts := strings.Split(tok, ".")
		if len(parts) != 3 {
			t.Fatalf("token has %d segments, want 3: %q", len(parts), tok)
		}
		if err := json.Unmarshal(segment(t, "payload", parts[1]), &claim); err != nil {
			t.Fatalf("payload is not a JSON object: %v", err)
		}
		return claim.ID
	}

	if id(first) == id(second) {
		t.Errorf("two tokens share the id %q, want a fresh UUID per token", id(first))
	}
}

func TestBuildToken_HostIsPassedThroughVerbatim(t *testing.T) {
	tests := []struct {
		target string
		want   string
	}{
		{"127.0.0.1:51820", "127.0.0.1"},
		{"wg.example.internal:51820", "wg.example.internal"},
		// SplitHostPort strips the brackets of an IPv6 literal; the server
		// re-joins host and port itself, so the bare address is correct.
		{"[fd00::1]:51820", "fd00::1"},
	}

	for _, tc := range tests {
		t.Run(tc.target, func(t *testing.T) {
			tok, err := BuildToken(tc.target)
			if err != nil {
				t.Fatalf("BuildToken(%q): %v", tc.target, err)
			}

			parts := strings.Split(tok, ".")
			if len(parts) != 3 {
				t.Fatalf("token has %d segments, want 3: %q", len(parts), tok)
			}
			var claim struct {
				R string `json:"r"`
			}
			if err := json.Unmarshal(segment(t, "payload", parts[1]), &claim); err != nil {
				t.Fatalf("payload is not a JSON object: %v", err)
			}
			if claim.R != tc.want {
				t.Errorf("r = %q, want %q", claim.R, tc.want)
			}
		})
	}
}

func TestBuildToken_RejectsBadTarget(t *testing.T) {
	tests := []struct {
		name   string
		target string
	}{
		{"empty", ""},
		{"no port", "127.0.0.1"},
		{"empty port", "127.0.0.1:"},
		{"non-numeric port", "127.0.0.1:wg"},
		{"port zero", "127.0.0.1:0"},
		{"port above 65535", "127.0.0.1:65536"},
		{"negative port", "127.0.0.1:-1"},
		{"empty host", ":51820"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tok, err := BuildToken(tc.target); err == nil {
				t.Fatalf("BuildToken(%q) = %q, want an error", tc.target, tok)
			}
		})
	}
}
