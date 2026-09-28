package main

import (
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/kuasar-sandbox/connector/pkg/debug"
	"github.com/spf13/cobra"
)

// Cleanup runs in a killable copy of this binary. A wedged netlink operation
// must not hold session.lock forever or block the caller's recovery sweep.
func runDebugCleanup(root *debug.StateRoot, dir string, stale bool) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	args := []string{"__debug-cleanup", "--state-root", root.Path, "--dir", dir}
	if stale {
		args = append(args, "--stale")
	}
	worker := exec.Command(self, args...)
	worker.Stderr = os.Stderr
	if err := worker.Start(); err != nil {
		return fmt.Errorf("start cleanup worker: %w", err)
	}
	done := make(chan error, 1)
	go func() { done <- worker.Wait() }()
	select {
	case err := <-done:
		if err == nil {
			return nil
		}
		return fmt.Errorf("cleanup worker: %w", err)
	case <-time.After(time.Duration(debugCtlTimeout)*time.Second + 60*time.Second):
		_ = worker.Process.Kill()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
		return fmt.Errorf("cleanup timed out; session retained")
	}
}

var debugCleanupInternal = &cobra.Command{
	Use: "__debug-cleanup", Hidden: true, Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		rootPath, _ := cmd.Flags().GetString("state-root")
		dir, _ := cmd.Flags().GetString("dir")
		stale, _ := cmd.Flags().GetBool("stale")
		if rootPath == "" || dir == "" {
			return fmt.Errorf("missing cleanup state root or session dir")
		}
		root, err := debug.OpenStateRoot(rootPath)
		if err != nil {
			return err
		}
		return debug.CleanupSession(root, dir, debug.CleanupOptions{Stale: stale})
	},
}

func init() {
	debugCleanupInternal.Flags().String("state-root", "", "")
	debugCleanupInternal.Flags().String("dir", "", "")
	debugCleanupInternal.Flags().Bool("stale", false, "")
	rootCmd.AddCommand(debugCleanupInternal)
}
