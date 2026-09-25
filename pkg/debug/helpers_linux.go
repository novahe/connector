package debug

import (
	"os"
	"path/filepath"
	"strconv"

	vnetns "github.com/vishvananda/netns"
	"golang.org/x/sys/unix"

	"github.com/kuasar-sandbox/connector/pkg/netns"
)

// helpers: flock primitives and netns-scoped ethtool entry points used by
// recovery and by the CLI flow.

func openLockFile(path string) (int, error) {
	return unix.Open(path, unix.O_RDONLY|unix.O_CREAT|unix.O_CLOEXEC, 0600)
}

func closeFD(fd int) error { return unix.Close(fd) }

func tryFlock(fd int) error { return unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB) }

func unlockFD(fd int) error { return unix.Flock(fd, unix.LOCK_UN) }

// SelfStarttime records this process's start time for ownership checks.
func SelfStarttime() string {
	start, err := ProcStarttime(defaultProcRoot, strconv.Itoa(os.Getpid()))
	if err != nil {
		return ""
	}
	return start
}

// ProcStarttimeOf wraps ProcStarttime for int pids.
func ProcStarttimeOf(pid int) (string, error) {
	return ProcStarttime(defaultProcRoot, strconv.Itoa(pid))
}

// inSwitchNetns runs f inside the named network namespace with the OS thread
// locked (required for correct netns-scoped ioctls from Go).
func inSwitchNetns(name string, f func() error) error {
	ns, err := netns.GetByName(name)
	if err != nil {
		return err
	}
	defer ns.Close()
	return ns.Do(f)
}

// The ethtool ioctls must execute in the namespace owning the netdev; these
// wrappers provide the ns-scoped variants the session flow needs.

func SuppressOffloadsInNS(nsName, tapName string) (*FeatureSnapshot, error) {
	var snap *FeatureSnapshot
	err := inSwitchNetns(nsName, func() error {
		var e error
		snap, e = SuppressOffloadsForBridge(tapName)
		return e
	})
	return snap, err
}

func SnapshotOffloadsInNS(nsName, tapName string) (*FeatureSnapshot, error) {
	var snap *FeatureSnapshot
	err := inSwitchNetns(nsName, func() error {
		var e error
		snap, e = SnapshotOffloads(tapName)
		return e
	})
	return snap, err
}

func ApplySuppressedOffloadsInNS(nsName, tapName string, snap *FeatureSnapshot) error {
	return inSwitchNetns(nsName, func() error {
		return ApplySuppressedOffloads(tapName, snap)
	})
}

func RestoreFeaturesInNS(nsName, tapName string, snap *FeatureSnapshot) error {
	return inSwitchNetns(nsName, func() error {
		return RestoreFeatures(tapName, snap, ethtoolRetries)
	})
}

// WriteFileAtomic is the exported atomic-write used by the attach worker to
// persist its receipt.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	return atomicWriteFile(path, data, perm)
}

// NSHandleFd opens the raw fd of a named netns (used by the exec helper).
func NSHandleFd(name string) (int, error) {
	return unix.Open(filepath.Join("/var/run/netns", name), unix.O_RDONLY|unix.O_CLOEXEC, 0)
}

var _ = vnetns.None // keep the import shaped for future helpers
