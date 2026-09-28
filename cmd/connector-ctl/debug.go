package main

// vswitch debug: open a disposable netns bridged to an allocated vswitch
// TAP port, so ping/curl/flatten-ctl run through the exact sandbox data plane
// without launching a sandbox. This file is a pure extension: the command
// registers itself via init() and no existing file is modified.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/kuasar-sandbox/connector/pkg/debug"
	"github.com/kuasar-sandbox/connector/pkg/netns"
	"github.com/kuasar-sandbox/connector/pkg/tapfd"
	"github.com/kuasar-sandbox/connector/pkg/vswitch"
	"golang.org/x/sys/unix"
)

var (
	debugPort           int
	debugList           bool
	debugCleanup        bool
	debugCIDR           string
	debugGateway        string
	debugDNS            string
	debugTailTries      int
	debugCtlTimeout     int
	debugCommandTimeout int
	debugKeepProxy      bool
	debugStateRoot      string
	debugTransitGW      string
	debugTransitVNI     uint32
	debugTransitMAC     string
)

var debugCmd = &cobra.Command{
	Use:   "debug [flags] [switch] [-- command [args...]]",
	Short: "Run network diagnostics through an allocated TAP switch port",
	Long: `Create a temporary network namespace and relay Ethernet frames between its
TAP device and an allocated switch TAP port. Commands in the namespace use the
same switch BPF paths as a sandbox, including management extraction, floating
IP handling, and Geneve transit when configured.

If switch is omitted, the only available switch is selected automatically.
When several switches exist, an interactive terminal prompts you to choose;
non-interactive invocations must specify a switch.

Without a command, start an interactive Bash shell. Exit the shell to stop the
relay, restore TAP offloads, remove the namespace, and release the port. Use
--list to inspect sessions and --cleanup-stale after an interrupted run.
Uncertain ownership or incomplete restoration leaves the port allocated for
inspection. Go sessions use /run/connector-debug by default; the separate
vswitch-tap-debug.sh sessions require that script's own --cleanup-stale.

Environment defaults (explicit flags take precedence): DEBUG_PORT, DEBUG_CIDR,
DEBUG_GATEWAY, DEBUG_DNS, DEBUG_TAIL_TRIES, DEBUG_CTL_TIMEOUT,
DEBUG_COMMAND_TIMEOUT,
DEBUG_KEEP_PROXY, DEBUG_STATE_ROOT, TRANSIT_GATEWAY_IP,
TRANSIT_GENEVE_VNI, and TRANSIT_MAC_ADDR.`,
	Example: `  connector-ctl vswitch debug
  connector-ctl vswitch debug --port 4 e2br0919
  connector-ctl vswitch debug e2br0919 -- ping -c 3 169.254.169.254
  connector-ctl vswitch debug --transit-gateway-ip 10.12.0.2 --transit-geneve-vni 100 sw1 -- ping -c 3 10.1.0.2
  connector-ctl vswitch debug --list
  connector-ctl vswitch debug --cleanup-stale`,
	Args: cobra.ArbitraryArgs,
	RunE: runDebug,
}

func init() {
	debugCmd.Flags().IntVar(&debugPort, "port", 0, "Allocate exactly port N; fail if unavailable. Without it the highest free tail ports are tried (never the front rows the sandbox allocator uses)")
	debugCmd.Flags().BoolVar(&debugList, "list", false, "List debug sessions (active/stale)")
	debugCmd.Flags().BoolVar(&debugCleanup, "cleanup-stale", false, "Clean sessions whose owner exited")
	debugCmd.Flags().StringVar(&debugCIDR, "cidr", "169.254.0.21/30", "Debug TAP IPv4/CIDR (e2b profile default)")
	debugCmd.Flags().StringVar(&debugGateway, "gateway", "169.254.0.22", "Default gateway inside the debug netns")
	debugCmd.Flags().StringVar(&debugDNS, "dns", "169.254.169.253", "Nameserver inside the debug netns (default: 169.254.169.253)")
	debugCmd.Flags().IntVar(&debugTailTries, "tail-tries", 8, "Auto mode: try at most this many highest-numbered free TAP slots in the upper half of the pool")
	debugCmd.Flags().IntVar(&debugCtlTimeout, "ctl-timeout", 30, "Maximum seconds for an attach operation (cleanup also bounds its worker)")
	debugCmd.Flags().IntVar(&debugCommandTimeout, "command-timeout", 120, "Maximum seconds for a one-shot command; 0 disables the limit (interactive shell is unlimited)")
	debugCmd.Flags().BoolVar(&debugKeepProxy, "keep-proxy", false, "Keep host proxy variables inside the namespace")
	debugCmd.Flags().StringVar(&debugStateRoot, "state-root", debug.DefaultStateRoot, "Session state directory")
	debugCmd.Flags().StringVar(&debugTransitGW, "transit-gateway-ip", "", "Geneve transit gateway IP (when transit is configured)")
	debugCmd.Flags().Uint32Var(&debugTransitVNI, "transit-geneve-vni", 0, "Geneve VNI for this port")
	debugCmd.Flags().StringVar(&debugTransitMAC, "transit-mac-addr", "", "Optional transit next-hop MAC")
	vswitchCmd.AddCommand(debugCmd)
}

