package config

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"

	"github.com/loficore/wg-user/internal/resolve"
)

// resolveHost resolves an Endpoint hostname. It is a package variable so tests
// can run without a resolver or a network.
var resolveHost = resolve.Lookup

// endpointCandidate is one [Peer] Endpoint whose host part is a name, not an IP
// literal, and therefore cannot be parsed by wireproxy on Android.
type endpointCandidate struct {
	// peer is the 1-based [Peer] ordinal, used in error messages.
	peer int
	// host and port are the endpoint's parts.
	host string
	port string
	// start and end delimit the value in the file, including any surrounding
	// quotes but excluding surrounding whitespace, so only the address is
	// rewritten and the rest of the file stays byte-identical.
	start int
	end   int
	// quote is the surrounding quote character, or 0 when the value is bare.
	// The rewriter re-emits it around the address, so start/end keep it too.
	quote byte
}

// rewritePeerEndpoints resolves every [Peer] Endpoint hostname in data and
// returns data with those endpoints replaced by "ip:port". It reports whether
// anything changed; when it did not, the caller must parse the original file
// exactly as before.
func rewritePeerEndpoints(ctx context.Context, data []byte) ([]byte, bool, error) {
	candidates := scanPeerEndpoints(data)
	if len(candidates) == 0 {
		return data, false, nil
	}

	var b strings.Builder
	b.Grow(len(data) + 16)
	last := 0
	for _, c := range candidates {
		addrs, err := resolveHost(ctx, c.host)
		if err != nil {
			return nil, false, fmt.Errorf("[Peer] %d: cannot resolve Endpoint host %q: %w", c.peer, c.host, err)
		}
		if len(addrs) == 0 {
			return nil, false, fmt.Errorf("[Peer] %d: cannot resolve Endpoint host %q: no addresses returned", c.peer, c.host)
		}

		b.Write(data[last:c.start])
		if c.quote != 0 {
			b.WriteByte(c.quote)
		}
		b.WriteString(net.JoinHostPort(addrs[0], c.port))
		if c.quote != 0 {
			b.WriteByte(c.quote)
		}
		last = c.end
	}
	b.Write(data[last:])

	return []byte(b.String()), true, nil
}

// scanPeerEndpoints finds the Endpoint values in [Peer] sections whose host is
// not an IP literal. The scan is textual, not INI-based, so the rest of the file
// — comments, key order, the private key — can be reused verbatim; it mirrors
// the parts of go-ini's parser that decide what a value is: a section header is
// a line that starts with '[' and ends with ']' (name taken verbatim, not
// trimmed), a key ends at the first '=' or ':' (go-ini's default delimiters),
// and a value is trimmed and cut at the first '#' or ';' unless it is
// backtick-quoted, then optionally surrounded by matching quotes.
func scanPeerEndpoints(data []byte) []endpointCandidate {
	var candidates []endpointCandidate
	inPeer := false
	peer := 0

	for offset := 0; offset < len(data); {
		lineStart := offset
		lineEnd := len(data)
		if i := bytes.IndexByte(data[offset:], '\n'); i >= 0 {
			lineEnd = offset + i + 1
		}
		line := bytes.TrimRight(data[offset:lineEnd], "\r\n")
		offset = lineEnd

		body := bytes.TrimSpace(line)
		if len(body) == 0 || body[0] == '#' || body[0] == ';' {
			continue
		}

		if body[0] == '[' {
			// go-ini takes the section name as the bytes between the first '['
			// and the *last* ']' without trimming, so "[ Peer ]" is a section
			// named " Peer ", not a Peer section. Ordinals must match
			// wireproxy's, because they appear in error messages.
			closeIdx := bytes.LastIndexByte(body, ']')
			if closeIdx < 0 {
				// go-ini rejects the file as an unclosed section.
				inPeer = false
				continue
			}
			inPeer = strings.EqualFold(string(body[1:closeIdx]), "Peer")
			if inPeer {
				peer++
			}
			continue
		}
		if !inPeer {
			continue
		}

		// go-ini splits a key at the first '=' or ':' (default
		// KeyValueDelimiters), so "Endpoint: host:port" is valid too.
		delim := bytes.IndexAny(line, "=:")
		if delim < 0 {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(string(line[:delim])), "Endpoint") {
			continue
		}

		c, ok := parseEndpointValue(line[delim+1:], lineStart+delim+1)
		if !ok {
			// Let wireproxy report a malformed Endpoint; it has the message.
			continue
		}
		if _, err := netip.ParseAddr(c.host); err == nil {
			continue // already an IP literal: nothing to resolve
		}
		c.peer = peer
		candidates = append(candidates, c)
	}

	return candidates
}

