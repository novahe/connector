package debug

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/kuasar-sandbox/connector/pkg/netns"
	"github.com/kuasar-sandbox/connector/pkg/tapfd"
)

// Root/integration test helpers shared by the *_test.go files.

// rootGate skips the test when not running as root.
func rootGate(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
}

// tapIdleSelfCheck is a cheap root probe for root-gated tests.
func tapIdleSelfCheck() (bool, error) {
	if os.Geteuid() != 0 {
		return false, fmt.Errorf("not root")
	}
	return true, nil
}

// createScratchNS makes a throwaway netns and deletes it on test cleanup.
func createScratchNS(t *testing.T, name string) string {
	t.Helper()
	rootGate(t)
	fq := fmt.Sprintf("dtut-%s-%d", name, os.Getpid())
	if _, err := CreateDebugNS(fq); err != nil {
		t.Fatalf("create ns: %v", err)
	}
	t.Cleanup(func() { _ = DeleteDebugNS(fq) })
	return fq
}

// createScratchTap opens a non-persistent TAP inside the named netns and
// returns its name; the device lives as long as the returned process keeps
// the fd (this helper parks the fd in a background process).
func createScratchTap(t *testing.T, ns, name string) string {
	t.Helper()
	rootGate(t)
	pid, err := forkTapHolder(ns, name)
	if err != nil {
		t.Fatalf("fork tap holder: %v", err)
	}
	t.Cleanup(func() {
		_ = unix.Kill(pid, unix.SIGKILL)
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if !ProcAlive(defaultProcRoot, fmt.Sprint(pid), "") {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
	})
	return name
}

// forkTapHolder reexecs the test binary as a helper that enters ns and holds
// a TAP fd open until killed (see TestHelperTapHolder).
func forkTapHolder(ns, tap string) (int, error) {
	attr := &os.ProcAttr{
		Env:   append(os.Environ(), "DTUT_TAP_HOLDER=1", "DTUT_NS="+ns, "DTUT_TAP="+tap),
		Files: []*os.File{os.Stdin, os.Stdout, os.Stderr},
	}
	proc, err := os.StartProcess(os.Args[0], []string{os.Args[0], "-test.run=TestHelperTapHolder", "--"}, attr)
	if err != nil {
		return 0, err
	}
	go func() { _, _ = proc.Wait() }()
	// wait for the device to appear
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := LinkIfindexInNS(ns, tap); err == nil {
			return proc.Pid, nil
		}
		time.Sleep(25 * time.Millisecond)
	}
	return 0, fmt.Errorf("tap %s did not appear in %s", tap, ns)
}

// TestHelperTapHolder is not a real test: it is the reexec target used by
// forkTapHolder (guard via env var).
func TestHelperTapHolder(t *testing.T) {
	if os.Getenv("DTUT_TAP_HOLDER") != "1" {
		t.Skip("helper target only")
	}
	ns := os.Getenv("DTUT_NS")
	tap := os.Getenv("DTUT_TAP")
	nsObj, err := netns.GetByName(ns)
	if err != nil {
		t.Fatal(err)
	}
	defer nsObj.Close()
	if err := nsObj.Do(func() error {
		f, err := tapfd.OpenTap(tap, unix.IFF_NO_PI)
		if err != nil {
			return err
		}
		_ = f
		// park until killed; the fd stays open in this process
		time.Sleep(24 * time.Hour)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// writeFileTree creates parent dirs then a file with the given content.
func writeFileTree(t *testing.T, base, rel, content string) {
	t.Helper()
	p := filepath.Join(base, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}