func debugDie(sess *debug.Session, root *debug.StateRoot, format string, args ...any) error {
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintf(os.Stderr, "debug: %s\n", msg)
	if sess != nil && root != nil {
		if err := runDebugCleanup(root, sess.Dir, false); err != nil {
			fmt.Fprintf(os.Stderr, "debug: cleanup incomplete: %v; session retained at %s\n", err, sess.Dir)
		}
	}
	return fmt.Errorf("%s", msg)
}

func runDebug(cmd *cobra.Command, args []string) error {
	sigCh := make(chan os.Signal, 8)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigCh)
	if err := applyDebugEnv(cmd); err != nil {
		return err
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("run as root")
	}
	root, err := debug.OpenStateRoot(debugStateRoot)
	if err != nil {
		return fmt.Errorf("state root: %w", err)
	}

	if debugList || debugCleanup {
		if debugList && debugCleanup {
			return fmt.Errorf("--list and --cleanup-stale are mutually exclusive")
		}
		sessions, err := root.ListSessions()
		if err != nil {
			return err
		}
		failed := false
		for _, sess := range sessions {
			st, err := sess.Load()
			if err != nil {
				fmt.Printf("%s state=unreadable\n", sess.Dir)
				if debugCleanup {
					failed = true
				}
				continue
			}
			port := st.Port
			if port == 0 {
				port = st.PendingPort
			}
			status := "stale"
			if st.OwnPortAlive() {
				status = "active"
			}
			if debugList {
				fmt.Printf("%s switch=%s port=%d ns=%s status=%s\n", sess.Dir, st.Switch, port, st.DebugNS, status)
				continue
			}
			fmt.Printf("cleaning %s (port=%d)\n", sess.Dir, port)
			if err := runDebugCleanup(root, sess.Dir, true); err != nil {
				fmt.Fprintf(os.Stderr, "debug: %v\n", err)
				failed = true
			}
		}
		if failed {
			return fmt.Errorf("some sessions retained; rerun --cleanup-stale after inspecting them")
		}
		return nil
	}

	var switchName string
	var userCmd []string
	if len(args) == 0 || cmd.Flags().ArgsLenAtDash() == 0 {
		switchName, err = selectDebugSwitch()
		if err != nil {
			return err
		}
		userCmd = args
	} else {
		switchName = args[0]
		userCmd = args[1:]
	}
	if debugPort < 0 || debugCommandTimeout < 0 || debugTailTries < 1 || debugCtlTimeout < 1 {
		return fmt.Errorf("--port and --command-timeout must be nonnegative; --tail-tries and --ctl-timeout must be positive")
	}
	const maxDurationSeconds = uint64((1<<63 - 1) / int64(time.Second))
	if uint64(debugCommandTimeout) > maxDurationSeconds || uint64(debugCtlTimeout) > maxDurationSeconds-60 {
		return fmt.Errorf("--command-timeout or --ctl-timeout is too large")
	}
	// pflag strips the "--" separator, so everything after the switch name is
	// the user command.

	innerIP, innerNet, err := net.ParseCIDR(debugCIDR)
	if err != nil || innerIP.To4() == nil {
		return fmt.Errorf("invalid --cidr %q", debugCIDR)
	}
	if net.ParseIP(debugGateway) == nil || net.ParseIP(debugDNS) == nil {
		return fmt.Errorf("invalid --gateway/--dns")
	}
	if sig := debugPendingSignal(sigCh); sig != nil {
		return fmt.Errorf("debug interrupted by %s", sig)
	}

	sw, err := debug.OpenSwitch(switchName)
	if err != nil {
		return fmt.Errorf("switch unavailable: %w", err)
	}
	status, err := sw.Status()
	if err != nil {
		return fmt.Errorf("switch status: %w", err)
	}
	if !status.IsReady() {
		return fmt.Errorf("switch is not ready: %s", status.GetNotReadyReasons())
	}
	cfg := sw.Config()
	switchNS := status.SwitchNetNS
	switchIno, err := debug.NetnsIno(switchNS)
	if err != nil {
		return fmt.Errorf("identify switch netns: %w", err)
	}

	state := &debug.SessionState{
		OwnerPID:    os.Getpid(),
		OwnerStart:  debug.SelfStarttime(),
		CreatedAt:   time.Now().Unix(),
		Switch:      switchName,
		SwitchNetns: switchNS,
		SwitchNsIno: switchIno,
		InnerIP:     innerIP.String(),
		Phase:       "preparing",
	}
	sess, err := root.NewSessionDir(state)
	if err != nil {
		return err
	}

	// ── port allocation (explicit or tail-window auto) ─────────────────────
	candidates, err := debugCandidates(sw, cfg.N_ports)
	if err != nil {
		_ = sess.Remove()
		return err
	}
	var out *vswitch.AttachOutput
	var st *debug.SessionState
	for _, port := range candidates {
		if sig := debugPendingSignal(sigCh); sig != nil {
			return debugDie(sess, root, "interrupted by %s", sig)
		}
		st, err = sess.Load()
		if err != nil {
			return debugDie(sess, root, "load session before attach: %v", err)
		}
		st.PendingPort = int(port)
		if err := sess.Save(st); err != nil {
			return debugDie(sess, root, "save pending port: %v", err)
		}
		attachOut, rc, err := debugAttachWorker(sess, switchName, port, innerIP.String(), sigCh)
		if err != nil {
			return debugDie(sess, root, "%v", err)
		}
		if rc == debugAttachBusy {
			continue // confirmed rejection: the port lost the race
		}
		out = attachOut
		break
	}
	if out == nil {
		_ = sess.Remove()
		return fmt.Errorf("failed to allocate a TAP port (%d tail candidates lost the race)", len(candidates))
	}
	if out.Mode != "tap" || out.PortDev == "" {
		return debugDie(sess, root, "allocated port %d is %q, expected TAP", out.Port, out.Mode)
	}
	if debugPort == 0 {
		fmt.Fprintf(os.Stderr, "debug: auto-selected tail port=%d (front ports left for the sequential allocator)\n", out.Port)
	}

	// ── verify slot and device identity ────────────────────────────────────
	slots := sw.MmapSlots()
	slotIdx := out.Port - 1
	ifindex := slots.GetSlot(slotIdx).Ifindex
	wantFIP := vswitch.Uint32ToIP(cfg.FloatingIpBase + slotIdx)
	st, err = sess.Load()
	if err != nil {
		return debugDie(sess, root, "load allocated session: %v", err)
	}
	st.Port = int(out.Port)
	st.TapName = out.PortDev
	st.Ifindex = ifindex
	st.FloatingIP = wantFIP.String()
	st.Phase = "allocated"
	if err := sess.Save(st); err != nil {
		return debugDie(sess, root, "save session: %v", err)
	}
	gotIfindex, err := debug.LinkIfindexInNS(switchNS, out.PortDev)
	if err != nil {
		return debugDie(sess, root, "pool TAP %s missing in %s: %v", out.PortDev, switchNS, err)
	}
	if gotIfindex != ifindex {
		return debugDie(sess, root, "pool TAP ifindex changed: slot=%d device=%d", ifindex, gotIfindex)
	}
	if idle, err := debug.TapIdle("/proc", out.PortDev); err != nil || !idle {
		return debugDie(sess, root, "allocated TAP %s is not idle", out.PortDev)
	}
	if sig := debugPendingSignal(sigCh); sig != nil {
		return debugDie(sess, root, "interrupted by %s", sig)
	}

	// ── offload snapshot + suppress, ns create, DNS ────────────────────────
	snap, err := debug.SnapshotOffloadsInNS(switchNS, out.PortDev)
	if err != nil {
		return debugDie(sess, root, "offload snapshot/suppress: %v", err)
	}
	if err := debug.SaveSnapshotFile(sess.OffloadPath(), snap); err != nil {
		return debugDie(sess, root, "save offload snapshot: %v", err)
	}
	st, err = sess.Load()
	if err != nil {
		return debugDie(sess, root, "load offload session: %v", err)
	}
	st.OffloadDirty = true
	if err := sess.Save(st); err != nil {
		return debugDie(sess, root, "save session: %v", err)
	}
	if err := debug.ApplySuppressedOffloadsInNS(switchNS, out.PortDev, snap); err != nil {
		return debugDie(sess, root, "disable TAP offloads: %v", err)
	}

	debugNS := "sandbox_debug_ns_" + strings.TrimPrefix(filepathBase(sess.Dir), "session.")
	st, err = sess.Load()
	if err != nil {
		return debugDie(sess, root, "load namespace session: %v", err)
	}
	st.DebugNS = debugNS
	if err := sess.Save(st); err != nil {
		return debugDie(sess, root, "save debug namespace intent: %v", err)
	}
	debugIno, err := debug.CreateDebugNS(debugNS)
	if err != nil {
		return debugDie(sess, root, "create debug netns: %v", err)
	}
	st, err = sess.Load()
	if err != nil {
		return debugDie(sess, root, "load created namespace session: %v", err)
	}
	st.DebugNSIno = debugIno
	if err := sess.Save(st); err != nil {
		return debugDie(sess, root, "save debug namespace identity: %v", err)
	}
	if err := debug.WriteResolvConf(debugNS, debugDNS); err != nil {
		return debugDie(sess, root, "resolv.conf: %v", err)
	}

	// ── open both TAP fds (main process holds them; they die with us) ─────
	var poolFD *os.File
	switchNsObj, err := netns.GetByName(switchNS)
	if err != nil {
		return debugDie(sess, root, "open switch ns: %v", err)
	}
	err = switchNsObj.Do(func() error {
		f, e := tapfd.OpenTap(out.PortDev, unix.IFF_NO_PI)
		if e != nil {
			return e
		}
		poolFD = f
		return nil
	})
	switchNsObj.Close()
	if err != nil {
		return debugDie(sess, root, "open pool TAP fd: %v", err)
	}
	if got, _ := debug.LinkIfindexInNS(switchNS, out.PortDev); got != ifindex {
		poolFD.Close()
		return debugDie(sess, root, "pool TAP identity changed after open")
	}

	var dbgFD *os.File
	debugNsObj, err := netns.GetByName(debugNS)
	if err != nil {
		poolFD.Close()
		return debugDie(sess, root, "open debug ns: %v", err)
	}
	err = debugNsObj.Do(func() error {
		f, e := tapfd.OpenTap("dbg0", unix.IFF_NO_PI)
		if e != nil {
			return e
		}
		dbgFD = f
		return nil
	})
	debugNsObj.Close()
	if err != nil {
		poolFD.Close()
		return debugDie(sess, root, "open dbg0: %v", err)
	}

	mac, _ := net.ParseMAC(out.PortMAC)
	mtu, err := debug.LinkMTUInNS(switchNS, out.PortDev)
	if err != nil {
		mtu = 1500
	}
	if err := debug.ConfigureDebugNS(debug.DebugNSConfig{
		NSName: debugNS, TapName: "dbg0", MAC: mac, MTU: mtu,
		HostIP: innerIP, CIDR: innerNet, Gateway: net.ParseIP(debugGateway),
	}); err != nil {
		poolFD.Close()
		dbgFD.Close()
		return debugDie(sess, root, "configure dbg0: %v", err)
	}

	// ── bridge + user command ──────────────────────────────────────────────
	bridge := debug.NewBridge(&debug.TapEndpoint{F: poolFD}, &debug.TapEndpoint{F: dbgFD})
	bridge.Start()

	childEnv := debugChildEnv(switchName, int(out.Port))
	oneShot := len(userCmd) != 0
	if len(userCmd) == 0 {
		userCmd = []string{"bash", "--noprofile", "--norc"}
	}
	self, err := os.Executable()
	if err != nil {
		_ = stopDebugBridge(bridge, 2*time.Second)
		return debugDie(sess, root, "resolve self: %v", err)
	}
	execArgs := append([]string{"__debug-exec", "--netns", debugNS, "--"}, userCmd...)
	child := exec.Command(self, execArgs...)
	child.Env = childEnv
	child.Stdin, child.Stdout, child.Stderr = os.Stdin, os.Stdout, os.Stderr
	if sig := debugPendingSignal(sigCh); sig != nil {
		_ = stopDebugBridge(bridge, 2*time.Second)
		return debugDie(sess, root, "interrupted by %s", sig)
	}
	if err := child.Start(); err != nil {
		_ = stopDebugBridge(bridge, 2*time.Second)
		return debugDie(sess, root, "start command: %v", err)
	}
	st, err = sess.Load()
	if err != nil {
		_ = stopDebugBridge(bridge, 2*time.Second)
		return debugDie(sess, root, "load command session: %v", err)
	}
	st.ChildPID = child.Process.Pid
	st.ChildStart, err = debug.ProcStarttimeOf(child.Process.Pid)
	if err != nil {
		_ = stopDebugBridge(bridge, 2*time.Second)
		return debugDie(sess, root, "identify command process: %v", err)
	}
	st.Phase = "running"
	if err := sess.Save(st); err != nil {
		_ = stopDebugBridge(bridge, 2*time.Second)
		return debugDie(sess, root, "save session: %v", err)
	}

	fmt.Fprintf(os.Stderr, "debug: port=%d TAP=%s MAC=%s netns=%s\n", out.Port, out.PortDev, out.PortMAC, debugNS)
	fmt.Fprintf(os.Stderr, "debug: entered %s; exit to release port %d\n", debugNS, out.Port)

	waitErr := make(chan error, 1)
	go func() { waitErr <- child.Wait() }()
	var commandTimeout <-chan time.Time
	if oneShot && debugCommandTimeout > 0 {
		timer := time.NewTimer(time.Duration(debugCommandTimeout) * time.Second)
		defer timer.Stop()
		commandTimeout = timer.C
	}

	var runErr error
	var interruptedBy syscall.Signal
	timedOut := false
