package debug

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"golang.org/x/sys/unix"
)

// Session state lives under /run/connector-debug/session.<random>/.
// The Go and Bash tools use separate roots because their on-disk state
// formats and recovery procedures differ.

const (
	// DefaultStateRoot is where recovery records are kept.
	DefaultStateRoot = "/run/connector-debug"
	// StateFileName is the atomically replaced session state file.
	StateFileName = "state.json"
	// LockFileName is never replaced while a session exists; flock target.
	LockFileName = "session.lock"
	// AttachLockName is held by the attach worker across parent SIGKILL.
	AttachLockName = "attach.lock"
	// AttachJSONName / AttachRCName / AttachErrName record the attach receipt.
	AttachJSONName = "attach.json"
	AttachRCName   = "attach.rc"
	AttachErrName  = "attach.err"
	// OffloadSnapshotName persists the pre-modification feature state.
	OffloadSnapshotName = "offload.json"
	// OwnershipLostName is the sticky marker that stops all future automatic
	// mutation of the pool TAP and its slot.
	OwnershipLostName = "ownership_lost"
	// SessionStateVersion is the on-disk format version.
	SessionStateVersion = 1
)

// SessionState is the durable recovery record for one debugging session.
type SessionState struct {
	Version int `json:"version"`

	OwnerPID   int    `json:"owner_pid"`
	OwnerStart string `json:"owner_start"`
	CreatedAt  int64  `json:"created_at"`

	Switch       string `json:"switch"`
	SwitchNetns  string `json:"switch_netns"`
	SwitchNsIno  string `json:"switch_ns_ino,omitempty"`
	DebugNS      string `json:"debug_ns,omitempty"`
	DebugNSIno   string `json:"debug_ns_ino,omitempty"`
	PendingPort  int    `json:"pending_port,omitempty"`
	Port         int    `json:"port,omitempty"`
	TapName      string `json:"tap_name,omitempty"`
	Ifindex      uint32 `json:"ifindex,omitempty"`
	InnerIP      string `json:"inner_ip,omitempty"`
	FloatingIP   string `json:"floating_ip,omitempty"`
	OffloadDirty bool   `json:"offload_dirty"`
	ChildPID     int    `json:"child_pid,omitempty"`
	ChildStart   string `json:"child_start,omitempty"`
	Phase        string `json:"phase"` // preparing|allocated|running
}

// OwnPortAlive reports whether the recorded owner process still runs.
func (s *SessionState) OwnPortAlive() bool {
	if s.OwnerPID <= 0 || s.OwnerStart == "" {
		return false
	}
	return ProcAlive(defaultProcRoot, strconv.Itoa(s.OwnerPID), s.OwnerStart)
}

// StateRoot holds per-session recovery state and validates its own layout.
type StateRoot struct {
	Path string
}

// OpenStateRoot validates (or creates) the state root: a real directory,
// owned by the caller, not writable by group/other — the same contract the
// Bash tool enforces.
func OpenStateRoot(path string) (*StateRoot, error) {
	info, err := os.Lstat(path)
	switch {
	case err == nil:
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return nil, fmt.Errorf("state root %s must be a directory, not a symlink", path)
		}
		if !filepath.IsAbs(path) {
			return nil, fmt.Errorf("state root must be absolute: %s", path)
		}
		if stat, ok := info.Sys().(*unix.Stat_t); ok && int(stat.Uid) != os.Getuid() {
			return nil, fmt.Errorf("state root %s must be owned by the caller", path)
		}
		if info.Mode().Perm()&022 != 0 {
			return nil, fmt.Errorf("state root %s must not be group/other writable", path)
		}
	case os.IsNotExist(err):
		if err := os.MkdirAll(path, 0700); err != nil {
			return nil, err
		}
	default:
		return nil, err
	}
	return &StateRoot{Path: path}, nil
}

// Session is one session directory with its lock.
type Session struct {
	Root     *StateRoot
	Dir      string
	lockFD   int
	lockHeld bool
}

