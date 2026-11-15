package state

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// sampleState is a fully populated State. The detail deliberately contains
// ":" and "/" because a wstunnel URL does, and the file must carry it intact.
func sampleState() State {
	return State{
		PID:             1234,
		Transport:       "wstunnel",
		TransportState:  "connected",
		TransportDetail: "wss://example.com:443",
		Updated:         time.Unix(1763000000, 0),
	}
}

// seedFile writes content to a fresh state file inside a temp dir and returns
// its path.
func seedFile(t *testing.T, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "state")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("seed state file: %v", err)
	}
	return path
}

// findTempFiles returns every "*.tmp" file under dir, recursively.
func findTempFiles(t *testing.T, dir string) []string {
	t.Helper()

	var found []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(p, ".tmp") {
			found = append(found, p)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	return found
}

func TestWriteRead_RoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state")
	want := sampleState()

	if err := Write(path, want); err != nil {
		t.Fatalf("Write() unexpected error: %v", err)
	}
	got, err := Read(path)
	if err != nil {
		t.Fatalf("Read() unexpected error: %v", err)
	}

	if got.PID != want.PID {
		t.Errorf("PID = %d, want %d", got.PID, want.PID)
	}
	if got.Transport != want.Transport {
		t.Errorf("Transport = %q, want %q", got.Transport, want.Transport)
	}
	if got.TransportState != want.TransportState {
		t.Errorf("TransportState = %q, want %q", got.TransportState, want.TransportState)
	}
	if got.TransportDetail != want.TransportDetail {
		t.Errorf("TransportDetail = %q, want %q", got.TransportDetail, want.TransportDetail)
	}
	// The file stores Unix seconds, so compare instants at that precision.
	if got.Updated.Unix() != want.Updated.Unix() {
		t.Errorf("Updated = %v (%d), want %d", got.Updated, got.Updated.Unix(), want.Updated.Unix())
	}
}

func TestWrite_Format(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state")

	if err := Write(path, sampleState()); err != nil {
		t.Fatalf("Write() unexpected error: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back state file: %v", err)
	}

	// The shell reader greps these literal keys, so the bytes are the contract.
	want := "pid=1234\n" +
		"transport=wstunnel\n" +
		"transport_state=connected\n" +
		"transport_detail=wss://example.com:443\n" +
		"updated=1763000000\n"
	if string(data) != want {
		t.Errorf("state file =\n%q\nwant\n%q", data, want)
	}
}

func TestWrite_IsAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state")

	if err := Write(path, sampleState()); err != nil {
		t.Fatalf("Write() unexpected error: %v", err)
	}

	if leftovers := findTempFiles(t, dir); len(leftovers) != 0 {
		t.Errorf("Write left temp files behind: %v", leftovers)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("Write did not produce %s: %v", path, err)
	}
}

func TestWrite_NoTempFileOnFailure(t *testing.T) {
	dir := t.TempDir()
	// The parent of the state file does not exist, so the temp file cannot be
	// created either: Write must fail without leaving anything in dir.
	path := filepath.Join(dir, "missing", "state")

	err := Write(path, sampleState())
	if err == nil {
		t.Fatal("Write() = nil error, want a failure for a missing directory")
	}
	if !strings.HasPrefix(err.Error(), "state:") {
		t.Errorf("error = %q, want a %q prefix", err, "state:")
	}

	leftovers := findTempFiles(t, dir)
	if len(leftovers) != 0 {
		t.Errorf("failed Write left temp files behind: %v", leftovers)
	}
}

func TestWrite_UsesMode0644(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state")

	if err := Write(path, sampleState()); err != nil {
		t.Fatalf("Write() unexpected error: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat state file: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o644 {
		t.Errorf("mode = %o, want 644", got)
	}
}

func TestWrite_OverwritesExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state")

	first := sampleState()
	if err := Write(path, first); err != nil {
		t.Fatalf("first Write() unexpected error: %v", err)
	}

	second := State{
		PID:             4321,
		Transport:       "off",
		TransportState:  "closed",
		TransportDetail: "",
		Updated:         time.Unix(1763000100, 0),
	}
	if err := Write(path, second); err != nil {
		t.Fatalf("second Write() unexpected error: %v", err)
	}

	got, err := Read(path)
	if err != nil {
		t.Fatalf("Read() unexpected error: %v", err)
	}
	if got.PID != second.PID || got.Transport != second.Transport || got.TransportState != second.TransportState {
		t.Errorf("state after overwrite = %+v, want %+v", got, second)
	}
	if got.Updated.Unix() != second.Updated.Unix() {
		t.Errorf("Updated = %d, want %d", got.Updated.Unix(), second.Updated.Unix())
	}
}

func TestWrite_RejectsNewlineInDetail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state")
	s := sampleState()
	s.TransportDetail = "wss://example.com:443\npid=666"

	err := Write(path, s)
	if err == nil {
		t.Fatal("Write() = nil error, want a rejection of a newline in TransportDetail")
	}
	if !strings.HasPrefix(err.Error(), "state:") {
		t.Errorf("error = %q, want a %q prefix", err, "state:")
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("Write wrote a state file despite rejecting the detail: %v", statErr)
	}
}

func TestRead_MissingFileReturnsError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state")

	_, err := Read(path)
	if err == nil {
		t.Fatal("Read() = nil error, want an error for a missing file")
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("error = %v, want it to wrap fs.ErrNotExist", err)
	}
	if !strings.HasPrefix(err.Error(), "state:") {
		t.Errorf("error = %q, want a %q prefix", err, "state:")
	}
}

func TestRead_MissingPIDReturnsError(t *testing.T) {
	path := seedFile(t, "transport=wstunnel\ntransport_state=connected\nupdated=1763000000\n")

	_, err := Read(path)
	if err == nil {
		t.Fatal("Read() = nil error, want an error for a missing pid")
	}
	want := "state: " + path + ": missing pid"
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err, want)
	}
}

func TestRead_IgnoresUnknownKeys(t *testing.T) {
	path := seedFile(t, "version=2\npid=1234\ntransport=off\nfuture_key=whatever\ntransport_state=closed\nunknown\nupdated=1763000000\n")

	got, err := Read(path)
	if err != nil {
		t.Fatalf("Read() unexpected error: %v", err)
	}
	if got.PID != 1234 {
		t.Errorf("PID = %d, want 1234", got.PID)
	}
	if got.Transport != "off" || got.TransportState != "closed" {
		t.Errorf("transport = %q/%q, want off/closed", got.Transport, got.TransportState)
	}
	if got.Updated.Unix() != 1763000000 {
		t.Errorf("Updated = %d, want 1763000000", got.Updated.Unix())
	}
}

func TestRead_ValueMayContainEquals(t *testing.T) {
	// Only the first "=" separates key from value, so a query string survives.
	path := seedFile(t, "pid=1234\ntransport=wstunnel\ntransport_detail=wss://example.com/events?token=a=b\nupdated=1763000000\n")

	got, err := Read(path)
	if err != nil {
		t.Fatalf("Read() unexpected error: %v", err)
	}
	if want := "wss://example.com/events?token=a=b"; got.TransportDetail != want {
		t.Errorf("TransportDetail = %q, want %q", got.TransportDetail, want)
	}
}

func TestRead_RejectsInvalidPID(t *testing.T) {
	path := seedFile(t, "pid=not-a-number\nupdated=1763000000\n")

	_, err := Read(path)
	if err == nil {
		t.Fatal("Read() = nil error, want an error for a non-numeric pid")
	}
	if !strings.HasPrefix(err.Error(), "state:") {
		t.Errorf("error = %q, want a %q prefix", err, "state:")
	}
}
