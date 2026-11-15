package device

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Android runs every app in a per-process cgroup under the cgroup v2 hierarchy,
// /uid_<uid>/pid_<pid>, so the framework can freeze the app when it is
// backgrounded. A process started from an app inherits that cgroup, and freezing
// it stops every thread: the pid stays alive, the pid file stays valid and
// run/state keeps its last value, but nothing runs. A daemon started from the
// WebUI (inside the KernelSU app) or from Termux therefore dies the moment the
// user switches away, which looks like "the transport is stuck connecting" and
// survives every restart.
//
// The root cgroup is not freezable, and it is where a boot-started daemon
// already lives: service.sh is executed by the root framework, not by an app.
// Moving there is not a privilege change — the daemon is already root — it only
// stops the app's freezer from owning it.

const (
	// cgroupSelfFile lists this process's cgroups, one "id:controllers:path"
	// line per hierarchy. The v2 hierarchy has an empty controller list.
	cgroupSelfFile = "/proc/self/cgroup"
	// cgroupV2Mount is the unified hierarchy. Writing a pid to cgroup.procs at
	// its root moves the whole thread group out of any subtree.
	cgroupV2Mount = "/sys/fs/cgroup"
)

// CgroupEscape is the outcome of EscapeAppCgroup.
type CgroupEscape struct {
	// Moved is true when the process left an app cgroup during this call.
	Moved bool
	// From is the cgroup path the process was in ("/uid_10303/pid_23569").
	From string
	// To is the path it is in afterwards ("/").
	To string
}

// EscapeAppCgroup moves the current process out of an Android per-app cgroup
// into the root cgroup, so the framework can no longer freeze it.
//
// It is a no-op (Moved false, nil error) when the process is already outside an
// app cgroup, or on a system without a cgroup v2 hierarchy — a daemon started
// by service.sh needs neither. Moving is idempotent, so calling it twice is
// harmless.
func EscapeAppCgroup() (CgroupEscape, error) {
	return escapeAppCgroup(os.Getpid(), cgroupOps{
		read: func() ([]byte, error) { return os.ReadFile(cgroupSelfFile) },
		move: func(pid int) error {
			// cgroup.procs takes the thread-group id and migrates every thread.
			procs := filepath.Join(cgroupV2Mount, "cgroup.procs")
			if err := os.WriteFile(procs, []byte(strconv.Itoa(pid)+"\n"), 0o644); err != nil {
				return fmt.Errorf("cgroup: write %s: %w", procs, err)
			}
			return nil
		},
	})
}

// cgroupOps makes escapeAppCgroup testable without a cgroup filesystem.
type cgroupOps struct {
	read func() ([]byte, error)
	move func(pid int) error
}

func escapeAppCgroup(pid int, ops cgroupOps) (CgroupEscape, error) {
	data, err := ops.read()
	if err != nil {
		// No cgroup visibility at all: nothing to escape from and nothing to
		// report, the daemon simply runs wherever it was started.
		return CgroupEscape{}, nil
	}
	from, ok := cgroupV2Path(string(data))
	if !ok || !isAppCgroup(from) {
		return CgroupEscape{From: from, To: from}, nil
	}

	if err := ops.move(pid); err != nil {
		return CgroupEscape{From: from}, err
	}

	// Confirm the move instead of trusting the write: a cgroup v1-only kernel
	// has no v2 root to write to, and a silently ignored write would leave the
	// daemon freezable while claiming otherwise.
	after, err := ops.read()
	if err != nil {
		return CgroupEscape{From: from}, fmt.Errorf("cgroup: re-read %s after moving: %w", cgroupSelfFile, err)
	}
	to, ok := cgroupV2Path(string(after))
	if !ok {
		return CgroupEscape{From: from}, fmt.Errorf("cgroup: %s has no cgroup v2 entry after moving out of %s", cgroupSelfFile, from)
	}
	if isAppCgroup(to) {
		return CgroupEscape{From: from, To: to},
			fmt.Errorf("cgroup: still in %s after writing %s", to, filepath.Join(cgroupV2Mount, "cgroup.procs"))
	}
	return CgroupEscape{Moved: true, From: from, To: to}, nil
}

// cgroupV2Path returns the path of this process in the cgroup v2 hierarchy: the
// line whose controller list is empty.
func cgroupV2Path(content string) (string, bool) {
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// "id:controllers:path"; only the last colon separates the path, so
		// split from the left with a limit and keep the remainder.
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 || parts[1] != "" {
			continue
		}
		return parts[2], true
	}
	return "", false
}

// isAppCgroup reports whether path is one of Android's per-app cgroups,
// /uid_<uid> or /uid_<uid>/pid_<pid>. Those are the ones the framework freezes;
// a boot-started process lives in "/" and a vendor service in some other
// subtree, and neither must be moved.
func isAppCgroup(path string) bool {
	rest, ok := strings.CutPrefix(path, "/")
	if !ok {
		return false
	}
	first, _, _ := strings.Cut(rest, "/")
	name, digits, ok := strings.Cut(first, "_")
	if !ok || name != "uid" || digits == "" {
		return false
	}
	_, err := strconv.ParseUint(digits, 10, 64)
	return err == nil
}
