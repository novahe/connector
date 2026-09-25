package debug

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// procscan walks /proc to answer the two identity questions the recovery
// protocol depends on: which processes hold a given TAP's fd, and which
// processes live inside a given network namespace. The proc root is
// injectable so unit tests can drive it against a fake tree.

const defaultProcRoot = "/proc"

// ProcStarttime returns field 22 of /proc/<pid>/stat (clock ticks since boot),
// robust against comm fields containing spaces or parentheses.
func ProcStarttime(procRoot, pid string) (string, error) {
	data, err := os.ReadFile(filepath.Join(procRoot, pid, "stat"))
	if err != nil {
		return "", err
	}
	idx := strings.LastIndexByte(string(data), ')')
	if idx < 0 || idx+2 > len(data) {
		return "", fmt.Errorf("malformed stat for pid %s", pid)
	}
	fields := strings.Fields(string(data[idx+2:]))
	// fields[0] is state; starttime is field 22 of the full stat, i.e. the
	// 20th field after "pid (comm)" => index 19 here.
	if len(fields) < 20 {
		return "", fmt.Errorf("short stat for pid %s", pid)
	}
	return fields[19], nil
}

// ProcAlive reports whether pid is running with the recorded start time and
// is not a zombie (a zombie no longer owns file descriptors or namespaces).
func ProcAlive(procRoot, pid, start string) bool {
	now, err := ProcStarttime(procRoot, pid)
	if err != nil || now != start {
		return false
	}
	data, err := os.ReadFile(filepath.Join(procRoot, pid, "stat"))
	if err != nil {
		return false
	}
	idx := strings.LastIndexByte(string(data), ')')
	if idx < 0 || idx+2 > len(data) {
		return false
	}
	state := data[idx+2]
	return state != 'Z' && state != 'X'
}

// StopProcess terminates a process identified by pid+start time with a
// TERM→KILL escalation; it returns an error only if the process is still
// alive after both signals.
func StopProcess(procRoot string, pid, start string, termWait, killWait time.Duration) error {
	p := atoi(pid)
	if p <= 0 {
		return fmt.Errorf("bad pid %q", pid)
	}
	fd, err := unix.PidfdOpen(p, 0)
	if err == unix.ESRCH {
		return nil
	}
	if err != nil {
		return fmt.Errorf("pidfd open %s: %w", pid, err)
	}
	defer unix.Close(fd)
	if !ProcAlive(procRoot, pid, start) {
		return nil
	}
	if err := unix.PidfdSendSignal(fd, unix.SIGTERM, nil, 0); err != nil && err != unix.ESRCH {
		return fmt.Errorf("signal TERM to %s: %w", pid, err)
	}
	if waitProcessGone(procRoot, pid, start, termWait) {
		return nil
	}
	if err := unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0); err != nil && err != unix.ESRCH {
		return fmt.Errorf("signal KILL to %s: %w", pid, err)
	}
	if waitProcessGone(procRoot, pid, start, killWait) {
		return nil
	}
	return fmt.Errorf("process %s did not exit", pid)
}

func waitProcessGone(procRoot, pid, start string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !ProcAlive(procRoot, pid, start) {
			return true
		}
		time.Sleep(25 * time.Millisecond)
	}
	return !ProcAlive(procRoot, pid, start)
}

func atoi(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return -1
	}
	return n
}

// ErrProcScanIncomplete marks a TAP holder scan that could not prove the
// device idle; callers must treat the port as not safely releasable.
var ErrProcScanIncomplete = fmt.Errorf("proc scan incomplete")

// TapHolder describes one process holding a TAP fd.
type TapHolder struct {
	PID   string
	Start string
}