loop:
	for {
		select {
		case sig := <-sigCh:
			childErr, stopErr := stopDebugCommand(child.Process.Pid, st.ChildStart, waitErr)
			interruptedBy = sig.(syscall.Signal)
			runErr = fmt.Errorf("debug command interrupted by %s", sig)
			if childErr != nil {
				runErr = fmt.Errorf("%w: %w", runErr, childErr)
			}
			if stopErr != nil {
				runErr = fmt.Errorf("%w; %v", runErr, stopErr)
			}
			break loop
		case err := <-waitErr:
			runErr = err
			break loop
		case <-commandTimeout:
			childErr, stopErr := stopDebugCommand(child.Process.Pid, st.ChildStart, waitErr)
			timedOut = true
			runErr = fmt.Errorf("debug command timed out after %d seconds", debugCommandTimeout)
			if childErr != nil {
				runErr = fmt.Errorf("%w: %w", runErr, childErr)
			}
			if stopErr != nil {
				runErr = fmt.Errorf("%w; %v", runErr, stopErr)
			}
			break loop
		case <-bridge.Done():
			// relay fault: terminate the command, never leave a live-but-dead shell
			_, stopErr := stopDebugCommand(child.Process.Pid, st.ChildStart, waitErr)
			runErr = fmt.Errorf("debug relay failed")
			if stopErr != nil {
				runErr = fmt.Errorf("%w; %v", runErr, stopErr)
			}
			break loop
		}
	}
	// Attempt to close both relay fds before cleanup. If closure stalls, the
	// recovery pipeline sees the live holder and conservatively retains the port.
	if err := stopDebugBridge(bridge, 2*time.Second); err != nil && !errors.Is(err, io.EOF) {
		fmt.Fprintf(os.Stderr, "debug: %v\n", err)
	}

	cleanupErr := runDebugCleanup(root, sess.Dir, false)
	if cleanupErr != nil {
		fmt.Fprintf(os.Stderr, "debug: %v\n", cleanupErr)
		fmt.Fprintf(os.Stderr, "debug: recovery: connector-ctl vswitch debug --cleanup-stale\n")
		return cleanupErr
	}
	if runErr != nil {
		if interruptedBy != 0 {
			fmt.Fprintln(os.Stderr, runErr)
			os.Exit(128 + int(interruptedBy))
		}
		if timedOut {
			fmt.Fprintln(os.Stderr, runErr)
			os.Exit(124)
		}
		var ee *exec.ExitError
		if errors.As(runErr, &ee) {
			if status, ok := ee.Sys().(syscall.WaitStatus); ok && status.Signaled() {
				os.Exit(128 + int(status.Signal()))
			}
			os.Exit(ee.ExitCode())
		}
		return runErr
	}
	return nil
}

