package debug

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMissingSessionStateIsVisibleAndFailsCleanup(t *testing.T) {
	root, err := OpenStateRoot(filepath.Join(t.TempDir(), "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root.Path, "session.missing")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	sessions, err := root.ListSessions()
	if err != nil || len(sessions) != 1 {
		t.Fatalf("missing state hidden: sessions=%d err=%v", len(sessions), err)
	}
	if err := CleanupSession(root, dir, CleanupOptions{}); err == nil {
		t.Fatal("cleanup silently accepted missing state")
	}
}

func TestOpenStateRootValidation(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(t *testing.T) string
		wantErr bool
	}{
		{
			name:  "creates with 0700 when absent",
			setup: func(t *testing.T) string { return filepath.Join(t.TempDir(), "sub", "root") },
		},
		{
			name: "accepts an existing 0700 dir",
			setup: func(t *testing.T) string {
				p := filepath.Join(t.TempDir(), "root")
				os.MkdirAll(p, 0700)
				return p
			},
		},
		{
			name: "rejects symlink",
			setup: func(t *testing.T) string {
				base := t.TempDir()
				real := filepath.Join(base, "real")
				os.MkdirAll(real, 0700)
				link := filepath.Join(base, "link")
				os.Symlink(real, link)
				return link
			},
			wantErr: true,
		},
		{
			name: "rejects group-writable",
			setup: func(t *testing.T) string {
				p := filepath.Join(t.TempDir(), "wide")
				os.MkdirAll(p, 0700)
				os.Chmod(p, 0770)
				return p
			},
			wantErr: true,
		},
		{
			name: "rejects a file",
			setup: func(t *testing.T) string {
				p := filepath.Join(t.TempDir(), "file")
				os.WriteFile(p, []byte("x"), 0600)
				return p
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := tt.setup(t)
			_, err := OpenStateRoot(path)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestSessionSaveLoadRoundtrip(t *testing.T) {
	root, err := OpenStateRoot(filepath.Join(t.TempDir(), "s"))
	if err != nil {
		t.Fatal(err)
	}
	in := &SessionState{
		OwnerPID: 42, OwnerStart: "12345", Switch: "sw", SwitchNetns: "sw_ns",
		SwitchNsIno: "5:6", DebugNS: "dbg", DebugNSIno: "7:8", Port: 9,
		TapName: "sw-t9", Ifindex: 12, InnerIP: "169.254.0.21",
		FloatingIP: "100.100.1.9", OffloadDirty: true, Phase: "running",
	}
	sess, err := root.NewSessionDir(in)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Remove()

	out, err := sess.Load()
	if err != nil {
		t.Fatal(err)
	}
	if out.OwnerPID != in.OwnerPID || out.Port != in.Port || out.TapName != in.TapName ||
		out.SwitchNsIno != in.SwitchNsIno || out.Ifindex != in.Ifindex || out.Phase != in.Phase {
		t.Fatalf("roundtrip mismatch: %+v vs %+v", out, in)
	}
	if out.Version != SessionStateVersion {
		t.Fatalf("version = %d", out.Version)
	}
	sessions, err := root.ListSessions()
	if err != nil || len(sessions) != 1 {
		t.Fatalf("ListSessions = %d sessions, %v", len(sessions), err)
	}
}

func TestSessionLockMutualExclusion(t *testing.T) {
	root, _ := OpenStateRoot(filepath.Join(t.TempDir(), "s"))
	sess, err := root.NewSessionDir(&SessionState{Phase: "preparing"})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Remove()

	if err := sess.LockExclusive(time.Second); err != nil {
		t.Fatalf("first lock: %v", err)
	}

	other, err := root.OpenSession(sess.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := other.LockExclusive(150 * time.Millisecond); err == nil {
		t.Fatal("second locker must not acquire while held")
	}

	sess.Unlock()
	if err := other.LockExclusive(time.Second); err != nil {
		t.Fatalf("acquire after unlock: %v", err)
	}
}

// TestAtomicWriteLeavesNoTmp verifies no temp files linger after saves.
func TestAtomicWriteLeavesNoTmp(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "f")
	if err := atomicWriteFile(target, []byte("v1"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := atomicWriteFile(target, []byte("v2"), 0600); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(target)
	if string(data) != "v2" {
		t.Fatalf("content = %q", data)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("leftover temp files: %d entries", len(entries))
	}
}
