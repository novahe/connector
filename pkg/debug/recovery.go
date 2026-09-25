package debug

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/kuasar-sandbox/connector/pkg/vswitch"
)

// recovery implements the deterministic cleanup pipeline shared by normal
// exit, signals, and --cleanup-stale. Every uncertain outcome retains the
// port (stays Allocated) and the session record; only fully verified state
// is torn down. The vswitch opener is injectable for unit tests.

// OpenSwitch is the hook to the real switch API (vswitch.Open).
var OpenSwitch = vswitch.Open

// CleanupOptions tweaks one cleanup run.
type CleanupOptions struct {
	// Stale marks a --cleanup-stale run: sessions with a live owner are skipped.
	Stale bool
	// LockWait bounds the session-lock wait.
	LockWait time.Duration
	// TermWait/KillWait bound process termination per signal.
	TermWait, KillWait time.Duration
}

func (o *CleanupOptions) fill() {
	if o.LockWait <= 0 {
		o.LockWait = 60 * time.Second
	}
	if o.TermWait <= 0 {
		o.TermWait = time.Second
	}
	if o.KillWait <= 0 {
		o.KillWait = time.Second
	}
}

// CleanupSession cleans one session directory. A nil return means the
// session reached a fully verified end state (or was legitimately skipped);
// an error means the session is retained for manual inspection or retry.
func CleanupSession(root *StateRoot, dir string, opts CleanupOptions) error {
	opts.fill()
	sess, err := root.OpenSession(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // already cleaned by a concurrent cleaner
		}
		return err
	}
	if err := sess.LockExclusive(opts.LockWait); err != nil {
		return fmt.Errorf("session lock: %w", err)
	}
	defer sess.Close()

	if _, err := os.Stat(sess.StatePath()); err != nil {
		return fmt.Errorf("session state missing or unreadable: %w", err)
	}
	st, err := sess.Load()
	if err != nil {
		return fmt.Errorf("session state unreadable: %w", err)
	}
	if opts.Stale && st.OwnPortAlive() {
		return nil // active; skipped
	}

	// An attach worker still holding attach.lock means the allocation
	// outcome is unknown: retain everything.
	if attachInFlight(sess) {
		return fmt.Errorf("attach still running; session retained")
	}

	uncertain := false
	ownPort := true
	processesOK := true

	// Pending allocation with no receipt: the slot must be provably free,
	// otherwise the ownership of any allocation is unknown.
	if st.Phase == "preparing" && st.PendingPort > 0 && st.Port == 0 {
		// The worker writes its receipt before releasing attach.lock. A killed
		// parent may never have copied a successful attach into state.json.
		if rcBytes, err := os.ReadFile(sess.AttachRCPath()); err == nil {
			rc, err := strconv.Atoi(strings.TrimSpace(string(rcBytes)))
			if err != nil {
				return fmt.Errorf("malformed attach receipt; session retained: %w", err)
			}
			if rc == 0 {
				data, err := os.ReadFile(sess.AttachJSONPath())
				if err != nil {
					return fmt.Errorf("attach success receipt missing: %w", err)
				}
				var out vswitch.AttachOutput
				if err := json.Unmarshal(data, &out); err != nil {
					return fmt.Errorf("attach receipt invalid: %w", err)
				}
				if int(out.Port) != st.PendingPort || out.Mode != "tap" || out.PortDev == "" {
					return fmt.Errorf("attach receipt identity mismatch; session retained")
				}
				sw, err := OpenSwitch(st.Switch)
				if err != nil {
					return err
				}
				cfg := sw.Config()
				if cfg == nil || out.Port == 0 || out.Port > cfg.N_ports {
					return fmt.Errorf("attach receipt port invalid")
				}
				st.Port = int(out.Port)
				st.TapName = out.PortDev
				st.Ifindex = sw.MmapSlots().GetSlot(out.Port - 1).Ifindex
				st.FloatingIP = vswitch.Uint32ToIP(cfg.FloatingIpBase + out.Port - 1).String()
				st.Phase = "allocated"
				if err := sess.Save(st); err != nil {
					return err
				}
			} else if rc == 99 {
				// Attach rejected this slot before claiming it; its current
				// occupant belongs to someone else.
				st.PendingPort = 0
				if err := sess.Save(st); err != nil {
					return err
				}
			} else {
				// Other errors may occur before or after the CAS. Only a
				// provably free slot lets us discard the pending intent.
				sw, err := OpenSwitch(st.Switch)
				if err != nil {
					return err
				}
				free, checkErr := slotIsFree(sw, st.PendingPort)
				_ = sw.Close()
				if checkErr != nil || !free {
					return fmt.Errorf("attach failed with rc=%d; allocation ownership unknown", rc)
				}
				st.PendingPort = 0
				if err := sess.Save(st); err != nil {
					return err
				}
			}
		}
	}
	if st.Phase == "preparing" && st.PendingPort > 0 && st.Port == 0 {
		sw, err := OpenSwitch(st.Switch)
		if err == nil {
			free, err := slotIsFree(sw, st.PendingPort)
			if err != nil || !free {
				uncertain = true
			}
		} else {
			uncertain = true
		}
		if uncertain {
			return fmt.Errorf("pending allocation %d has unknown ownership; session retained", st.PendingPort)
		}
	}

	// Switch namespace identity: a rebuilt switch of the same name must not
	// be treated as the switch this session allocated on.
	if st.SwitchNsIno != "" && st.SwitchNetns != "" {
		if ino, err := NetnsIno(st.SwitchNetns); err != nil || ino != st.SwitchNsIno {
			fmt.Fprintf(os.Stderr, "debug: switch namespace changed; port left untouched: %s\n", dir)
			ownPort = false
			uncertain = true
		}
	}

	// Debug namespace identity: a recreated namespace of the same name is
	// not ours — never touch its processes.
	nsValid := false
	if st.DebugNS != "" {
		if st.DebugNSIno == "" {
			if _, err := NetnsIno(st.DebugNS); err == nil {
				return fmt.Errorf("debug namespace %s has no recorded identity; session retained", st.DebugNS)
			} else if !os.IsNotExist(err) {
				return fmt.Errorf("identify pending debug namespace: %w", err)
			}
		}
		ino, err := NetnsIno(st.DebugNS)
		switch {
		case err != nil:
			nsValid = false // gone: nothing to clean inside
		case st.DebugNSIno != "" && ino == st.DebugNSIno:
			nsValid = true
		default:
			fmt.Fprintf(os.Stderr, "debug: debug namespace changed; namespace left untouched: %s\n", dir)
			uncertain = true
		}
	}

	// Stop the session command, then reap anything left inside our ns.
	if st.ChildPID > 0 && st.ChildStart != "" {
		if err := StopProcess(defaultProcRoot, fmt.Sprint(st.ChildPID), st.ChildStart, opts.TermWait, opts.KillWait); err != nil {
			processesOK = false
		}
	}
	if nsValid && st.DebugNSIno != "" {
		if err := KillNetnsProcesses(defaultProcRoot, st.DebugNSIno); err != nil {
			processesOK = false
		}
	}

	// The bridge fds die with this process; any remaining holder of the
	// pool TAP is foreign (e.g. a VMM after a same-IP reuse).
	if st.TapName != "" {
		idle, err := TapIdle(defaultProcRoot, st.TapName)
		if err != nil || !idle {
			if err == nil && processesOK {
				// our processes are gone but the fd is held: not ours anymore
				if err := atomicWriteFile(sess.OwnershipLostPath(), []byte("tap fd held by foreign process\n"), 0600); err != nil {
					return fmt.Errorf("record lost TAP ownership; session retained: %w", err)
				}
				ownPort = false
			} else if err != nil {
				processesOK = false // scan unprovable
			}
		}
	}
	if _, err := os.Stat(sess.OwnershipLostPath()); err == nil {
		ownPort = false
	}
	// Verify ownership before touching shared TAP features. A slot may have
	// been detached and reused while this session was interrupted.
	if st.Port > 0 {
		if st.TapName == "" || st.Ifindex == 0 || st.InnerIP == "" || st.FloatingIP == "" {
			return fmt.Errorf("allocated port %d has incomplete identity; session retained", st.Port)
		}
		if ownPort && processesOK && !uncertain {
			sw, err := OpenSwitch(st.Switch)
			if err != nil {
				return fmt.Errorf("reopen switch: %w", err)
			}
			match, err := slotIdentityMatches(sw, st.Port, st.InnerIP, st.FloatingIP, st.Ifindex)
			_ = sw.Close()
			if err != nil {
				return fmt.Errorf("verify slot %d: %w", st.Port, err)
			}
			if !match {
				if err := atomicWriteFile(sess.OwnershipLostPath(), []byte("slot identity changed\n"), 0600); err != nil {
					return err
				}
				ownPort = false
				uncertain = true
			}
		}
	}

	// Offload restore only while the port verifiably belongs to this session.
	if ownPort && processesOK && !uncertain && st.OffloadDirty && st.TapName != "" {
		snap, err := LoadSnapshotFile(sess.OffloadPath())
		if err != nil {
			return fmt.Errorf("offload snapshot unavailable; session retained: %w", err)
		}
		if err := RestoreFeaturesInNS(st.SwitchNetns, st.TapName, snap); err != nil {
			return err
		}
	}

	// Namespace teardown (only the namespace we created).
	if nsValid && st.DebugNS != "" {
		if err := DeleteDebugNS(st.DebugNS); err != nil {
			return fmt.Errorf("delete netns %s: %w", st.DebugNS, err)
		}
	}
	if st.DebugNS != "" {
		if _, err := NetnsIno(st.DebugNS); os.IsNotExist(err) {
			_ = RemoveResolvConf(st.DebugNS)
		}
	}

	if uncertain || !processesOK {
		return fmt.Errorf("cleanup incomplete; port and session record retained: %s", dir)
	}

	// Conditional detach: re-verify slot identity right before releasing.
	if ownPort && st.Port > 0 {
		sw, err := OpenSwitch(st.Switch)
		if err != nil {
			return fmt.Errorf("reopen switch: %w", err)
		}
		match, err := slotIdentityMatches(sw, st.Port, st.InnerIP, st.FloatingIP, st.Ifindex)
		if err != nil {
			return fmt.Errorf("verify slot %d: %w", st.Port, err)
		}
		if !match {
			return fmt.Errorf("port %d changed ownership during cleanup; session retained", st.Port)
		}
		if st.TapName != "" {
			idle, err := TapIdle(defaultProcRoot, st.TapName)
			if err != nil || !idle {
				return fmt.Errorf("port %d: TAP not verifiably idle; session retained", st.Port)
			}
		}
		if err := sw.Detach(vswitch.DetachOptions{Port: st.Port, SkipDevice: true}); err != nil {
			return fmt.Errorf("detach port %d: %w", st.Port, err)
		}
	}
	if !ownPort {
		// ownership was lost: keep the record for forensics; rerunning
		// --cleanup-stale stays conservative via the sticky marker.
		return fmt.Errorf("port ownership lost; session record retained: %s", dir)
	}
	return sess.Remove()
}