// applyDebugEnv keeps the shell tool's configuration names; explicit flags win.
func applyDebugEnv(cmd *cobra.Command) error {
	for _, entry := range []struct{ flag, env string }{
		{"port", "DEBUG_PORT"}, {"cidr", "DEBUG_CIDR"},
		{"gateway", "DEBUG_GATEWAY"}, {"dns", "DEBUG_DNS"},
		{"tail-tries", "DEBUG_TAIL_TRIES"}, {"state-root", "DEBUG_STATE_ROOT"},
		{"ctl-timeout", "DEBUG_CTL_TIMEOUT"},
		{"command-timeout", "DEBUG_COMMAND_TIMEOUT"},
		{"transit-gateway-ip", "TRANSIT_GATEWAY_IP"},
		{"transit-geneve-vni", "TRANSIT_GENEVE_VNI"},
		{"transit-mac-addr", "TRANSIT_MAC_ADDR"},
	} {
		if value := os.Getenv(entry.env); value != "" && !cmd.Flags().Changed(entry.flag) {
			if err := cmd.Flags().Set(entry.flag, value); err != nil {
				return fmt.Errorf("%s: %w", entry.env, err)
			}
		}
	}
	if os.Getenv("DEBUG_KEEP_PROXY") != "" && !cmd.Flags().Changed("keep-proxy") {
		if err := cmd.Flags().Set("keep-proxy", "true"); err != nil {
			return err
		}
	}
	return nil
}