// NewSessionDir creates a fresh session directory (with lock file) and
// records the initial preparing state.
func (r *StateRoot) NewSessionDir(state *SessionState) (*Session, error) {
	dir, err := os.MkdirTemp(r.Path, "session.")
	if err != nil {
		return nil, err
	}
	s := &Session{Root: r, Dir: dir}
	if err := s.ensureLockFile(); err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	if err := s.Save(state); err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	return s, nil
}

func (s *Session) ensureLockFile() error {
	fd, err := unix.Open(filepath.Join(s.Dir, LockFileName), unix.O_RDONLY|unix.O_CREAT|unix.O_CLOEXEC, 0600)
	if err != nil {
		return err
	}
	s.lockFD = fd
	return nil
}

// StatePath / LockPath / AttachLockPath are the well-known session files.
func (s *Session) StatePath() string         { return filepath.Join(s.Dir, StateFileName) }
func (s *Session) LockPath() string          { return filepath.Join(s.Dir, LockFileName) }
func (s *Session) AttachLockPath() string    { return filepath.Join(s.Dir, AttachLockName) }
func (s *Session) AttachJSONPath() string    { return filepath.Join(s.Dir, AttachJSONName) }
func (s *Session) AttachRCPath() string      { return filepath.Join(s.Dir, AttachRCName) }
func (s *Session) AttachErrPath() string     { return filepath.Join(s.Dir, AttachErrName) }
func (s *Session) OffloadPath() string       { return filepath.Join(s.Dir, OffloadSnapshotName) }
func (s *Session) OwnershipLostPath() string { return filepath.Join(s.Dir, OwnershipLostName) }

// Save atomically persists the session state (tmp + fsync + rename + dir fsync).
func (s *Session) Save(state *SessionState) error {
	state.Version = SessionStateVersion
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteFile(s.StatePath(), data, 0600)
}

// Load reads the session state.
func (s *Session) Load() (*SessionState, error) {
	data, err := os.ReadFile(s.StatePath())
	if err != nil {
		return nil, err
	}
	var st SessionState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// LockExclusive takes the session lock with a bounded wait.
func (s *Session) LockExclusive(timeout time.Duration) error {
	if s.lockFD <= 0 {
		if err := s.ensureLockFile(); err != nil {
			return err
		}
	}
	deadline := time.Now().Add(timeout)
	for {
		err := unix.Flock(s.lockFD, unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			s.lockHeld = true
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("session lock busy: %w", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Unlock releases the session lock.
func (s *Session) Unlock() {
	if s.lockFD > 0 && s.lockHeld {
		_ = unix.Flock(s.lockFD, unix.LOCK_UN)
		s.lockHeld = false
	}
}

// Close releases the lock fd.
func (s *Session) Close() {
	s.Unlock()
	if s.lockFD > 0 {
		_ = unix.Close(s.lockFD)
		s.lockFD = 0
	}
}

// ListSessions enumerates session directories in the root.
func (r *StateRoot) ListSessions() ([]*Session, error) {
	entries, err := os.ReadDir(r.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []*Session
	for _, e := range entries {
		if !e.IsDir() || !hasPrefix(e.Name(), "session.") {
			continue
		}
		dir := filepath.Join(r.Path, e.Name())
		out = append(out, &Session{Root: r, Dir: dir})
	}
	return out, nil
}

// OpenSession opens an existing session directory by path.
func (r *StateRoot) OpenSession(dir string) (*Session, error) {
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("session path is not a directory: %s", dir)
	}
	return &Session{Root: r, Dir: dir}, nil
}

// Remove deletes the session directory (only after full successful cleanup).
func (s *Session) Remove() error {
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.Name() == LockFileName {
			continue // still held; removed by RemoveAll below anyway
		}
		if err := os.RemoveAll(filepath.Join(s.Dir, e.Name())); err != nil {
			return err
		}
	}
	s.Close()
	return os.RemoveAll(s.Dir)
}

func hasPrefix(s, p string) bool { return len(s) >= len(p) && s[:len(p)] == p }

// atomicWriteFile writes data to path atomically: tmp in the same directory,
// fsync, rename, fsync of the directory.
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		if tmpName != "" {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	tmpName = ""
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