// parseEndpointValue parses the text after the key delimiter into a candidate
// whose start/end are absolute byte offsets in the file. base is the offset just
// past the delimiter. It returns false when the value is not a plain
// "host:port", so the caller can leave the line to wireproxy's own validation
// instead of failing the load with a nonsense hostname.
func parseEndpointValue(raw []byte, base int) (endpointCandidate, bool) {
	var c endpointCandidate

	// go-ini (as wireproxy configures it) cuts the value at the first comment
	// character, then trims it. A backtick-quoted value skips that cut, but the
	// defensive check below drops anything the difference could misparse.
	if i := bytes.IndexAny(raw, "#;"); i >= 0 {
		raw = raw[:i]
	}
	start := len(raw) - len(bytes.TrimLeft(raw, " \t"))
	stop := len(bytes.TrimRight(raw, " \t"))
	if stop <= start {
		return c, false
	}

	value := string(raw[start:stop])
	inner := value
	if len(value) >= 2 && isQuoteByte(value[0]) && value[len(value)-1] == value[0] &&
		strings.IndexByte(value[1:], value[0]) == len(value)-2 {
		// start/stop keep the quotes: the rewriter re-emits the quote around
		// the address, so advancing them here would duplicate it.
		c.quote = value[0]
		inner = value[1 : len(value)-1]
	}
	if strings.ContainsAny(inner, "\"'` \t") {
		// Not something a host may contain. Leave it to wireproxy, which will
		// report it against the original line.
		return c, false
	}

	host, port, err := net.SplitHostPort(inner)
	if err != nil {
		return c, false
	}

	c.host = host
	c.port = port
	c.start = base + start
	c.end = base + stop
	return c, true
}

// isQuoteByte reports whether b is a quote character go-ini understands around
// an INI value.
func isQuoteByte(b byte) bool {
	return b == '"' || b == '\'' || b == '`'
}

// createTemp creates the temporary config file. It is a package variable so
// tests can observe whether a rewrite happened at all and can force the failure
// branches below.
var createTemp = os.CreateTemp

// writeTemp writes the config bytes to the temporary file. It is a package
// variable so a test can force a write failure without a full or broken disk.
var writeTemp = func(f *os.File, data []byte) error {
	_, err := f.Write(data)
	return err
}

// writeTempConfig writes data to a fresh, 0600 file in path's directory and
// returns its name. It must be in the same directory as the original so a
// relative WGConfig key still resolves; it holds the private key, so it is
// created owner-only and removed on every path.
func writeTempConfig(path string, data []byte) (string, error) {
	f, err := createTemp(filepath.Dir(path), "."+filepath.Base(path)+".resolved-*")
	if err != nil {
		return "", fmt.Errorf("create temporary config next to %s: %w", path, err)
	}
	name := f.Name()
	// CreateTemp already uses 0600; set it explicitly because this file holds
	// the private key and the requirement must not depend on a stdlib default.
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		_ = os.Remove(name)
		return "", fmt.Errorf("set mode on temporary config %s: %w", name, err)
	}
	if err := writeTemp(f, data); err != nil {
		_ = f.Close()
		_ = os.Remove(name)
		return "", fmt.Errorf("write temporary config %s: %w", name, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(name)
		return "", fmt.Errorf("close temporary config %s: %w", name, err)
	}
	return name, nil
}
