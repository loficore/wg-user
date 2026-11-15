// Package config loads a wg-user configuration file: one wireproxy-style INI
// file that carries both the sections wireproxy owns ([Interface], [Peer],
// [Socks5], …) and the extra [Transport] section that selects how WireGuard
// datagrams reach the server.
package config

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/go-ini/ini"
	"github.com/windtf/wireproxy"
)

// Transport modes understood by the [Transport] Mode key.
const (
	// ModeOff hands WG datagrams straight to the kernel, which sends them to
	// [Peer] Endpoint. This is the 0.2.0 behaviour and the default.
	ModeOff = "off"
	// ModeWstunnel tunnels WG datagrams over a WebSocket to the wstunnel
	// server, which forwards them to [Peer] Endpoint.
	ModeWstunnel = "wstunnel"
)

const (
	transportSection = "Transport"
	// defaultPath is the WS upgrade prefix used when Path is not set.
	defaultPath = "v1"
	// defaultKeepalive is the WS ping interval used when Keepalive is not set.
	defaultKeepalive = 25 * time.Second
)

// Transport is the parsed [Transport] section.
type Transport struct {
	// Mode is ModeOff or ModeWstunnel.
	Mode string
	// URL is the wstunnel server URL (ws:// or wss://). Empty when ModeOff.
	URL string
	// Path is the WS upgrade prefix as a bare URL segment, without slashes:
	// the client requests "/<Path>/events". Never empty.
	Path string
	// Username and Password are the optional HTTP basic-auth credentials.
	Username string
	Password string
	// Insecure skips TLS certificate verification for the WebSocket.
	Insecure bool
	// Keepalive is the WS ping interval; zero disables pings.
	Keepalive time.Duration
}

// Config is a fully parsed and validated configuration file.
type Config struct {
	// Path is the file path exactly as passed to Load.
	Path string
	// WireProxy holds the sections parsed by the upstream wireproxy parser.
	WireProxy *wireproxy.Configuration
	// Transport holds the [Transport] section parsed by wg-user itself.
	Transport Transport
}

// Load reads and validates the configuration file at path. Every error it
// returns is prefixed with "config <path>:" so callers can print one line.
func Load(path string) (*Config, error) {
	c, err := load(path)
	if err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	return c, nil
}

func load(path string) (*Config, error) {
	// Insensitive matches what wireproxy.ParseConfig asks for, so "[transport]"
	// and "mode" are accepted too.
	file, err := ini.LoadSources(ini.LoadOptions{Insensitive: true}, path)
	if err != nil {
		return nil, err
	}

	// wireproxy only looks up the section names it knows, so it ignores
	// [Transport] and the same file stays readable by 0.2.0 binaries.
	wp, err := wireproxy.ParseConfig(path)
	if err != nil {
		return nil, err
	}

	transport, err := parseTransport(file.Section(transportSection), wp)
	if err != nil {
		return nil, err
	}

	return &Config{Path: path, WireProxy: wp, Transport: *transport}, nil
}

// parseTransport reads the [Transport] section. section is an empty section
// when the file has none, which means ModeOff with the documented defaults.
//
// Validation order is fixed so the first error a user sees is the most
// actionable one: Mode → URL (wstunnel) → Path → Keepalive → Endpoint (every
// mode) → peer count (wstunnel).
func parseTransport(section *ini.Section, wp *wireproxy.Configuration) (*Transport, error) {
	tr := &Transport{
		Mode:      ModeOff,
		Path:      defaultPath,
		Keepalive: defaultKeepalive,
	}

	// Mode. A missing or empty value means ModeOff.
	rawMode := stringValue(section, "Mode")
	if rawMode != "" {
		tr.Mode = strings.ToLower(rawMode)
	}
	switch tr.Mode {
	case ModeOff, ModeWstunnel:
	default:
		return nil, fmt.Errorf("unsupported transport mode %q (want %q or %q)", rawMode, ModeOff, ModeWstunnel)
	}

	// URL, only meaningful for wstunnel.
	if tr.Mode == ModeWstunnel {
		rawURL := stringValue(section, "URL")
		if rawURL == "" {
			return nil, fmt.Errorf("[Transport] URL is required when Mode = %q", ModeWstunnel)
		}
		parsed, err := url.Parse(rawURL)
		if err != nil {
			return nil, fmt.Errorf("[Transport] URL is not a valid URL: %w", err)
		}
		switch strings.ToLower(parsed.Scheme) {
		case "ws", "wss":
		default:
			return nil, fmt.Errorf("[Transport] URL scheme must be ws or wss, got %q", parsed.Scheme)
		}
		tr.URL = rawURL
	}

	// Path: normalise to a bare segment so "<base>/<Path>/events" never
	// produces a double slash. An empty or all-slash value falls back to the
	// default.
	tr.Path = strings.Trim(stringValue(section, "Path"), "/")
	if tr.Path == "" {
		tr.Path = defaultPath
	}

	// Keepalive in whole seconds; 0 disables, negative is a config error.
	if key, err := section.GetKey("Keepalive"); err == nil {
		secs, convErr := key.Int()
		if convErr != nil {
			return nil, fmt.Errorf("[Transport] Keepalive must be a whole number of seconds: %w", convErr)
		}
		if secs < 0 {
			return nil, fmt.Errorf("[Transport] Keepalive must not be negative, got %d", secs)
		}
		tr.Keepalive = time.Duration(secs) * time.Second
	}

	// Credentials are optional; the transport sends Authorization: Basic only
	// when both are present.
	tr.Username = stringValue(section, "Username")
	tr.Password = stringValue(section, "Password")

	// Insecure uses ini's boolean parsing, so only the spellings it accepts
	// are allowed.
	if key, err := section.GetKey("Insecure"); err == nil {
		insecure, convErr := key.Bool()
		if convErr != nil {
			return nil, fmt.Errorf("[Transport] Insecure must be a boolean: %w", convErr)
		}
		tr.Insecure = insecure
	}

	// Every [Peer] needs an Endpoint in every mode: off mode sends WG UDP
	// straight to it, wstunnel mode hands it to the server as the delivery
	// target. A peer without one can never handshake, so reject it here rather
	// than at run time.
	if err := validatePeerEndpoints(wp.Device.Peers); err != nil {
		return nil, err
	}

	if tr.Mode != ModeWstunnel {
		return tr, nil
	}

	// One transport can only target one peer.
	if n := len(wp.Device.Peers); n != 1 {
		return nil, fmt.Errorf("transport mode %q requires exactly one [Peer], got %d", ModeWstunnel, n)
	}

	return tr, nil
}

// validatePeerEndpoints requires a non-empty Endpoint on every peer, in every
// mode. wireproxy.ParseConfig already validates the host:port form (and that
// at least one [Peer] exists), so this only has to catch a missing value.
func validatePeerEndpoints(peers []wireproxy.PeerConfig) error {
	for i, peer := range peers {
		if peer.Endpoint == nil || *peer.Endpoint == "" {
			return fmt.Errorf("[Peer] Endpoint is required (peer %d)", i+1)
		}
	}
	return nil
}

// stringValue returns the value of name in section, or "" when the key or the
// whole section is absent. ini's Section.Key invents an empty key, so GetKey is
// used to tell "missing" from "set to an empty value".
func stringValue(section *ini.Section, name string) string {
	if key, err := section.GetKey(name); err == nil {
		return key.String()
	}
	return ""
}
