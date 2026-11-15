package device

import (
	"errors"
	"strings"
	"testing"
)

func TestCgroupV2Path(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
		ok      bool
	}{
		{
			name:    "android cgroup v1 plus v2",
			content: "4:memory:/apps\n3:cpuset:/top-app\n2:cpu:/top-app\n1:blkio:/\n0::/uid_10303/pid_23569\n",
			want:    "/uid_10303/pid_23569",
			ok:      true,
		},
		{
			name:    "boot-started process is at the root",
			content: "4:memory:/apps\n0::/\n",
			want:    "/",
			ok:      true,
		},
		{
			name:    "v1 only (no unified hierarchy)",
			content: "4:memory:/apps\n3:cpuset:/top-app\n1:blkio:/\n",
			ok:      false,
		},
		{
			name:    "empty file",
			content: "",
			ok:      false,
		},
		{
			name:    "blank lines and spaces",
			content: "\n  0::/uid_10123  \n",
			want:    "/uid_10123",
			ok:      true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := cgroupV2Path(tt.content)
			if ok != tt.ok || got != tt.want {
				t.Fatalf("cgroupV2Path(%q) = (%q, %v), want (%q, %v)", tt.content, got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestIsAppCgroup(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{"/uid_10303/pid_23569", true},
		{"/uid_10123", true},
		{"/uid_0/pid_1", true},
		{"/", false},
		{"", false},
		{"/system.slice/wg-user.service", false},
		{"/uid_abc", false},
		{"/uid_", false},
		{"/uid", false},
		{"/uid_10123x", false},
		{"uid_10123", false}, // no leading slash: not a cgroup path we recognise
		{"/apps/uid_10123/pid_9", false},
	}
	for _, tt := range tests {
		if got := isAppCgroup(tt.path); got != tt.want {
			t.Errorf("isAppCgroup(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

func TestEscapeAppCgroupMovesOut(t *testing.T) {
	var movedPid int
	reads := 0
	ops := cgroupOps{
		read: func() ([]byte, error) {
			reads++
			if reads == 1 {
				return []byte("4:memory:/apps\n0::/uid_10303/pid_23569\n"), nil
			}
			return []byte("4:memory:/apps\n0::/\n"), nil
		},
		move: func(pid int) error {
			movedPid = pid
			return nil
		},
	}

	got, err := escapeAppCgroup(1729, ops)
	if err != nil {
		t.Fatalf("escapeAppCgroup: %v", err)
	}
	if !got.Moved || got.From != "/uid_10303/pid_23569" || got.To != "/" {
		t.Fatalf("escapeAppCgroup = %+v, want moved from /uid_10303/pid_23569 to /", got)
	}
	if movedPid != 1729 {
		t.Fatalf("moved pid = %d, want 1729", movedPid)
	}
	if reads != 2 {
		t.Fatalf("reads = %d, want 2 (before and after the move)", reads)
	}
}

func TestEscapeAppCgroupLeavesOtherCgroupsAlone(t *testing.T) {
	for _, content := range []string{
		"0::/\n",                         // boot-started
		"0::/system.slice/wg-user\n",     // some other manager owns it
		"3:cpuset:/top-app\n1:blkio:/\n", // v1 only
		"",                               // no cgroups at all
	} {
		moved := false
		ops := cgroupOps{
			read: func() ([]byte, error) { return []byte(content), nil },
			move: func(int) error { moved = true; return nil },
		}
		got, err := escapeAppCgroup(1, ops)
		if err != nil {
			t.Errorf("content %q: unexpected error: %v", content, err)
		}
		if got.Moved || moved {
			t.Errorf("content %q: moved (result %+v), want a no-op", content, got)
		}
	}
}

func TestEscapeAppCgroupUnreadableCgroupsIsNotAnError(t *testing.T) {
	ops := cgroupOps{
		read: func() ([]byte, error) { return nil, errors.New("permission denied") },
		move: func(int) error { t.Fatal("must not move"); return nil },
	}
	got, err := escapeAppCgroup(1, ops)
	if err != nil || got.Moved {
		t.Fatalf("escapeAppCgroup = (%+v, %v), want a silent no-op", got, err)
	}
}

func TestEscapeAppCgroupReportsMoveFailure(t *testing.T) {
	ops := cgroupOps{
		read: func() ([]byte, error) { return []byte("0::/uid_10303/pid_1\n"), nil },
		move: func(int) error { return errors.New("read-only file system") },
	}
	got, err := escapeAppCgroup(1, ops)
	if err == nil || !strings.Contains(err.Error(), "read-only file system") {
		t.Fatalf("escapeAppCgroup error = %v, want the write failure", err)
	}
	if got.Moved {
		t.Fatalf("Moved = true after a failed write")
	}
}

func TestEscapeAppCgroupFailsWhenTheMoveDidNotTake(t *testing.T) {
	// A write that "succeeds" but leaves the process in the app cgroup (a
	// kernel without the v2 freezer, or a silently ignored write) must not be
	// reported as success: the daemon would stay freezable while claiming
	// otherwise.
	ops := cgroupOps{
		read: func() ([]byte, error) { return []byte("0::/uid_10303/pid_1\n"), nil },
		move: func(int) error { return nil },
	}
	got, err := escapeAppCgroup(1, ops)
	if err == nil || !strings.Contains(err.Error(), "still in") {
		t.Fatalf("escapeAppCgroup error = %v, want a 'still in' failure", err)
	}
	if got.Moved {
		t.Fatalf("Moved = true although the process did not leave the app cgroup")
	}
}

func TestEscapeAppCgroupFailsWhenCgroupV2Disappears(t *testing.T) {
	reads := 0
	ops := cgroupOps{
		read: func() ([]byte, error) {
			reads++
			if reads == 1 {
				return []byte("0::/uid_10303/pid_1\n"), nil
			}
			return []byte("4:memory:/apps\n"), nil
		},
		move: func(int) error { return nil },
	}
	got, err := escapeAppCgroup(1, ops)
	if err == nil || !strings.Contains(err.Error(), "no cgroup v2 entry") {
		t.Fatalf("escapeAppCgroup error = %v, want a missing-hierarchy failure", err)
	}
	if got.Moved {
		t.Fatalf("Moved = true without a verified destination")
	}
}