// TapHolders returns every task holding an fd on the named TAP, discovered
// via the kernel's fdinfo `iff:` field (works for any holder, including
// multiqueue threads and VMMs, and survives netns migration of the holder).
// Transient process exits during the scan are tolerated; other read errors
// make the result unprovable and return ErrProcScanIncomplete.
func TapHolders(procRoot, tapName string) ([]TapHolder, error) {
	want := "iff:\t" + tapName + "\n"
	tasks, err := listTasks(procRoot)
	if err != nil {
		return nil, fmt.Errorf("%w: list tasks: %v", ErrProcScanIncomplete, err)
	}
	seen := map[string]bool{}
	var holders []TapHolder
	for _, task := range tasks {
		infos, err := os.ReadDir(filepath.Join(procRoot, task, "fdinfo"))
		if err != nil {
			if os.IsNotExist(err) {
				continue // task exited mid-scan
			}
			return nil, fmt.Errorf("%w: readdir %s: %v", ErrProcScanIncomplete, task, err)
		}
		for _, info := range infos {
			data, err := os.ReadFile(filepath.Join(procRoot, task, "fdinfo", info.Name()))
			if err != nil {
				if os.IsNotExist(err) {
					continue
				}
				return nil, fmt.Errorf("%w: read fdinfo: %v", ErrProcScanIncomplete, err)
			}
			if !strings.Contains(string(data), want) {
				continue
			}
			pid := taskPID(task)
			if seen[pid] {
				continue
			}
			seen[pid] = true
			start, _ := ProcStarttime(procRoot, pid)
			holders = append(holders, TapHolder{PID: pid, Start: start})
			break
		}
	}
	return holders, nil
}

// TapIdle reports whether no process holds the named TAP. An incomplete scan
// is reported as not idle.
func TapIdle(procRoot, tapName string) (bool, error) {
	holders, err := TapHolders(procRoot, tapName)
	if err != nil {
		return false, err
	}
	return len(holders) == 0, nil
}

// listTasks enumerates /proc/<pid>/task/<tid> paths (fdinfo of a non-leader
// thread is not visible via /proc/<pid>/fdinfo).
func listTasks(procRoot string) ([]string, error) {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return nil, err
	}
	var tasks []string
	for _, e := range entries {
		if !e.IsDir() || atoi(e.Name()) <= 0 {
			continue
		}
		pid := e.Name()
		threadDir := filepath.Join(procRoot, pid, "task")
		tids, err := os.ReadDir(threadDir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		for _, tid := range tids {
			tasks = append(tasks, filepath.Join(pid, "task", tid.Name()))
		}
	}
	return tasks, nil
}

func taskPID(task string) string {
	// task = <pid>/task/<tid>
	i := strings.IndexByte(task, '/')
	if i < 0 {
		return task
	}
	return task[:i]
}

// NetnsIno returns the "dev:ino" identity of a named network namespace.
// /var/run/netns entries are bind mounts of nsfs, not symlinks, so the
// identity comes from stat — the same value /proc/<pid>/ns/net stats to.
func NetnsIno(name string) (string, error) {
	var st syscall.Stat_t
	if err := syscall.Stat(filepath.Join("/var/run/netns", name), &st); err != nil {
		return "", err
	}
	return fmt.Sprintf("%d:%d", st.Dev, st.Ino), nil
}

// nsInoOfProc resolves the netns identity of a process via stat of its
// /proc/<pid>/ns/net node (same dev:ino format as NetnsIno).
func nsInoOfProc(procRoot, pid string) (string, error) {
	var st syscall.Stat_t
	if err := syscall.Stat(filepath.Join(procRoot, pid, "ns", "net"), &st); err != nil {
		return "", err
	}
	return fmt.Sprintf("%d:%d", st.Dev, st.Ino), nil
}

// KillNetnsProcesses terminates every live process whose network namespace
// matches the given "net:[ino]" identity. Used to reap background commands a
// user left inside the session-owned debug netns.
func KillNetnsProcesses(procRoot, nsIno string) error {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return err
	}
	var firstErr error
	for _, e := range entries {
		pid := e.Name()
		if !e.IsDir() || atoi(pid) <= 0 {
			continue
		}
		ino, err := nsInoOfProc(procRoot, pid)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			firstErr = fmt.Errorf("readlink ns of %s: %w", pid, err)
			continue
		}
		if ino != nsIno {
			continue
		}
		start, err := ProcStarttime(procRoot, pid)
		if err != nil {
			continue
		}
		if err := StopProcess(procRoot, pid, start, 500*time.Millisecond, 500*time.Millisecond); err != nil {
			firstErr = err
		}
	}
	return firstErr
}

// SignalProcess pins the target by pidfd before verifying its start time, so
// a reused PID cannot receive a delayed signal.
func SignalProcess(procRoot string, pid int, start string, sig unix.Signal) error {
	if pid <= 0 || start == "" {
		return fmt.Errorf("process identity missing")
	}
	fd, err := unix.PidfdOpen(pid, 0)
	if err == unix.ESRCH {
		return nil
	}
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	if !ProcAlive(procRoot, strconv.Itoa(pid), start) {
		return nil
	}
	err = unix.PidfdSendSignal(fd, sig, nil, 0)
	if err == unix.ESRCH {
		return nil
	}
	return err
}
