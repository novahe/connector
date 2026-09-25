package debug

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/vishvananda/netlink"
	vnetns "github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

// netns_setup wires the debug netns: create/delete the named namespace,
// configure the dbg0 TAP inside it, and manage the /etc/netns resolv.conf.

// CreateDebugNS creates the named network namespace and returns its
// "net:[ino]" identity for later ownership verification.
func CreateDebugNS(name string) (identity string, retErr error) {
	type result struct {
		identity string
		err      error
	}
	results := make(chan result, 1)
	restoreFailures := make(chan error, 1)
	go func() {
		identity, retErr := createDebugNSOnThread(name, restoreFailures)
		results <- result{identity, retErr}
	}()
	select {
	case r := <-results:
		return r.identity, r.err
	case err := <-restoreFailures:
		return "", err
	}
}

func createDebugNSOnThread(name string, restoreFailures chan<- error) (identity string, retErr error) {
	runtime.LockOSThread()
	restored := false
	defer func() {
		if restored {
			runtime.UnlockOSThread()
		}
	}()
	original, err := vnetns.Get()
	if err != nil {
		return "", err
	}
	defer original.Close()
	// NewNamed enters the new namespace on this OS thread. Restore even when
	// creation reports an error after unshare.
	defer func() {
		if err := vnetns.Set(original); err != nil {
			retErr = fmt.Errorf("restore original netns: %w (prior error: %v)", err, retErr)
			// Report the failure before discarding this locked OS thread.
			restoreFailures <- retErr
			runtime.Goexit()
		}
		restored = true
	}()
	created, err := vnetns.NewNamed(name)
	if err != nil {
		return "", fmt.Errorf("create netns %s: %w", name, err)
	}
	defer created.Close()
	return NetnsIno(name)
}

// DeleteDebugNS removes the named network namespace.
func DeleteDebugNS(name string) error {
	return vnetns.DeleteNamed(name)
}

// PristineDebugNS verifies the narrow crash window after NewNamed and before
// its inode is persisted. At that point the namespace has only lo and no
// processes. Anything else may be a recreated namespace and is left alone.
func PristineDebugNS(name, identity string) (bool, error) {
	if identity == "" {
		return false, fmt.Errorf("missing namespace identity")
	}
	fd, err := unix.Open(filepath.Join("/var/run/netns", name), unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return false, err
	}
	defer unix.Close(fd)
	var opened syscall.Stat_t
	if err := syscall.Fstat(fd, &opened); err != nil {
		return false, err
	}
	if fmt.Sprintf("%d:%d", opened.Dev, opened.Ino) != identity {
		return false, nil
	}
	handle, err := netlink.NewHandleAt(vnetns.NsHandle(fd))
	if err != nil {
		return false, err
	}
	defer handle.Delete()
	links, err := handle.LinkList()
	if err != nil {
		return false, err
	}
	if len(links) != 1 || links[0].Attrs().Name != "lo" {
		return false, nil
	}
	entries, err := os.ReadDir(defaultProcRoot)
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		if !entry.IsDir() || atoi(entry.Name()) <= 0 {
			continue
		}
		tasks, err := os.ReadDir(filepath.Join(defaultProcRoot, entry.Name(), "task"))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return false, err
		}
		for _, task := range tasks {
			if !task.IsDir() || atoi(task.Name()) <= 0 {
				continue
			}
			var taskNS syscall.Stat_t
			err := syscall.Stat(filepath.Join(defaultProcRoot, entry.Name(), "task", task.Name(), "ns", "net"), &taskNS)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return false, err
			}
			if fmt.Sprintf("%d:%d", taskNS.Dev, taskNS.Ino) == identity {
				return false, nil
			}
		}
	}
	return true, nil
}

// DebugNSConfig configures dbg0 inside the debug netns: MAC from the attach
// output, MTU mirroring the pool TAP, the inner /30, and the default route
// via the profile gateway. lo is brought up as well.
type DebugNSConfig struct {
	NSName  string
	TapName string // dbg0
	MAC     net.HardwareAddr
	MTU     int
	HostIP  net.IP     // host address inside CIDR (ParseCIDR masks IPNet.IP to the network address)
	CIDR    *net.IPNet // masked network
	Gateway net.IP
}