// debugCandidates returns explicit [port] or the tail-window free TAP slots.
func debugCandidates(sw vswitch.Interface, totalPorts uint32) ([]uint32, error) {
	if debugPort > 0 {
		if uint32(debugPort) > totalPorts {
			return nil, fmt.Errorf("--port %d exceeds switch ports (%d)", debugPort, totalPorts)
		}
		idx := uint32(debugPort - 1)
		slot := sw.MmapSlots().GetSlot(idx)
		if vswitch.SlotPortKind(slot) != vswitch.PortKindTap || slot.Ifindex == 0 {
			return nil, fmt.Errorf("--port %d is not a provisioned TAP slot", debugPort)
		}
		if sw.MmapSlots().GetInnerIP(idx) != vswitch.InnerIPFree {
			return nil, fmt.Errorf("--port %d is already allocated", debugPort)
		}
		return []uint32{uint32(debugPort)}, nil
	}
	tail := debugTailTries
	lowest := int(totalPorts / 2)
	var out []uint32
	slots := sw.MmapSlots()
	for p := int(totalPorts); p > lowest; p-- {
		idx := uint32(p - 1)
		if slots.GetInnerIP(idx) != vswitch.InnerIPFree {
			continue
		}
		slot := slots.GetSlot(idx)
		if vswitch.SlotPortKind(slot) != vswitch.PortKindTap || slot.Ifindex == 0 {
			continue
		}
		out = append(out, uint32(p))
		if len(out) == tail {
			break
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no free TAP-mode port in the tail range")
	}
	return out, nil
}

// debugChildEnv strips host proxy variables (the guest has none) and adds
// a shell prompt identifying the session.
func debugChildEnv(switchName string, port int) []string {
	skip := map[string]bool{
		"http_proxy": true, "https_proxy": true, "all_proxy": true, "ftp_proxy": true, "no_proxy": true,
		"HTTP_PROXY": true, "HTTPS_PROXY": true, "ALL_PROXY": true, "FTP_PROXY": true, "NO_PROXY": true,
	}
	var env []string
	for _, kv := range os.Environ() {
		key := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			key = kv[:i]
		}
		if debugKeepProxy || !skip[key] {
			env = append(env, kv)
		}
	}
	env = append(env, fmt.Sprintf("PS1=debug:%s:%d$ ", switchName, port))
	return env
}

