package debug

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// procscan tests run against a fake /proc tree under t.TempDir().

const (
	statCommSpaces = "1234 (some comm (with parens)) R 1 0 0 0 -1 0 0 0 0 0 0 0 0 0 0 0 1 9999 100 0 0 20 0 0 0 0 0 0 0 0 0 0 0 0 0\n"
	statNormal     = "42 (bash) S 1 42 42 0 -1 0 0 0 0 0 0 0 0 0 0 0 1 555 200 0 0 20 0 0 0 0 0 0 0 0 0 0 0 0 0\n"
	statZombie     = "43 (sleep) Z 1 43 43 0 -1 0 0 0 0 0 0 0 0 0 0 0 1 777 300 0 0 20 0 0 0 0 0 0 0 0 0 0 0 0\n"
)

func TestProcStarttime(t *testing.T) {
	tests := []struct {
		name    string
		stat    string
		want    string
		wantErr bool
	}{
		{"normal", statNormal, "200", false},
		{"comm with spaces and parens", statCommSpaces, "100", false},
		{"zombie still has starttime", statZombie, "300", false},
		{"malformed no paren", "1 no parens here\n", "", true},
		{"short after paren", "1 (x)\n", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			writeFileTree(t, root, "42/stat", tt.stat)
			got, err := ProcStarttime(root, "42")
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("starttime = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestProcAliveZombieIsDead(t *testing.T) {
	root := t.TempDir()
	writeFileTree(t, root, "43/stat", statZombie)
	writeFileTree(t, root, "42/stat", statNormal)
	if ProcAlive(root, "43", "300") {
		t.Fatal("zombie must not count as alive (fd ownership is gone)")
	}
	if !ProcAlive(root, "42", "200") {
		t.Fatal("running process must be alive")
	}
	if ProcAlive(root, "42", "999") {
		t.Fatal("start time mismatch must not be alive")
	}
	if ProcAlive(root, "44", "1") {
		t.Fatal("missing pid must not be alive")
	}
}

func TestTapHoldersFindsThreadFD(t *testing.T) {
	root := t.TempDir()
	// pid 100 has two threads; the SECOND thread holds the tap fd — a
	// leader-only scan would miss it.
	writeFileTree(t, root, "100/stat", statNormal)
	writeFileTree(t, root, "100/task/100/stat", statNormal)
	writeFileTree(t, root, "100/task/101/stat", statNormal)
	writeFileTree(t, root, "100/task/100/fdinfo/3", "pos:\t0\nflags:\t02\n")
	writeFileTree(t, root, "100/task/101/fdinfo/7", "pos:\t0\nmnt_id:\t15\niff:\tpool-tap-9\n")
	// pid 200 holds a different tap
	writeFileTree(t, root, "200/stat", statNormal)
	writeFileTree(t, root, "200/task/200/stat", statNormal)
	writeFileTree(t, root, "200/task/200/fdinfo/3", "iff:\tother-tap\n")
	// pid 300 is our own pid shape but no taps
	writeFileTree(t, root, "300/stat", statNormal)
	writeFileTree(t, root, "300/task/300/fdinfo/4", "pos:\t0\n")

	holders, err := TapHolders(root, "pool-tap-9")
	if err != nil {
		t.Fatalf("TapHolders: %v", err)
	}
	if len(holders) != 1 || holders[0].PID != "100" {
		t.Fatalf("holders = %+v, want exactly pid 100", holders)
	}

	idle, err := TapIdle(root, "other-tap")
	if err != nil || idle {
		t.Fatalf("TapIdle(other-tap) = %v, %v; want false, nil", idle, err)
	}
	idle, err = TapIdle(root, "unknown-tap")
	if err != nil || !idle {
		t.Fatalf("TapIdle(unknown-tap) = %v, %v; want true, nil", idle, err)
	}
}

func TestTapHoldersSubstringNotMatched(t *testing.T) {
	root := t.TempDir()
	writeFileTree(t, root, "1/stat", statNormal)
	writeFileTree(t, root, "1/task/1/stat", statNormal)
	// "iff:\tpool-tap" must not match a holder of "pool-tap-9" (fixed-line match)
	writeFileTree(t, root, "1/task/1/fdinfo/3", "iff:\tpool-tap\n")
	holders, err := TapHolders(root, "pool-tap-9")
	if err != nil {
		t.Fatalf("TapHolders: %v", err)
	}
	if len(holders) != 0 {
		t.Fatalf("substring must not match: %+v", holders)
	}
}

// TestTapHoldersRoot is the live check on a real throwaway TAP.
func TestTapHoldersRoot(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	ns := createScratchNS(t, "scan")
	tap := createScratchTap(t, ns, "scan0")

	holders, err := TapHolders(defaultProcRoot, tap)
	if err != nil || len(holders) == 0 {
		t.Fatalf("expected holder, got %+v, %v", holders, err)
	}
	// Kill the holder; after exit the TAP must be idle.
	if p := holders[0]; p.PID != "" {
		if err := SignalProcess(defaultProcRoot, atoi(p.PID), p.Start+"-stale", unix.SIGKILL); err != nil {
			t.Fatalf("stale identity signal: %v", err)
		}
		if !ProcAlive(defaultProcRoot, p.PID, p.Start) {
			t.Fatal("stale identity killed the TAP holder")
		}
		if err := SignalProcess(defaultProcRoot, atoi(p.PID), p.Start, unix.SIGTERM); err != nil {
			t.Fatalf("signal holder: %v", err)
		}
		stop := StopProcess(defaultProcRoot, p.PID, p.Start, 500e6, 500e6)
		if stop != nil {
			t.Fatalf("stop: %v", stop)
		}
	}
	idle, err := TapIdle(defaultProcRoot, tap)
	if err != nil || !idle {
		t.Fatalf("TapIdle after holder death = %v, %v", idle, err)
	}
	_ = filepath.Join // keep import if assertions above change
}