func ConfigureDebugNS(cfg DebugNSConfig) error {
	nsFd, err := unix.Open(filepath.Join("/var/run/netns", cfg.NSName), unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(nsFd)
	handle, err := netlink.NewHandleAt(vnetns.NsHandle(nsFd))
	if err != nil {
		return fmt.Errorf("netlink handle at %s: %w", cfg.NSName, err)
	}
	defer handle.Delete()

	lo, err := handle.LinkByName("lo")
	if err == nil {
		_ = handle.LinkSetUp(lo)
	}

	link, err := handle.LinkByName(cfg.TapName)
	if err != nil {
		return fmt.Errorf("find %s in %s: %w", cfg.TapName, cfg.NSName, err)
	}
	if cfg.MAC != nil {
		if err := handle.LinkSetHardwareAddr(link, cfg.MAC); err != nil {
			return fmt.Errorf("set mac: %w", err)
		}
	}
	if cfg.MTU > 0 {
		if err := handle.LinkSetMTU(link, cfg.MTU); err != nil {
			return fmt.Errorf("set mtu: %w", err)
		}
	}
	if cfg.CIDR != nil {
		ipnet := *cfg.CIDR
		if cfg.HostIP != nil {
			ipnet.IP = cfg.HostIP
		}
		if err := handle.AddrAdd(link, &netlink.Addr{IPNet: &ipnet}); err != nil {
			return fmt.Errorf("add addr: %w", err)
		}
	}
	if err := handle.LinkSetUp(link); err != nil {
		return fmt.Errorf("link up: %w", err)
	}
	if cfg.Gateway != nil {
		route := &netlink.Route{
			LinkIndex: link.Attrs().Index,
			Gw:        cfg.Gateway,
		}
		if err := handle.RouteReplace(route); err != nil {
			return fmt.Errorf("default route: %w", err)
		}
	}
	return nil
}

// LinkMTUInNS returns the MTU of a link inside a named namespace.
func LinkMTUInNS(nsName, linkName string) (int, error) {
	nsFd, err := unix.Open(filepath.Join("/var/run/netns", nsName), unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return 0, err
	}
	defer unix.Close(nsFd)
	handle, err := netlink.NewHandleAt(vnetns.NsHandle(nsFd))
	if err != nil {
		return 0, err
	}
	defer handle.Delete()
	link, err := handle.LinkByName(linkName)
	if err != nil {
		return 0, err
	}
	return link.Attrs().MTU, nil
}

// LinkIfindexInNS returns the ifindex of a link inside a named namespace.
// TUNSETIFF silently creates a new device when the named one is gone, so the
// pool TAP identity must be verified by ifindex after opening its fd.
func LinkIfindexInNS(nsName, linkName string) (uint32, error) {
	nsFd, err := unix.Open(filepath.Join("/var/run/netns", nsName), unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return 0, err
	}
	defer unix.Close(nsFd)
	handle, err := netlink.NewHandleAt(vnetns.NsHandle(nsFd))
	if err != nil {
		return 0, err
	}
	defer handle.Delete()
	link, err := handle.LinkByName(linkName)
	if err != nil {
		return 0, err
	}
	return uint32(link.Attrs().Index), nil
}

// WriteResolvConf installs /etc/netns/<ns>/resolv.conf — the file the
// ip-netns bind-mount convention (and our exec helper) puts over
// /etc/resolv.conf inside the namespace.
func WriteResolvConf(nsName, dns string) error {
	dir := filepath.Join("/etc/netns", nsName)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	return atomicWriteFile(filepath.Join(dir, "resolv.conf"), []byte("nameserver "+dns+"\n"), 0644)
}

// RemoveResolvConf drops the /etc/netns/<ns> directory.
func RemoveResolvConf(nsName string) error {
	return os.RemoveAll(filepath.Join("/etc/netns", nsName))
}

// WaitForLink polls until the named link exists in the namespace.
func WaitForLinkInNS(nsName, linkName string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := LinkIfindexInNS(nsName, linkName); err == nil {
			return nil
		}
		time.Sleep(25 * time.Millisecond)
	}
	return fmt.Errorf("link %s did not appear in %s", linkName, nsName)
}
