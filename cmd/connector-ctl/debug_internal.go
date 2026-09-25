package main

// Hidden reexec subcommands backing `vswitch debug`. They are internal
// entry points of the same binary and are never called by users directly.

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/kuasar-sandbox/connector/pkg/debug"
	"github.com/kuasar-sandbox/connector/pkg/vswitch"
	"golang.org/x/sys/unix"
)

// __debug-attach: runs vswitch Attach on an exact port and persists the
// receipt (attach.json / attach.rc / attach.err). The inherited attach.lock
// fd (ExtraFiles[0] → fd 3) keeps the lock held across a parent SIGKILL, so
// recovery never races a live attach.
var debugAttachInternal = &cobra.Command{
	Use:    "__debug-attach",
	Short:  "internal: attach worker for debug",
	Hidden: true,
	Args:   cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		dir, _ := cmd.Flags().GetString("dir")
		switchName, _ := cmd.Flags().GetString("switch")
		port, _ := cmd.Flags().GetUint32("port")
		innerIP, _ := cmd.Flags().GetString("inner-ip")
		gw, _ := cmd.Flags().GetString("transit-gateway-ip")
		vni, _ := cmd.Flags().GetUint32("transit-geneve-vni")
		macStr, _ := cmd.Flags().GetString("transit-mac-addr")

		sess := &debug.Session{Dir: dir}
		opts := vswitch.AttachOptions{Port: int(port), InnerIP: parseIPOrNil(innerIP)}
		if gw != "" {
			opts.TransitGatewayIP = parseIPOrNil(gw)
		}
		opts.TransitGeneveVNI = vni
		if macStr != "" {
			mac, err := net_ParseMAC(macStr)
			if err != nil {
				return writeAttachReceipt(sess, 2, nil, err)
			}
			opts.TransitMAC = mac
		}

		sw, err := debug.OpenSwitch(switchName)
		if err != nil {
			return writeAttachReceipt(sess, 1, nil, err)
		}
		out, err := sw.Attach(opts)
		if err != nil {
			if isPortAllocatedErr(err) {
				// confirmed rejection: safe for the parent to try the next port
				return writeAttachReceipt(sess, debugAttachBusy, nil, err)
			}
			return writeAttachReceipt(sess, 1, nil, err)
		}
		return writeAttachReceipt(sess, 0, out, nil)
	},
}

func parseIPOrNil(s string) net.IP {
	ip := net.ParseIP(s)
	if ip == nil {
		return net.IPv4zero
	}
	return ip
}

func isPortAllocatedErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "already allocated") ||
		strings.Contains(msg, vswitch.ErrPortAllocated.Error())
}

func writeAttachReceipt(sess *debug.Session, rc int, out *vswitch.AttachOutput, cause error) error {
	if cause != nil {
		_ = debug.WriteFileAtomic(sess.AttachErrPath(), []byte(cause.Error()+"\n"), 0600)
	}
	if out != nil {
		data, err := json.Marshal(out)
		if err != nil {
			return fmt.Errorf("encode attach receipt: %w", err)
		}
		if err := debug.WriteFileAtomic(sess.AttachJSONPath(), data, 0600); err != nil {
			return fmt.Errorf("persist attach receipt: %w", err)
		}
	}
	return debug.WriteFileAtomic(sess.AttachRCPath(), []byte(strconv.Itoa(rc)+"\n"), 0600)
}

// __debug-exec: enters the debug netns with a private mount namespace,
// bind-mounts the session resolv.conf over /etc/resolv.conf (the ip-netns
// convention — plain setns cannot fake DNS), then execs the user command.
var debugExecInternal = &cobra.Command{
	Use:    "__debug-exec --netns NAME [--] command [args...]",
	Short:  "internal: exec helper for debug",
	Hidden: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		nsName, _ := cmd.Flags().GetString("netns")
		flags := cmd.Flags()
		_ = flags
		cmdArgs := args
		// strip a leading "--"
		if len(cmdArgs) > 0 && cmdArgs[0] == "--" {
			cmdArgs = cmdArgs[1:]
		}
		if nsName == "" || len(cmdArgs) == 0 {
			return fmt.Errorf("usage: __debug-exec --netns NAME [--] command [args...]")
		}

		runtime.LockOSThread()
		defer runtime.UnlockOSThread()

		nsFd, err := unix.Open("/var/run/netns/"+nsName, unix.O_RDONLY|unix.O_CLOEXEC, 0)
		if err != nil {
			return fmt.Errorf("open netns %s: %w", nsName, err)
		}
		defer unix.Close(nsFd)
		if err := unix.Setns(nsFd, unix.CLONE_NEWNET); err != nil {
			return fmt.Errorf("setns %s: %w", nsName, err)
		}

		// Private mount namespace so the resolv.conf bind mount below never
		// leaks into the host.
		if err := unix.Unshare(unix.CLONE_NEWNS); err != nil {
			return fmt.Errorf("unshare mount ns: %w", err)
		}
		if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
			return fmt.Errorf("make mount namespace private: %w", err)
		}
		// Remount sysfs so /sys/class/net reflects the debug netns: the
		// inherited /sys mount stays tagged to the namespace it was mounted
		// in (the initial one), exactly what `ip netns exec` corrects for.
		// Best-effort: without it, netlink tools still work, but sysfs-based
		// lookups inside the session would see the host's interfaces.
		if err := unix.Unmount("/sys", unix.MNT_DETACH); err == nil {
			if err := unix.Mount("sysfs", "/sys", "sysfs", 0, ""); err != nil {
				fmt.Fprintf(os.Stderr, "debug-exec: sysfs remount failed (%v); /sys shows host devices\n", err)
				_ = unix.Mount("none", "/sys", "", 0, "")
			}
		}
		resolv := "/etc/netns/" + nsName + "/resolv.conf"
		if _, err := os.Stat(resolv); err != nil {
			return fmt.Errorf("debug resolv.conf: %w", err)
		}
		if err := unix.Mount(resolv, "/etc/resolv.conf", "", unix.MS_BIND, ""); err != nil {
			return fmt.Errorf("bind resolv.conf: %w", err)
		}

		bin, err := exec.LookPath(cmdArgs[0])
		if err != nil {
			return fmt.Errorf("lookpath %s: %w", cmdArgs[0], err)
		}
		return unix.Exec(bin, cmdArgs, os.Environ())
	},
}

func net_ParseMAC(s string) (net.HardwareAddr, error) { return net.ParseMAC(s) }

func init() {
	debugAttachInternal.Flags().String("dir", "", "session dir")
	debugAttachInternal.Flags().String("switch", "", "switch name")
	debugAttachInternal.Flags().Uint32("port", 0, "port to attach")
	debugAttachInternal.Flags().String("inner-ip", "", "inner ip")
	debugAttachInternal.Flags().String("transit-gateway-ip", "", "")
	debugAttachInternal.Flags().Uint32("transit-geneve-vni", 0, "")
	debugAttachInternal.Flags().String("transit-mac-addr", "", "")

	debugExecInternal.Flags().String("netns", "", "debug netns name")
	rootCmd.AddCommand(debugAttachInternal)
	rootCmd.AddCommand(debugExecInternal)
}