// attachInFlight reports whether an attach worker still holds attach.lock.
func attachInFlight(sess *Session) bool {
	if _, err := os.Stat(sess.AttachLockPath()); err != nil {
		return false
	}
	fd, err := openLockFile(sess.AttachLockPath())
	if err != nil {
		return true // cannot probe: assume the worst
	}
	defer closeFD(fd)
	if err := tryFlock(fd); err != nil {
		return true
	}
	unlockFD(fd)
	return false
}

// slotIsFree checks the mmap slot state for a port number (1-based).
func slotIsFree(sw vswitch.Interface, port int) (bool, error) {
	idx := uint32(port - 1)
	cfg := sw.Config()
	if cfg == nil || idx >= cfg.N_ports {
		return false, fmt.Errorf("port %d out of range", port)
	}
	return sw.MmapSlots().GetInnerIP(idx) == vswitch.InnerIPFree, nil
}

// slotIdentityMatches re-reads the slot and compares every deterministic
// field of this session's allocation.
func slotIdentityMatches(sw vswitch.Interface, port int, innerIP, fip string, ifindex uint32) (bool, error) {
	idx := uint32(port - 1)
	cfg := sw.Config()
	if cfg == nil || idx >= cfg.N_ports {
		return false, fmt.Errorf("port %d out of range", port)
	}
	slots := sw.MmapSlots()
	curIP := slots.GetInnerIP(idx)
	if curIP == vswitch.InnerIPFree || curIP == vswitch.InnerIPReserved {
		return false, nil
	}
	if innerIP != "" {
		if want := vswitch.IPToUint32(net.ParseIP(innerIP)); want != 0 && curIP != want {
			return false, nil
		}
	}
	if fip != "" {
		wantFIP := vswitch.Uint32ToIP(cfg.FloatingIpBase + idx)
		if wantFIP.String() != fip {
			return false, nil
		}
	}
	if ifindex != 0 && slots.GetSlot(idx).Ifindex != ifindex {
		return false, nil
	}
	return true, nil
}
