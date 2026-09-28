package main

import (
	"fmt"
	"os"
	"time"

	"github.com/kuasar-sandbox/connector/pkg/debug"
	"golang.org/x/sys/unix"
)

func debugPendingSignal(signals <-chan os.Signal) os.Signal {
	select {
	case sig := <-signals:
		return sig
	default:
		return nil
	}
}

// stopDebugCommand bounds both escalation steps. A child stuck in an
// uninterruptible kernel wait is left to the recovery worker, which retains
// the session and allocated port if it cannot prove the child has exited.
func stopDebugCommand(pid int, start string, waited <-chan error) (error, error) {
	termErr := debug.SignalProcess("/proc", pid, start, unix.SIGTERM)
	select {
	case err := <-waited:
		return err, nil
	case <-time.After(1500 * time.Millisecond):
	}
	killErr := debug.SignalProcess("/proc", pid, start, unix.SIGKILL)
	select {
	case err := <-waited:
		return err, nil
	case <-time.After(2 * time.Second):
		return nil, fmt.Errorf("command did not exit after TERM/KILL (TERM: %v, KILL: %v)", termErr, killErr)
	}
}

func stopDebugBridge(br *debug.Bridge, timeout time.Duration) error {
	done := make(chan error, 1)
	go func() { done <- br.Stop() }()
	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		return fmt.Errorf("debug relay did not stop within %s", timeout)
	}
}