func filepathBase(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

const debugAttachBusy = 99 // worker-detected "port already allocated"

// debugAttachWorker runs attach in a reexec'd worker holding attach.lock,
// so a SIGKILLed parent cannot race recovery against a live attach.
func debugAttachWorker(sess *debug.Session, switchName string, port uint32, innerIP string, interrupt <-chan os.Signal) (*vswitch.AttachOutput, int, error) {
	lockFD, err := os.OpenFile(sess.AttachLockPath(), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, 0, fmt.Errorf("attach lock: %w", err)
	}
	defer lockFD.Close()
	deadline := time.Now().Add(time.Duration(debugCtlTimeout) * time.Second)
	for {
		if sig := debugPendingSignal(interrupt); sig != nil {
			return nil, 0, fmt.Errorf("attach interrupted by %s", sig)
		}
		err = unix.Flock(int(lockFD.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if err != unix.EWOULDBLOCK && err != unix.EAGAIN {
			return nil, 0, fmt.Errorf("flock attach: %w", err)
		}
		if !time.Now().Before(deadline) {
			return nil, 0, fmt.Errorf("attach lock timed out after %d seconds", debugCtlTimeout)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if sig := debugPendingSignal(interrupt); sig != nil {
		return nil, 0, fmt.Errorf("attach interrupted by %s", sig)
	}
	if err := os.Remove(sess.AttachRCPath()); err != nil && !os.IsNotExist(err) {
		return nil, 0, fmt.Errorf("remove old attach receipt: %w", err)
	}

	self, err := os.Executable()
	if err != nil {
		return nil, 0, err
	}
	args := []string{
		"__debug-attach",
		"--dir", sess.Dir,
		"--switch", switchName,
		"--port", strconv.FormatUint(uint64(port), 10),
		"--inner-ip", innerIP,
	}
	if debugTransitGW != "" {
		args = append(args, "--transit-gateway-ip", debugTransitGW)
	}
	if debugTransitVNI != 0 {
		args = append(args, "--transit-geneve-vni", strconv.FormatUint(uint64(debugTransitVNI), 10))
	}
	if debugTransitMAC != "" {
		args = append(args, "--transit-mac-addr", debugTransitMAC)
	}
	worker := exec.Command(self, args...)
	worker.ExtraFiles = []*os.File{lockFD}
	worker.Stdout, worker.Stderr = os.Stderr, os.Stderr
	if err := worker.Start(); err != nil {
		return nil, 0, fmt.Errorf("start attach worker: %w", err)
	}
	done := make(chan error, 1)
	go func() { done <- worker.Wait() }()
	select {
	case <-done:
	case sig := <-interrupt:
		_ = worker.Process.Kill()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
		return nil, 0, fmt.Errorf("attach interrupted by %s; session retained until cleanup verifies allocation", sig)
	case <-time.After(time.Duration(debugCtlTimeout) * time.Second):
		_ = worker.Process.Kill()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			return nil, 0, fmt.Errorf("attach worker did not exit after KILL; session retained")
		}
		if _, err := os.Stat(sess.AttachRCPath()); err != nil {
			return nil, 0, fmt.Errorf("attach worker killed on timeout with no receipt; session retained")
		}
	}
	rcData, err := os.ReadFile(sess.AttachRCPath())
	if err != nil {
		return nil, 0, fmt.Errorf("attach outcome unknown (no receipt); session retained")
	}
	rc, err := strconv.Atoi(strings.TrimSpace(string(rcData)))
	if err != nil || rc < 0 || rc >= 128 {
		return nil, 0, fmt.Errorf("attach receipt malformed (%q); session retained", string(rcData))
	}
	if rc == debugAttachBusy {
		return nil, debugAttachBusy, nil
	}
	if rc != 0 {
		errTxt, _ := os.ReadFile(sess.AttachErrPath())
		return nil, 0, fmt.Errorf("attach failed (rc=%d): %s", rc, strings.TrimSpace(string(errTxt)))
	}
	data, err := os.ReadFile(sess.AttachJSONPath())
	if err != nil {
		return nil, 0, fmt.Errorf("attach receipt unreadable: %w", err)
	}
	var out vswitch.AttachOutput
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, 0, fmt.Errorf("attach receipt malformed: %w", err)
	}
	if int(out.Port) != int(port) {
		return nil, 0, fmt.Errorf("attach receipt port mismatch: got %d want %d", out.Port, port)
	}
	return &out, 0, nil
}
