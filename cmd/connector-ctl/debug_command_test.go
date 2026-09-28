package main

import (
	"errors"
	"os/exec"
	"testing"
	"time"

	"github.com/kuasar-sandbox/connector/pkg/debug"
)

type blockedDebugEndpoint struct{ release <-chan struct{} }

func (b blockedDebugEndpoint) ReadFrame([]byte) (int, error) {
	<-b.release
	return 0, errors.New("closed")
}
func (b blockedDebugEndpoint) WriteFrame([]byte) error { <-b.release; return errors.New("closed") }
func (b blockedDebugEndpoint) Close() error            { <-b.release; return nil }

func TestStopDebugBridgeIsBounded(t *testing.T) {
	release := make(chan struct{})
	br := debug.NewBridge(blockedDebugEndpoint{release}, blockedDebugEndpoint{release})
	br.Start()
	if err := stopDebugBridge(br, 20*time.Millisecond); err == nil {
		t.Fatal("blocked bridge unexpectedly stopped")
	}
	close(release)
	_ = br.Wait()
}

func TestStopDebugCommandEscalatesAndReturns(t *testing.T) {
	cmd := exec.Command("sh", "-c", "trap '' TERM; exec sleep 60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill() }()
	start, err := debug.ProcStarttimeOf(cmd.Process.Pid)
	if err != nil {
		_ = cmd.Process.Kill()
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	begin := time.Now()
	_, stopErr := stopDebugCommand(cmd.Process.Pid, start, waited)
	if stopErr != nil {
		t.Fatal(stopErr)
	}
	if time.Since(begin) > 5*time.Second {
		t.Fatal("command termination exceeded bound")
	}
}

func TestDebugCommandTimeoutDefault(t *testing.T) {
	if got := debugCmd.Flags().Lookup("command-timeout").DefValue; got != "120" {
		t.Fatalf("command-timeout default = %s, want 120", got)
	}
}
