// Package state reads and writes wg-user's runtime state file: the small
// key=value snapshot the daemon refreshes on every transport change so that
// "wg-user status" and the WebUI can report state without parsing logs.
//
// The file is a contract with shell readers, which parse it one key per line
// (for example sed -n 's/^transport=//p'). Write emits exactly these keys, in
// this order, with no spaces around "=":
//
//	pid=1234
//	transport=wstunnel
//	transport_state=connected
//	transport_detail=wss://example.com:443
//	updated=1763000000
//
// transport is a [Transport] mode from the configuration: "off" or "wstunnel".
// transport_state mirrors the wstunnel client: one of "connected",
// "connecting", "reconnecting" or "closed". When transport is "off" there is
// nothing to connect, so transport_state is "off" too. transport_detail is
// free-form, normally the wstunnel URL or the direct endpoint; it may contain
// ":" and "/" but never a newline (Write rejects one). updated is Unix seconds.
//
// The allowed values are documented here rather than imported from config and
// wstunnel: this package depends only on the standard library. Read does not
// validate them, so a future mode or state does not break the reader.
package state

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Keys of the state file. They are literal and lowercase because shell
// readers match them exactly; changing one is a format change.
const (
	keyPID             = "pid"
	keyTransport       = "transport"
	keyTransportState  = "transport_state"
	keyTransportDetail = "transport_detail"
	keyUpdated         = "updated"
)

// State is the daemon's runtime state as stored in the state file.
type State struct {
	// PID is the daemon's process ID. Read requires it; a file without a pid
	// is not a state file.
	PID int
	// Transport is the active transport mode: "off" or "wstunnel".
	Transport string
	// TransportState is the transport's connection state: "connected",
	// "connecting", "reconnecting" or "closed" for wstunnel, plus "off" when
	// the transport is disabled (Transport is then "off" too).
	TransportState string
	// TransportDetail is a human-readable detail for status output, normally
	// the wstunnel URL or the direct endpoint. It must not contain a newline.
	TransportDetail string
	// Updated is when the daemon last wrote the file, at second precision
	// because the file carries Unix seconds.
	Updated time.Time
}

// Write atomically replaces the state file at path with s.
//
// It writes path+".tmp" in the same directory, sets its mode to 0644 and
// renames it over path, so a concurrent Read sees either the old file or the
// new one, never a partial write. The temp file is removed on every failure
// path.
func Write(path string, s State) error {
	// A newline in the detail would forge an extra line (and key) in the file.
	if strings.ContainsAny(s.TransportDetail, "\r\n") {
		return fmt.Errorf("state: %s: transport_detail contains a newline", path)
	}

	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("state: %s: %w", path, err)
	}
	// Remove the temp file on every failure path. After a successful rename
	// the name is gone and Remove fails harmlessly.
	defer os.Remove(tmp)

	_, err = fmt.Fprintf(f, "%s=%d\n%s=%s\n%s=%s\n%s=%s\n%s=%d\n",
		keyPID, s.PID,
		keyTransport, s.Transport,
		keyTransportState, s.TransportState,
		keyTransportDetail, s.TransportDetail,
		keyUpdated, s.Updated.Unix())
	if err != nil {
		f.Close()
		return fmt.Errorf("state: %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("state: %s: %w", path, err)
	}
	// OpenFile's mode only applies to a newly created file, so a stale temp
	// file from an earlier crash could still be 0600.
	if err := os.Chmod(tmp, 0o644); err != nil {
		return fmt.Errorf("state: %s: %w", path, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("state: %s: %w", path, err)
	}
	return nil
}

// Read parses the state file at path. Unknown keys and lines without "=" are
// ignored, so the file can grow new keys without breaking older readers. A
// file without a pid, or with an unparsable pid or updated, is an error.
func Read(path string) (State, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return State{}, fmt.Errorf("state: %s: %w", path, err)
	}

	var (
		s       State
		havePID bool
	)
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSuffix(line, "\r")
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			// Blank lines and anything that is not key=value.
			continue
		}
		switch key {
		case keyPID:
			pid, err := strconv.Atoi(value)
			if err != nil {
				return State{}, fmt.Errorf("state: %s: invalid pid %q", path, value)
			}
			s.PID = pid
			havePID = true
		case keyTransport:
			s.Transport = value
		case keyTransportState:
			s.TransportState = value
		case keyTransportDetail:
			s.TransportDetail = value
		case keyUpdated:
			sec, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return State{}, fmt.Errorf("state: %s: invalid updated %q", path, value)
			}
			s.Updated = time.Unix(sec, 0)
		}
	}
	if !havePID {
		return State{}, fmt.Errorf("state: %s: missing pid", path)
	}
	return s, nil
}
